package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"

	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
)

// MicroVMAPI is the part of the Lambda MicroVMs API that the backend uses.
type MicroVMAPI interface {
	RunMicrovm(ctx context.Context, in *lambdamicrovms.RunMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error)
	GetMicrovm(ctx context.Context, in *lambdamicrovms.GetMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	ResumeMicrovm(ctx context.Context, in *lambdamicrovms.ResumeMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error)
	TerminateMicrovm(ctx context.Context, in *lambdamicrovms.TerminateMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
	CreateMicrovmAuthToken(ctx context.Context, in *lambdamicrovms.CreateMicrovmAuthTokenInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
}

// EFSAPI is the part of the EFS API that the backend uses.
type EFSAPI interface {
	CreateAccessPoint(ctx context.Context, in *efs.CreateAccessPointInput, opts ...func(*efs.Options)) (*efs.CreateAccessPointOutput, error)
	DescribeAccessPoints(ctx context.Context, in *efs.DescribeAccessPointsInput, opts ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error)
	DeleteAccessPoint(ctx context.Context, in *efs.DeleteAccessPointInput, opts ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error)
}

// IAMAPI is the part of the IAM API that the backend uses.
type IAMAPI interface {
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, opts ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, opts ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, opts ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, opts ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

// MicroVM runs containers as AWS Lambda MicroVMs.
//
// A MicroVM lives at most eight hours and loses its disk when it ends, so a
// container is a series of MicroVMs. What survives is the home directory: it
// lives on an EFS access point that belongs to the container, and every
// MicroVM of the container mounts it. Each container also has its own IAM
// execution role, which may mount only that access point, so that the root
// user of one container cannot mount the home directory of another.
//
// The agent inside the VM reports a termination. When the VM went away
// without the daemon stopping it, OnTerminated is called, which starts the
// next MicroVM.
type MicroVM struct {
	cfg    *config.MicroVMConfig
	log    *slog.Logger
	state  *state.State
	region string
	vms    MicroVMAPI
	efs    EFSAPI
	iam    IAMAPI

	// OnTerminated is called, in its own goroutine, when the MicroVM of a
	// container terminated without the daemon stopping it.
	OnTerminated func(name string)

	// ctx bounds the watchers of the VMs.
	ctx context.Context
	// poll is the interval of the loops that wait for a change of state.
	poll time.Duration
	// home is where the home directory is mounted inside the VM.
	home string
	// unmountedHome skips the mount check of Remove (tests).
	unmountedHome bool

	mu sync.Mutex
	// tokens caches the endpoint tokens by MicroVM.
	tokens map[string]authToken
	// stopping names the MicroVM that the daemon is stopping, by container.
	stopping map[string]string
	// watching names the MicroVM whose events are watched, by container.
	watching map[string]string
	// runningUntil caches a RUNNING answer by MicroVM.
	runningUntil map[string]time.Time
	// policies holds the policy last written to each execution role.
	policies map[string]string

	// shareMu serialises the lookup of the access points of the shared
	// directories, so that two containers never create one twice.
	shareMu sync.Mutex
	// shareAPs caches the access points of the shared directories by path.
	shareAPs map[string]shareAP
}

type authToken struct {
	header  http.Header
	expires time.Time
}

var _ Backend = (*MicroVM)(nil)

// NewMicroVM creates the backend. ctx bounds the background work.
func NewMicroVM(ctx context.Context, cfg *config.MicroVMConfig, log *slog.Logger, st *state.State, aws aws.Config) *MicroVM {
	return NewMicroVMWith(ctx, cfg, log, st, MicroVMClients{
		Region: aws.Region,
		VMs:    lambdamicrovms.NewFromConfig(aws),
		EFS:    efs.NewFromConfig(aws),
		IAM:    iam.NewFromConfig(aws),
	})
}

// MicroVMClients are the AWS API clients of the backend.
type MicroVMClients struct {
	Region string
	VMs    MicroVMAPI
	EFS    EFSAPI
	IAM    IAMAPI
	// Poll is the interval of the loops that wait for a change of state.
	// Zero selects the default; tests use a short one.
	Poll time.Duration
	// Home is where the home directory is mounted inside the VM. It
	// defaults to /root, the home of the user that logs in.
	Home string
	// UnmountedHome lets Remove delete a home directory that is not a mount
	// point. Only tests, whose agents run on the host, set it.
	UnmountedHome bool
}

// NewMicroVMWith creates the backend with the given API clients.
func NewMicroVMWith(ctx context.Context, cfg *config.MicroVMConfig, log *slog.Logger, st *state.State, c MicroVMClients) *MicroVM {
	if c.Poll <= 0 {
		c.Poll = 2 * time.Second
	}
	if c.Home == "" {
		c.Home = "/root"
	}
	return &MicroVM{
		cfg: cfg, log: log, state: st, region: c.Region,
		vms: c.VMs, efs: c.EFS, iam: c.IAM,
		ctx:           ctx,
		poll:          c.Poll,
		home:          c.Home,
		unmountedHome: c.UnmountedHome,
		tokens:        map[string]authToken{},
		stopping:      map[string]string{},
		watching:      map[string]string{},
		runningUntil:  map[string]time.Time{},
		policies:      map[string]string{},
	}
}

// Image resolves an image name to a MicroVM image.
func (b *MicroVM) Image(name string) (config.MicroVMImage, error) {
	if name == "" {
		name = b.cfg.DefaultImage
	}
	img, ok := b.cfg.Images[name]
	if !ok {
		return config.MicroVMImage{}, fmt.Errorf("unknown MicroVM image %q: use one of %s", name, strings.Join(b.cfg.ImageNames(), ", "))
	}
	return img, nil
}

// NewInstanceID returns a fresh identifier for the AWS resources of a
// container.
func NewInstanceID() string { return randomHex(16) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Provision creates the AWS resources of a container: the access point of its
// home directory and its execution role. Resources that already exist are
// kept, so that a failed attempt can be repeated.
func (b *MicroVM) Provision(ctx context.Context, vm *state.VM) error {
	mv, ok := b.state.MicroVMOf(vm.Name)
	if !ok {
		return fmt.Errorf("container %s is not a MicroVM", vm.Name)
	}
	tags := map[string]string{"sandboxxing:instance": mv.InstanceID, "sandboxxing:name": vm.Name}
	if mv.AccessPointID == "" {
		progress.From(ctx).Step("creating the home directory of %s", vm.Name)
		out, err := b.efs.CreateAccessPoint(ctx, &efs.CreateAccessPointInput{
			ClientToken:  aws.String(mv.InstanceID),
			FileSystemId: aws.String(b.cfg.FileSystemID),
			// Every request through the access point is made as root, the
			// user that logs into the container, and is confined to the
			// directory of this container.
			PosixUser: &efstypes.PosixUser{Uid: aws.Int64(0), Gid: aws.Int64(0)},
			RootDirectory: &efstypes.RootDirectory{
				Path: aws.String(b.cfg.HomeRoot + "/" + mv.InstanceID),
				CreationInfo: &efstypes.CreationInfo{
					OwnerUid: aws.Int64(0), OwnerGid: aws.Int64(0), Permissions: aws.String("0700"),
				},
			},
			Tags: efsTags(tags),
		})
		if err != nil {
			return fmt.Errorf("create the access point of %s: %w", vm.Name, err)
		}
		if err := b.state.UpdateMicroVM(vm.Name, func(m *state.MicroVM) error {
			m.AccessPointID = aws.ToString(out.AccessPointId)
			m.AccessPointARN = aws.ToString(out.AccessPointArn)
			return nil
		}); err != nil {
			return err
		}
		if err := b.waitAccessPoint(ctx, aws.ToString(out.AccessPointId)); err != nil {
			return err
		}
		mv, _ = b.state.MicroVMOf(vm.Name)
	}
	if mv.RoleARN == "" {
		progress.From(ctx).Step("creating the execution role of %s", vm.Name)
		name := "sandboxxing-" + mv.InstanceID
		out, err := b.iam.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(name),
			Path:                     aws.String(b.cfg.RolePath),
			AssumeRolePolicyDocument: aws.String(trustPolicy),
			PermissionsBoundary:      aws.String(b.cfg.PermissionsBoundary),
			Description:              aws.String("sandboxxing container " + vm.Name),
			Tags:                     iamTags(tags),
		})
		if err != nil {
			return fmt.Errorf("create the execution role of %s: %w", vm.Name, err)
		}
		if err := b.state.UpdateMicroVM(vm.Name, func(m *state.MicroVM) error {
			m.RoleName = name
			m.RoleARN = aws.ToString(out.Role.Arn)
			return nil
		}); err != nil {
			return err
		}
		mv, _ = b.state.MicroVMOf(vm.Name)
	}
	// The shares are granted with the home directory, so that the policy
	// has propagated by the time the first MicroVM mounts them.
	_, err := b.grant(ctx, vm, mv)
	return err
}

// waitAccessPoint waits until a new access point can be mounted.
func (b *MicroVM) waitAccessPoint(ctx context.Context, id string) error {
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := b.efs.DescribeAccessPoints(ctx, &efs.DescribeAccessPointsInput{AccessPointId: aws.String(id)})
		if err != nil {
			return fmt.Errorf("describe access point %s: %w", id, err)
		}
		if len(out.AccessPoints) == 1 {
			switch out.AccessPoints[0].LifeCycleState {
			case efstypes.LifeCycleStateAvailable:
				return nil
			case efstypes.LifeCycleStateError, efstypes.LifeCycleStateDeleting, efstypes.LifeCycleStateDeleted:
				return fmt.Errorf("access point %s is %s", id, out.AccessPoints[0].LifeCycleState)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("access point %s did not become available", id)
		}
		if err := sleep(ctx, b.poll); err != nil {
			return err
		}
	}
}

// trustPolicy lets Lambda assume an execution role.
const trustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Principal":{"Service":"lambda.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`

const rolePolicyName = "sandboxxing-home"

// rolePolicy is the only permission of an execution role: mounting the file
// system through the access point of its container and through those of its
// shared directories, and writing the log stream of its container. The
// condition is what keeps one container out of the home directories of the
// others.
func (b *MicroVM) rolePolicy(mv state.MicroVM, shares []shareGrant) (string, error) {
	partition, account, err := arnAccount(mv.AccessPointARN)
	if err != nil {
		return "", err
	}
	fileSystem := fmt.Sprintf("arn:%s:elasticfilesystem:%s:%s:file-system/%s", partition, b.region, account, b.cfg.FileSystemID)
	statements := []map[string]any{{
		"Effect": "Allow",
		"Action": []string{
			"elasticfilesystem:ClientMount",
			"elasticfilesystem:ClientWrite",
			"elasticfilesystem:ClientRootAccess",
		},
		"Resource": fileSystem,
		"Condition": map[string]any{
			"StringEquals": map[string]string{"elasticfilesystem:AccessPointArn": mv.AccessPointARN},
		},
	}}
	statements = append(statements, shareStatements(fileSystem, shares)...)
	if b.cfg.LogGroup != "" {
		group := fmt.Sprintf("arn:%s:logs:%s:%s:log-group:%s", partition, b.region, account, b.cfg.LogGroup)
		statements = append(statements,
			map[string]any{"Effect": "Allow", "Action": "logs:CreateLogStream", "Resource": group + ":*"},
			map[string]any{"Effect": "Allow", "Action": "logs:PutLogEvents", "Resource": group + ":log-stream:" + mv.InstanceID},
		)
	}
	doc, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": statements})
	return string(doc), err
}

// arnAccount returns the partition and the account of an ARN.
func arnAccount(arn string) (partition, account string, err error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[1] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("invalid ARN %q", arn)
	}
	return parts[1], parts[4], nil
}

// Remove deletes a container: the files of its home directory, its MicroVM,
// its access point and its execution role. The files can only be reached
// from inside a MicroVM of the container, so one is started when needed.
func (b *MicroVM) Remove(ctx context.Context, vm *state.VM) error {
	mv, ok := b.state.MicroVMOf(vm.Name)
	if !ok {
		return nil
	}
	if mv.AccessPointID != "" && mv.RoleARN != "" {
		progress.From(ctx).Step("deleting the home directory of %s", vm.Name)
		if err := b.mountedHome(ctx, vm); err != nil {
			return err
		}
		code, err := b.Run(ctx, vm, Exec{Argv: []string{"find", b.home, "-xdev", "-mindepth", "1", "-delete"}})
		if err != nil {
			return fmt.Errorf("delete the home directory of %s: %w", vm.Name, err)
		}
		if code != 0 {
			return fmt.Errorf("delete the home directory of %s: find exited with status %d", vm.Name, code)
		}
	}
	if err := b.Stop(ctx, vm); err != nil {
		return err
	}
	if mv.AccessPointID != "" {
		_, err := b.efs.DeleteAccessPoint(ctx, &efs.DeleteAccessPointInput{AccessPointId: aws.String(mv.AccessPointID)})
		var notFound *efstypes.AccessPointNotFound
		if err != nil && !errors.As(err, &notFound) {
			return fmt.Errorf("delete the access point of %s: %w", vm.Name, err)
		}
	}
	if mv.RoleName != "" {
		var noSuch *iamtypes.NoSuchEntityException
		_, err := b.iam.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName: aws.String(mv.RoleName), PolicyName: aws.String(rolePolicyName),
		})
		if err != nil && !errors.As(err, &noSuch) {
			return fmt.Errorf("delete the execution role of %s: %w", vm.Name, err)
		}
		_, err = b.iam.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(mv.RoleName)})
		if err != nil && !errors.As(err, &noSuch) {
			return fmt.Errorf("delete the execution role of %s: %w", vm.Name, err)
		}
		b.mu.Lock()
		delete(b.policies, mv.RoleName)
		b.mu.Unlock()
	}
	return nil
}

// mountedHome starts the container with its home directory mounted. The
// files are deleted from inside the VM, and an unmounted home directory would
// make that delete the files of the VM instead and leave the ones on EFS. A
// MicroVM whose home directory went away is replaced by a new one.
func (b *MicroVM) mountedHome(ctx context.Context, vm *state.VM) error {
	for attempt := 0; ; attempt++ {
		if err := b.Start(ctx, vm); err != nil {
			return fmt.Errorf("start %s to delete its home directory: %w", vm.Name, err)
		}
		if b.unmountedHome {
			return nil
		}
		code, err := b.Run(ctx, vm, Exec{Argv: []string{"mountpoint", "-q", b.home}})
		if err == nil && code == 0 {
			return nil
		}
		if attempt == 1 {
			return fmt.Errorf("the home directory of %s is not mounted, so it cannot be deleted", vm.Name)
		}
		b.log.Warn("the home directory of a container is not mounted, starting a new MicroVM", "name", vm.Name)
		if err := b.Stop(ctx, vm); err != nil {
			return err
		}
	}
}

// Start makes sure that the container has a running MicroVM: a suspended one
// is resumed and a terminated one replaced by a new one.
func (b *MicroVM) Start(ctx context.Context, vm *state.VM) error {
	mv, ok := b.state.MicroVMOf(vm.Name)
	if !ok {
		return fmt.Errorf("container %s is not a MicroVM", vm.Name)
	}
	if mv.ID != "" {
		out, err := b.get(ctx, mv.ID)
		switch {
		case isNotFound(err):
		case err != nil:
			return err
		default:
			switch out.State {
			case mvtypes.MicrovmStatePending, mvtypes.MicrovmStateRunning:
				return b.waitReady(ctx, vm.Name, mv)
			case mvtypes.MicrovmStateSuspending, mvtypes.MicrovmStateSuspended:
				progress.From(ctx).Step("resuming the MicroVM of %s", vm.Name)
				if _, err := b.vms.ResumeMicrovm(ctx, &lambdamicrovms.ResumeMicrovmInput{MicrovmIdentifier: aws.String(mv.ID)}); err != nil {
					var conflict *mvtypes.ConflictException
					// A VM that is still suspending cannot be resumed
					// yet; waitReady retries until it answers.
					if !errors.As(err, &conflict) {
						return fmt.Errorf("resume the MicroVM of %s: %w", vm.Name, err)
					}
				}
				return b.waitReady(ctx, vm.Name, mv)
			}
			// A terminating or terminated VM is replaced.
		}
	}
	return b.runNewRetrying(ctx, vm, mv)
}

// errStartTerminated reports a MicroVM that terminated before it answered.
var errStartTerminated = errors.New("terminated while starting")

// runNewRetrying starts the next MicroVM. The execution role of a new
// container may not have propagated through IAM when the first MicroVM
// starts: Lambda accepts the request and the VM then terminates because the
// role cannot be assumed. The first start is therefore tried again a few times.
func (b *MicroVM) runNewRetrying(ctx context.Context, vm *state.VM, mv state.MicroVM) error {
	first := mv.Generation == 0
	for attempt := 1; ; attempt++ {
		err := b.runNew(ctx, vm, mv)
		if err == nil || !first || attempt == 6 || !errors.Is(err, errStartTerminated) {
			return err
		}
		b.log.Info("the first MicroVM of a container did not start, retrying", "name", vm.Name, "err", err)
		if err := sleep(ctx, 5*b.poll); err != nil {
			return err
		}
		mv, _ = b.state.MicroVMOf(vm.Name)
	}
}

// maxRunHookPayload is the largest run hook payload Lambda accepts.
const maxRunHookPayload = 4096

// runNew starts the next MicroVM of a container.
func (b *MicroVM) runNew(ctx context.Context, vm *state.VM, mv state.MicroVM) error {
	if mv.AccessPointID == "" || mv.RoleARN == "" {
		return fmt.Errorf("the AWS resources of %s are incomplete: remove the container and create it again", vm.Name)
	}
	// A share added to the configuration since the last start is granted
	// now.
	shares, err := b.grant(ctx, vm, mv)
	if err != nil {
		return err
	}
	secret := randomHex(32)
	generation := mv.Generation + 1
	payload, err := json.Marshal(agent.RunConfig{
		Name:            vm.Name,
		Secret:          secret,
		Env:             envList(vm.Env),
		FileSystemID:    b.cfg.FileSystemID,
		AccessPointID:   mv.AccessPointID,
		Home:            b.home,
		Shares:          shares,
		MaximumDuration: b.cfg.MaximumDuration.Duration(),
	})
	if err != nil {
		return err
	}
	if len(payload) > maxRunHookPayload {
		return fmt.Errorf("the environment of %s is too large for a MicroVM: at most %d bytes of configuration can be passed", vm.Name, maxRunHookPayload)
	}
	partition, _, err := arnAccount(mv.AccessPointARN)
	if err != nil {
		return err
	}
	in := &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier: aws.String(mv.ImageARN),
		// The token makes the retries below idempotent. It differs for every
		// start, because the payload does.
		ClientToken:      aws.String(fmt.Sprintf("%s-%d-%s", mv.InstanceID, generation, secret[:8])),
		ExecutionRoleArn: aws.String(mv.RoleARN),
		IngressNetworkConnectors: []string{
			fmt.Sprintf("arn:%s:lambda:%s:aws:network-connector:aws-network-connector:ALL_INGRESS", partition, b.region),
		},
		EgressNetworkConnectors:  b.cfg.EgressNetworkConnectors,
		MaximumDurationInSeconds: aws.Int32(int32(b.cfg.MaximumDuration.Duration() / time.Second)),
		RunHookPayload:           aws.String(string(payload)),
	}
	if mv.ImageVersion != "" {
		in.ImageVersion = aws.String(mv.ImageVersion)
	}
	if b.cfg.LogGroup != "" {
		in.Logging = &mvtypes.LoggingMemberCloudWatch{Value: mvtypes.CloudWatchLogging{
			LogGroup: aws.String(b.cfg.LogGroup), LogStream: aws.String(mv.InstanceID),
		}}
	} else {
		in.Logging = &mvtypes.LoggingMemberDisabled{}
	}
	if p := b.cfg.IdlePolicy; p != nil {
		in.IdlePolicy = &mvtypes.IdlePolicy{
			AutoResumeEnabled:        aws.Bool(p.AutoResume),
			MaxIdleDurationSeconds:   aws.Int32(int32(p.MaxIdle.Duration() / time.Second)),
			SuspendedDurationSeconds: aws.Int32(int32(p.SuspendedDuration.Duration() / time.Second)),
		}
	}

	progress.From(ctx).Step("starting a MicroVM for %s", vm.Name)
	out, err := b.run(ctx, in, generation == 1)
	if err != nil {
		return fmt.Errorf("start a MicroVM for %s: %w", vm.Name, err)
	}
	startedAt := time.Now()
	if out.StartedAt != nil {
		startedAt = *out.StartedAt
	}
	if err := b.state.UpdateMicroVM(vm.Name, func(m *state.MicroVM) error {
		m.ID = aws.ToString(out.MicrovmId)
		m.Endpoint = aws.ToString(out.Endpoint)
		m.StartedAt = startedAt
		m.Generation = generation
		m.Secret = secret
		return nil
	}); err != nil {
		return err
	}
	b.log.Info("started a MicroVM", "name", vm.Name, "microvm", aws.ToString(out.MicrovmId), "generation", generation)
	mv, _ = b.state.MicroVMOf(vm.Name)
	return b.waitReady(ctx, vm.Name, mv)
}

// run calls RunMicrovm. The execution role of a new container may not have
// propagated through IAM yet, which Lambda reports as an invalid or denied
// request, so the first start of a container retries those for a while.
func (b *MicroVM) run(ctx context.Context, in *lambdamicrovms.RunMicrovmInput, newRole bool) (*lambdamicrovms.RunMicrovmOutput, error) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := b.vms.RunMicrovm(ctx, in)
		if err == nil {
			return out, nil
		}
		var invalid *mvtypes.ValidationException
		var denied *mvtypes.AccessDeniedException
		if !newRole || time.Now().After(deadline) || !(errors.As(err, &invalid) || errors.As(err, &denied)) {
			return nil, err
		}
		b.log.Debug("the execution role is not usable yet, retrying", "err", err)
		if err := sleep(ctx, 2*b.poll); err != nil {
			return nil, err
		}
	}
}

// waitReady waits until the agent of the current MicroVM answers, which it
// does once the run hook mounted the home directory, and watches the VM.
func (b *MicroVM) waitReady(ctx context.Context, name string, mv state.MicroVM) error {
	deadline := time.Now().Add(b.cfg.StartTimeout.Duration())
	client := b.clientFor(mv)
	for {
		// The state is eventually consistent, so readiness is decided by the
		// agent; the state only tells a VM that failed to start.
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := client.Ping(pingCtx)
		cancel()
		if err == nil {
			b.markRunning(mv.ID)
			b.watch(name, mv.ID)
			return nil
		}
		out, gerr := b.get(ctx, mv.ID)
		if gerr == nil && (out.State == mvtypes.MicrovmStateTerminating || out.State == mvtypes.MicrovmStateTerminated) {
			return fmt.Errorf("the MicroVM of %s %w: %s", name, errStartTerminated, aws.ToString(out.StateReason))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the MicroVM of %s did not answer within %s: %w", name, b.cfg.StartTimeout.Duration(), err)
		}
		if err := sleep(ctx, b.poll/2); err != nil {
			return err
		}
	}
}

// Stop terminates the current MicroVM of a container. The home directory is
// kept; the next start runs a new MicroVM.
func (b *MicroVM) Stop(ctx context.Context, vm *state.VM) error {
	mv, ok := b.state.MicroVMOf(vm.Name)
	if !ok || mv.ID == "" {
		return nil
	}
	b.mu.Lock()
	b.stopping[vm.Name] = mv.ID
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.stopping[vm.Name] == mv.ID {
			delete(b.stopping, vm.Name)
		}
		b.mu.Unlock()
	}()

	progress.From(ctx).Step("terminating the MicroVM of %s", vm.Name)
	_, err := b.vms.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(mv.ID)})
	var conflict *mvtypes.ConflictException
	if err != nil && !isNotFound(err) && !errors.As(err, &conflict) {
		return fmt.Errorf("terminate the MicroVM of %s: %w", vm.Name, err)
	}
	// The terminate hook of the agent flushes and unmounts the home
	// directory; Lambda gives it a minute.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := b.get(ctx, mv.ID)
		if isNotFound(err) || (err == nil && out.State == mvtypes.MicrovmStateTerminated) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the MicroVM of %s did not terminate in time", vm.Name)
		}
		if err := sleep(ctx, b.poll); err != nil {
			return err
		}
	}
	b.forget(mv.ID)
	return b.state.UpdateMicroVM(vm.Name, func(m *state.MicroVM) error {
		if m.ID == mv.ID {
			clearCurrent(m)
		}
		return nil
	})
}

func clearCurrent(m *state.MicroVM) {
	m.ID, m.Endpoint, m.Secret = "", "", ""
	m.StartedAt = time.Time{}
}

// runningCache is how long a RUNNING answer is trusted. Every SSH request
// asks whether the container runs.
const runningCache = 15 * time.Second

// Running reports whether the current MicroVM runs.
func (b *MicroVM) Running(ctx context.Context, vm *state.VM) bool {
	mv, ok := b.state.MicroVMOf(vm.Name)
	if !ok || mv.ID == "" {
		return false
	}
	b.mu.Lock()
	until, cached := b.runningUntil[mv.ID]
	b.mu.Unlock()
	if cached && time.Now().Before(until) {
		return true
	}
	out, err := b.get(ctx, mv.ID)
	if err != nil || out.State != mvtypes.MicrovmStateRunning {
		return false
	}
	b.markRunning(mv.ID)
	return true
}

func (b *MicroVM) markRunning(id string) {
	b.mu.Lock()
	b.runningUntil[id] = time.Now().Add(runningCache)
	b.mu.Unlock()
}

// forget drops what is cached about a MicroVM.
func (b *MicroVM) forget(id string) {
	b.mu.Lock()
	delete(b.runningUntil, id)
	delete(b.tokens, id)
	b.mu.Unlock()
}

// StatusOf reports the current MicroVM.
func (b *MicroVM) StatusOf(ctx context.Context, vm *state.VM) Status {
	mv, _ := b.state.MicroVMOf(vm.Name)
	st := Status{Unit: mv.ID}
	if b.Running(ctx, vm) {
		st.Running = true
		st.Since = mv.StartedAt
	}
	return st
}

// Collect is not available: Lambda does not report the usage of a MicroVM.
func (b *MicroVM) Collect(ctx context.Context, vm *state.VM) (Metrics, error) {
	return Metrics{}, errors.New("resource usage is not available for MicroVMs")
}

// Run executes a command in the current MicroVM.
func (b *MicroVM) Run(ctx context.Context, vm *state.VM, e Exec) (int, error) {
	sess, err := b.start(ctx, vm, e, false, 0, 0)
	if err != nil {
		return 1, err
	}
	return sess.Wait()
}

// StartProcess starts a command in the current MicroVM.
func (b *MicroVM) StartProcess(ctx context.Context, vm *state.VM, e Exec) (Process, error) {
	return b.start(ctx, vm, e, false, 0, 0)
}

// StartTerminal starts a command on a terminal of the current MicroVM.
func (b *MicroVM) StartTerminal(ctx context.Context, vm *state.VM, e Exec, rows, cols int) (Terminal, error) {
	return b.start(ctx, vm, e, true, rows, cols)
}

func (b *MicroVM) start(ctx context.Context, vm *state.VM, e Exec, tty bool, rows, cols int) (*agent.Session, error) {
	client, err := b.client(vm.Name)
	if err != nil {
		return nil, err
	}
	req := agent.ExecRequest{Argv: e.Argv, Env: e.Env, Dir: e.Dir, TTY: tty, Rows: rows, Cols: cols}
	if tty {
		return client.Start(ctx, req, nil, nil, nil)
	}
	return client.Start(ctx, req, e.Stdin, e.Stdout, e.Stderr)
}

// Dial connects to a port of the current MicroVM.
func (b *MicroVM) Dial(ctx context.Context, vm *state.VM, port int) (net.Conn, error) {
	client, err := b.client(vm.Name)
	if err != nil {
		return nil, err
	}
	return client.Dial(ctx, port)
}

// client returns the agent client of the current MicroVM of a container.
func (b *MicroVM) client(name string) (*agent.Client, error) {
	mv, ok := b.state.MicroVMOf(name)
	if !ok || mv.ID == "" {
		return nil, fmt.Errorf("container %s has no running MicroVM", name)
	}
	return b.clientFor(mv), nil
}

func (b *MicroVM) clientFor(mv state.MicroVM) *agent.Client {
	base := mv.Endpoint
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	return &agent.Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		Header: func(ctx context.Context) (http.Header, error) {
			h, err := b.token(ctx, mv.ID)
			if err != nil {
				return nil, err
			}
			h.Set(agent.SecretHeader, mv.Secret)
			return h, nil
		},
	}
}

// tokenRenewal is how long before its expiry a token is replaced, so that a
// connection never starts with a token that runs out during the handshake.
const tokenRenewal = 5 * time.Minute

// token returns the headers that authenticate at the endpoint of a MicroVM.
// The token only opens the data port of the agent.
func (b *MicroVM) token(ctx context.Context, id string) (http.Header, error) {
	b.mu.Lock()
	t, ok := b.tokens[id]
	b.mu.Unlock()
	if ok && time.Until(t.expires) > tokenRenewal {
		return t.header.Clone(), nil
	}
	ttl := b.cfg.TokenTTL.Duration()
	out, err := b.vms.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   aws.String(id),
		ExpirationInMinutes: aws.Int32(int32(ttl / time.Minute)),
		AllowedPorts:        []mvtypes.PortSpecification{&mvtypes.PortSpecificationMemberPort{Value: agent.DataPort}},
	})
	if err != nil {
		return nil, fmt.Errorf("create a token for MicroVM %s: %w", id, err)
	}
	h := http.Header{}
	for k, v := range out.AuthToken {
		h.Set(k, v)
	}
	h.Set("X-aws-proxy-port", strconv.Itoa(agent.DataPort))
	b.mu.Lock()
	b.tokens[id] = authToken{header: h, expires: time.Now().Add(ttl)}
	b.mu.Unlock()
	return h.Clone(), nil
}

func (b *MicroVM) get(ctx context.Context, id string) (*lambdamicrovms.GetMicrovmOutput, error) {
	return b.vms.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(id)})
}

func isNotFound(err error) bool {
	var notFound *mvtypes.ResourceNotFoundException
	return errors.As(err, &notFound)
}

// watch follows the events of a MicroVM, once per MicroVM.
func (b *MicroVM) watch(name, id string) {
	b.mu.Lock()
	if b.watching[name] == id {
		b.mu.Unlock()
		return
	}
	b.watching[name] = id
	b.mu.Unlock()
	go b.watchLoop(name, id)
}

// watchLoop keeps the events connection of a MicroVM open. When the agent
// announces the termination, or the connection is lost, the state of the VM
// is checked with Lambda: the announcement alone is not trusted, because
// anybody inside the VM could send it. A VM that really went away is handed
// to OnTerminated, unless the daemon stopped it.
func (b *MicroVM) watchLoop(name, id string) {
	defer func() {
		b.mu.Lock()
		if b.watching[name] == id {
			delete(b.watching, name)
		}
		b.mu.Unlock()
	}()
	backoff := b.poll / 2
	for {
		mv, ok := b.state.MicroVMOf(name)
		if !ok || mv.ID != id {
			return
		}
		ctx, cancel := context.WithCancel(b.ctx)
		connected := time.Now()
		err := b.clientFor(mv).Events(ctx, func(ev agent.Control) {
			if ev.Type == agent.ControlTerminating {
				b.log.Info("the agent announced the termination of its MicroVM", "name", name, "microvm", id, "reason", ev.Reason)
				cancel()
			}
		})
		cancel()
		if b.ctx.Err() != nil {
			return
		}
		b.log.Debug("the events connection of a MicroVM ended", "name", name, "microvm", id, "err", err)

		st, gone := b.settle(b.ctx, id)
		if !gone {
			if st == mvtypes.MicrovmStateSuspending || st == mvtypes.MicrovmStateSuspended {
				// Reconnecting would resume it. The next start watches the
				// VM again.
				b.forget(id)
				return
			}
			if time.Since(connected) > time.Minute {
				backoff = b.poll / 2
			}
			if sleep(b.ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		b.forget(id)
		b.mu.Lock()
		stopping := b.stopping[name] == id
		b.mu.Unlock()
		if stopping {
			return
		}
		replaced := false
		_ = b.state.UpdateMicroVM(name, func(m *state.MicroVM) error {
			if m.ID == id {
				clearCurrent(m)
				replaced = true
			}
			return nil
		})
		if !replaced {
			return
		}
		b.log.Info("the MicroVM of a container terminated", "name", name, "microvm", id)
		if b.OnTerminated != nil {
			go b.OnTerminated(name)
		}
		return
	}
}

// settle asks Lambda about a MicroVM whose events connection ended. The state
// is eventually consistent, so a VM that still looks alive is asked again
// for a while. gone reports a VM that is terminating, terminated or unknown.
func (b *MicroVM) settle(ctx context.Context, id string) (mvtypes.MicrovmState, bool) {
	var st mvtypes.MicrovmState
	for range 10 {
		out, err := b.get(ctx, id)
		switch {
		case isNotFound(err):
			return mvtypes.MicrovmStateTerminated, true
		case err == nil:
			st = out.State
			if st == mvtypes.MicrovmStateTerminating || st == mvtypes.MicrovmStateTerminated {
				return st, true
			}
		}
		if sleep(ctx, b.poll) != nil {
			break
		}
	}
	return st, false
}

// Reconcile looks at the MicroVMs recorded in the state after the daemon
// started: running ones are watched again, and the ones that terminated in
// the meantime are forgotten, so that the next login starts a new one.
func (b *MicroVM) Reconcile(ctx context.Context) {
	for _, vm := range b.state.List() {
		mv, ok := b.state.MicroVMOf(vm.Name)
		if !ok || mv.ID == "" {
			continue
		}
		out, err := b.get(ctx, mv.ID)
		switch {
		case isNotFound(err):
		case err != nil:
			b.log.Warn("look up a MicroVM", "name", vm.Name, "microvm", mv.ID, "err", err)
			continue
		case out.State == mvtypes.MicrovmStateRunning || out.State == mvtypes.MicrovmStatePending:
			b.watch(vm.Name, mv.ID)
			continue
		case out.State == mvtypes.MicrovmStateSuspending || out.State == mvtypes.MicrovmStateSuspended:
			continue
		}
		b.log.Info("the MicroVM of a container terminated while the daemon was down", "name", vm.Name, "microvm", mv.ID)
		_ = b.state.UpdateMicroVM(vm.Name, func(m *state.MicroVM) error {
			if m.ID == mv.ID {
				clearCurrent(m)
			}
			return nil
		})
	}
}

// envList turns the environment of a container into NAME=value entries.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func efsTags(tags map[string]string) []efstypes.Tag {
	out := make([]efstypes.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, efstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func iamTags(tags map[string]string) []iamtypes.Tag {
	out := make([]iamtypes.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
