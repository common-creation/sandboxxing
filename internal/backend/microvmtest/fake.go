// Package microvmtest fakes the AWS APIs that the microvm backend uses. Every
// fake MicroVM runs a real agent on loopback HTTP servers, so that commands,
// lifecycle hooks and events behave like in a VM, without AWS.
package microvmtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
)

// Account is the account of the fake resources.
const Account = "123456789012"

// Fake implements backend.MicroVMAPI, backend.EFSAPI and backend.IAMAPI.
type Fake struct {
	mu  sync.Mutex
	seq int

	VMs  map[string]*VM
	Runs []*lambdamicrovms.RunMicrovmInput

	AccessPoints map[string]*efs.CreateAccessPointInput
	Roles        map[string]*iam.CreateRoleInput
	Policies     map[string]string
}

// VM is a fake MicroVM.
type VM struct {
	ID    string
	State mvtypes.MicrovmState
	// Config is the run hook payload the VM received.
	Config agent.RunConfig
	Agent  *agent.Server
	Home   *Home

	hooks, data *httptest.Server
	// hijacked are the WebSocket connections of the agent. A real VM that
	// ends takes them down; httptest does not track them.
	hijackedMu sync.Mutex
	hijacked   []net.Conn
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{
		VMs:          map[string]*VM{},
		AccessPoints: map[string]*efs.CreateAccessPointInput{},
		Roles:        map[string]*iam.CreateRoleInput{},
		Policies:     map[string]string{},
	}
}

// Close stops the agents.
func (f *Fake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, vm := range f.VMs {
		vm.shutdown()
	}
}

// Home records the home directory operations of an agent.
type Home struct {
	mu       sync.Mutex
	Mounts   int
	Unmounts int
}

func (h *Home) Mount(ctx context.Context, cfg *agent.RunConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Mounts++
	return nil
}

func (h *Home) Check(ctx context.Context, cfg *agent.RunConfig) error { return nil }

func (h *Home) Unmount(ctx context.Context, cfg *agent.RunConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Unmounts++
	return nil
}

func (vm *VM) shutdown() {
	if vm.data != nil {
		vm.hijackedMu.Lock()
		for _, c := range vm.hijacked {
			c.Close()
		}
		vm.hijacked = nil
		vm.hijackedMu.Unlock()
		vm.data.CloseClientConnections()
		vm.data.Close()
		vm.hooks.Close()
		vm.data = nil
	}
}

func (f *Fake) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefix, f.seq)
}

func notFound(id string) error {
	return &mvtypes.ResourceNotFoundException{Message: aws.String("no MicroVM " + id)}
}

func (f *Fake) RunMicrovm(ctx context.Context, in *lambdamicrovms.RunMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	f.mu.Lock()
	f.Runs = append(f.Runs, in)
	id := f.next("mvm")
	f.mu.Unlock()

	home := &Home{}
	srv := agent.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Home = home
	vm := &VM{ID: id, State: mvtypes.MicrovmStatePending, Agent: srv, Home: home,
		hooks: httptest.NewServer(srv.HookHandler())}
	vm.data = httptest.NewUnstartedServer(srv.DataHandler())
	vm.data.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateHijacked {
			vm.hijackedMu.Lock()
			vm.hijacked = append(vm.hijacked, c)
			vm.hijackedMu.Unlock()
		}
	}
	vm.data.Start()
	_ = json.Unmarshal([]byte(aws.ToString(in.RunHookPayload)), &vm.Config)
	// Lambda calls the run hook before the VM receives traffic.
	body, _ := json.Marshal(agent.RunRequest{MicrovmID: id, RunHookPayload: aws.ToString(in.RunHookPayload)})
	resp, err := http.Post(vm.hooks.URL+agent.HookPath+"run", "application/json", bytes.NewReader(body))
	vm.State = mvtypes.MicrovmStateRunning
	if err != nil || resp.StatusCode != http.StatusOK {
		vm.State = mvtypes.MicrovmStateTerminated
	}
	if resp != nil {
		resp.Body.Close()
	}
	f.mu.Lock()
	f.VMs[id] = vm
	f.mu.Unlock()
	now := time.Now()
	return &lambdamicrovms.RunMicrovmOutput{MicrovmId: aws.String(id), Endpoint: aws.String(vm.data.URL),
		StartedAt: &now, State: vm.State}, nil
}

func (f *Fake) GetMicrovm(ctx context.Context, in *lambdamicrovms.GetMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.VMs[aws.ToString(in.MicrovmIdentifier)]
	if !ok {
		return nil, notFound(aws.ToString(in.MicrovmIdentifier))
	}
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String(vm.ID), State: vm.State}, nil
}

func (f *Fake) ResumeMicrovm(ctx context.Context, in *lambdamicrovms.ResumeMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.VMs[aws.ToString(in.MicrovmIdentifier)]
	if !ok {
		return nil, notFound(aws.ToString(in.MicrovmIdentifier))
	}
	vm.State = mvtypes.MicrovmStateRunning
	return &lambdamicrovms.ResumeMicrovmOutput{}, nil
}

func (f *Fake) TerminateMicrovm(ctx context.Context, in *lambdamicrovms.TerminateMicrovmInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	if err := f.Expire(aws.ToString(in.MicrovmIdentifier)); err != nil {
		return nil, err
	}
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

// Expire terminates a VM the way Lambda does at its maximum duration: the VM
// becomes TERMINATING, the terminate hook runs and the VM goes away.
func (f *Fake) Expire(id string) error {
	f.mu.Lock()
	vm, ok := f.VMs[id]
	if !ok {
		f.mu.Unlock()
		return notFound(id)
	}
	if vm.State == mvtypes.MicrovmStateTerminated {
		f.mu.Unlock()
		return nil
	}
	vm.State = mvtypes.MicrovmStateTerminating
	hooks := vm.hooks
	f.mu.Unlock()

	if hooks != nil {
		if resp, err := http.Post(hooks.URL+agent.HookPath+"terminate", "", nil); err == nil {
			resp.Body.Close()
		}
	}
	f.mu.Lock()
	vm.State = mvtypes.MicrovmStateTerminated
	vm.shutdown()
	f.mu.Unlock()
	return nil
}

// Suspend suspends a VM.
func (f *Fake) Suspend(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.VMs[id]; ok {
		vm.State = mvtypes.MicrovmStateSuspended
	}
}

// AnnounceTermination calls the terminate hook of a VM without terminating
// it, which is what a user inside the VM could do.
func (f *Fake) AnnounceTermination(id string) error {
	f.mu.Lock()
	vm, ok := f.VMs[id]
	f.mu.Unlock()
	if !ok {
		return notFound(id)
	}
	resp, err := http.Post(vm.hooks.URL+agent.HookPath+"terminate", "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get returns a fake VM.
func (f *Fake) Get(id string) *VM {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.VMs[id]
}

// StateOf returns the state of a fake VM.
func (f *Fake) StateOf(id string) mvtypes.MicrovmState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.VMs[id]; ok {
		return vm.State
	}
	return ""
}

// RunCount returns the number of RunMicrovm calls.
func (f *Fake) RunCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Runs)
}

func (f *Fake) CreateMicrovmAuthToken(ctx context.Context, in *lambdamicrovms.CreateMicrovmAuthTokenInput, opts ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	return &lambdamicrovms.CreateMicrovmAuthTokenOutput{
		AuthToken: map[string]string{"X-aws-proxy-auth": "token-" + aws.ToString(in.MicrovmIdentifier)},
	}, nil
}

func (f *Fake) CreateAccessPoint(ctx context.Context, in *efs.CreateAccessPointInput, opts ...func(*efs.Options)) (*efs.CreateAccessPointOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.next("fsap")
	f.AccessPoints[id] = in
	return &efs.CreateAccessPointOutput{
		AccessPointId:  aws.String(id),
		AccessPointArn: aws.String("arn:aws:elasticfilesystem:us-east-1:" + Account + ":access-point/" + id),
		LifeCycleState: efstypes.LifeCycleStateCreating,
	}, nil
}

func (f *Fake) DescribeAccessPoints(ctx context.Context, in *efs.DescribeAccessPointsInput, opts ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.AccessPointId)
	if _, ok := f.AccessPoints[id]; !ok {
		return nil, &efstypes.AccessPointNotFound{Message: aws.String(id)}
	}
	return &efs.DescribeAccessPointsOutput{AccessPoints: []efstypes.AccessPointDescription{{
		AccessPointId: aws.String(id), LifeCycleState: efstypes.LifeCycleStateAvailable,
	}}}, nil
}

func (f *Fake) DeleteAccessPoint(ctx context.Context, in *efs.DeleteAccessPointInput, opts ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.AccessPointId)
	if _, ok := f.AccessPoints[id]; !ok {
		return nil, &efstypes.AccessPointNotFound{Message: aws.String(id)}
	}
	delete(f.AccessPoints, id)
	return &efs.DeleteAccessPointOutput{}, nil
}

func (f *Fake) CreateRole(ctx context.Context, in *iam.CreateRoleInput, opts ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.RoleName)
	f.Roles[name] = in
	arn := "arn:aws:iam::" + Account + ":role" + aws.ToString(in.Path) + name
	return &iam.CreateRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Arn: aws.String(arn)}}, nil
}

func (f *Fake) PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, opts ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Policies[aws.ToString(in.RoleName)] = aws.ToString(in.PolicyDocument)
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *Fake) DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, opts ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Policies, aws.ToString(in.RoleName))
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *Fake) DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, opts ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.RoleName)
	if _, ok := f.Roles[name]; !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String(name)}
	}
	delete(f.Roles, name)
	return &iam.DeleteRoleOutput{}, nil
}
