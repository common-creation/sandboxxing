package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

type ec2Requests struct {
	t                        *testing.T
	power, kind, token, fail string
	size, target             int
	events                   []string
}

// Do accepts EC2's Query API on an in-process transport. This exercises
// SDK serialization of SkipOsShutdown and the actual ordering of API calls.
func (f *ec2Requests) Do(r *http.Request) (*http.Response, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	op := r.Form.Get("Action")
	status := 200
	body := ""
	if op == "DescribeVolumesModifications" && f.target == 0 {
		status = http.StatusBadRequest
		body = "<Response><Errors><Error><Code>InvalidVolumeModification.NotFound</Code><Message>No previous modification</Message></Error></Errors></Response>"
	} else if f.fail == op {
		f.fail = ""
		status = 400
		body = "<Response><Errors><Error><Code>InjectedFailure</Code><Message>failed once</Message></Error></Errors></Response>"
	} else {
		switch op {
		case "DescribeImages":
			body = "<imagesSet><item><imageId>ami-ubuntu</imageId><name>ubuntu</name><architecture>x86_64</architecture><rootDeviceType>ebs</rootDeviceType><rootDeviceName>/dev/sda1</rootDeviceName><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeSize>8</volumeSize></ebs></item></blockDeviceMapping></item></imagesSet>"
		case "RunInstances":
			if f.power != "" {
				f.t.Fatal("duplicate RunInstances")
			}
			checks := map[string]string{"ImageId": "ami-ubuntu", "ClientToken": "token", "InstanceType": config.DefaultEC2InstanceType, "NetworkInterface.1.AssociatePublicIpAddress": "false", "NetworkInterface.1.DeviceIndex": "0", "NetworkInterface.1.SubnetId": "subnet-private", "NetworkInterface.1.SecurityGroupId.1": "sg-closed", "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true", "BlockDeviceMapping.1.Ebs.Encrypted": "true", "BlockDeviceMapping.1.Ebs.VolumeSize": "8", "MetadataOptions.HttpTokens": "required"}
			for key, want := range checks {
				if got := r.Form.Get(key); got != want {
					f.t.Errorf("%s=%q, want %q", key, got, want)
				}
			}
			if r.Form.Get("UserData") == "" {
				f.t.Fatal("missing bootstrap")
			}
			body = "<instancesSet><item><instanceId>i-own</instanceId><instanceType>m7i-flex.large</instanceType></item></instancesSet>"
			f.power = "running"
			f.events = append(f.events, "launch")
		case "DescribeInstances":
			if f.power == "" {
				break
			}
			body = fmt.Sprintf("<reservationSet><item><instancesSet><item><instanceId>i-own</instanceId><clientToken>%s</clientToken><instanceType>%s</instanceType><instanceState><name>%s</name></instanceState><rootDeviceName>/dev/sda1</rootDeviceName><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeId>vol-own</volumeId></ebs></item></blockDeviceMapping></item></instancesSet></item></reservationSet>", f.token, f.kind, f.power)
		case "DescribeVolumes":
			body = fmt.Sprintf("<volumeSet><item><volumeId>vol-own</volumeId><size>%d</size></item></volumeSet>", f.size)
		case "DescribeVolumesModifications":
			if f.target > 0 {
				body = fmt.Sprintf("<volumeModificationSet><item><volumeId>vol-own</volumeId><targetSize>%d</targetSize><modificationState>optimizing</modificationState></item></volumeModificationSet>", f.target)
			}
		case "DescribeInstanceTypes":
			body = "<instanceTypeSet><item><instanceType>m7i.large</instanceType><processorInfo><supportedArchitectures><item>x86_64</item></supportedArchitectures></processorInfo></item></instanceTypeSet>"
		case "ModifyVolume":
			if r.Form.Get("VolumeId") != "vol-own" || r.Form.Get("Size") != "16" {
				f.t.Fatalf("unexpected EBS modification %s", r.Form)
			}
			f.size, f.target = 16, 16
			f.events = append(f.events, "grow")
		case "StopInstances":
			f.power = "stopped"
			f.events = append(f.events, "stop:"+r.Form.Get("SkipOsShutdown"))
		case "ModifyInstanceAttribute":
			if f.power != "stopped" {
				f.t.Fatal("type changed before shutdown")
			}
			f.kind = r.Form.Get("InstanceType.Value")
			f.events = append(f.events, "change:"+f.kind)
		case "StartInstances":
			f.power = "running"
			f.events = append(f.events, "start")
		case "TerminateInstances":
			if f.power != "stopped" {
				f.t.Fatal("instance terminated before stop")
			}
			f.power = "terminated"
			f.events = append(f.events, "terminate")
		case "RebootInstances":
			f.events = append(f.events, "reboot")
		default:
			f.t.Fatalf("unexpected EC2 API %q", op)
		}
		body = "<" + op + "Response>" + body + "</" + op + "Response>"
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

type noEC2Profiles struct{}

func (noEC2Profiles) ensure(context.Context, *state.VM, string) (string, []agent.ShareMount, error) {
	return "profile", nil, nil
}
func (noEC2Profiles) remove(context.Context, string) error { return nil }

func ec2Fixture(t *testing.T, power string) (*EC2, *ec2Requests, *state.VM) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	vm := &state.VM{Name: "work", Backend: state.BackendEC2, Disk: 8 << 30, EC2: &state.EC2{ClientToken: "token", InstanceID: "i-own", InstanceType: config.DefaultEC2InstanceType, RootVolumeID: "vol-own", Ready: power == "running", Region: "test-region"}}
	if err = st.Add(vm); err != nil {
		t.Fatal(err)
	}
	wire := &ec2Requests{t: t, power: power, kind: vm.EC2.InstanceType, token: "token", size: 8}
	b := &EC2{cfg: &config.EC2Config{StartTimeout: config.Duration(time.Second)}, state: st, poll: time.Microsecond, region: "test-region",
		api: ec2.New(ec2.Options{Region: "test-region", BaseEndpoint: aws.String("https://fake.example"), Credentials: aws.AnonymousCredentials{}, HTTPClient: wire, RetryMaxAttempts: 1}), profiles: noEC2Profiles{}}
	b.guestReady = func(context.Context, *state.VM, []agent.ShareMount) error {
		wire.events = append(wire.events, "ready")
		return nil
	}
	b.growFileSystem = func(_ context.Context, v *state.VM) error {
		if v.Disk != 16<<30 {
			t.Fatalf("guest sees disk target %d", v.Disk)
		}
		wire.events = append(wire.events, "filesystem")
		return nil
	}
	b.bootIdentifier = func(context.Context, *state.VM) (string, error) {
		if slices.Contains(wire.events, "reboot") {
			return "new", nil
		}
		return "old", nil
	}
	return b, wire, vm
}

func TestEC2CombinedResizeResumesAfterTypeAPIFailure(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "running")
	wire.fail = "ModifyInstanceAttribute"
	if err := b.Resize(t.Context(), vm, 16<<30, "m7i.large"); err == nil {
		t.Fatal("missing injected error")
	}
	vm, _ = b.state.Get(vm.Name)
	if vm.Disk != 16<<30 || vm.EC2.PendingResize == nil || !vm.EC2.PendingResize.WasRunning {
		t.Fatalf("partial state: %+v", vm)
	}
	if err := b.Resize(t.Context(), vm, 16<<30, "m7i.large"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"grow", "filesystem", "stop:false", "change:m7i.large", "start", "ready"}; !slices.Equal(wire.events, want) {
		t.Fatalf("calls %v, want %v", wire.events, want)
	}
	vm, _ = b.state.Get(vm.Name)
	if !vm.EC2.Ready || vm.EC2.InstanceType != "m7i.large" || vm.EC2.PendingResize != nil {
		t.Fatalf("unexpected final state %+v", vm.EC2)
	}
}

func TestEC2ResizeStoppedAndDiskOnly(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "stopped")
	if err := b.Resize(t.Context(), vm, 16<<30, "m7i.large"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"grow", "change:m7i.large"}; !slices.Equal(wire.events, want) {
		t.Fatalf("calls %v", wire.events)
	}
	vm, _ = b.state.Get(vm.Name)
	if !vm.EC2.GrowOnBoot {
		t.Fatal("stopped sandbox lost pending filesystem growth")
	}
	if err := b.Start(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wire.events[2:], []string{"start", "ready", "filesystem"}) {
		t.Fatalf("boot calls %v", wire.events[2:])
	}
	b, wire, vm = ec2Fixture(t, "running")
	if err := b.Resize(t.Context(), vm, 4<<30, ""); err == nil {
		t.Fatal("accepted shrink")
	}
	if err := b.Resize(t.Context(), vm, (12<<30)+1, ""); err == nil {
		t.Fatal("accepted fractional GiB")
	}
	if err := b.Resize(t.Context(), vm, 16<<30, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wire.events, []string{"grow", "filesystem"}) {
		t.Fatalf("online resize %v", wire.events)
	}
}

func TestEC2ForceAndOwnership(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "running")
	if err := b.Restart(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wire.events, []string{"reboot", "ready"}) {
		t.Fatalf("restart %v", wire.events)
	}
	b.region = "wrong-region"
	if err := b.RemoveForce(t.Context(), vm, true); err == nil {
		t.Fatal("accepted another region")
	}
	b.region = vm.EC2.Region
	wire.token = "another-sandbox"
	if err := b.RemoveForce(t.Context(), vm, true); err == nil {
		t.Fatal("accepted another sandbox")
	}
	wire.token = "token"
	if err := b.RemoveForce(t.Context(), vm, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wire.events, []string{"reboot", "ready", "stop:true", "terminate"}) {
		t.Fatalf("force calls %v", wire.events)
	}
}

func TestEC2ProvisionDiscoversAcceptedLaunch(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "")
	b.cfg.DefaultImage = config.DefaultEC2Image
	b.cfg.Images = map[string]string{config.DefaultEC2Image: "ami-ubuntu"}
	b.cfg.SubnetID = "subnet-private"
	b.cfg.SecurityGroupIDs = []string{"sg-closed"}
	b.cfg.FileSystemID = "fs-test"
	b.transport = newEC2Transport(aws.Config{Region: b.region}, b.cfg, b.state)
	err := b.state.UpdateEC2(vm.Name, func(v *state.VM) {
		v.EC2.InstanceID = ""
		v.EC2.RootVolumeID = ""
		v.EC2.Region = ""
		v.EC2.Secret = "secret"
		v.Image = config.DefaultEC2Image
		v.Disk = 0
	})
	if err != nil {
		t.Fatal(err)
	}
	vm, _ = b.state.Get(vm.Name)
	if err := b.Provision(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	vm, _ = b.state.Get(vm.Name)
	if vm.EC2.InstanceID != "i-own" || vm.EC2.RootVolumeID != "vol-own" || vm.EC2.Region != b.region || vm.Disk != 8<<30 || !vm.EC2.Ready {
		t.Fatalf("launch state %+v", vm)
	}
	// Simulate an interrupted initial launch with only its token persisted.
	if err = b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.InstanceID = ""; v.EC2.RootVolumeID = ""; v.EC2.LaunchPending = true }); err != nil {
		t.Fatal(err)
	}
	vm, _ = b.state.Get(vm.Name)
	if err = b.Start(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wire.events, []string{"launch", "ready", "ready"}) {
		t.Fatalf("recovery %v", wire.events)
	}
}

func TestEC2ExplicitStopOverridesInterruptedResize(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "running")
	wire.fail = "ModifyInstanceAttribute"
	if err := b.Resize(t.Context(), vm, 16<<30, "m7i.large"); err == nil {
		t.Fatal("expected type change failure")
	}
	vm, _ = b.state.Get(vm.Name)
	if err := b.StopForce(t.Context(), vm, true); err != nil {
		t.Fatal(err)
	}
	vm, _ = b.state.Get(vm.Name)
	if vm.EC2.PendingResize.WasRunning {
		t.Fatal("explicit stop lost to pending resize")
	}
	if err := b.Resize(t.Context(), vm, 16<<30, "m7i.large"); err != nil {
		t.Fatal(err)
	}
	if wire.power != "stopped" {
		t.Fatal("recovery restarted explicitly stopped sandbox")
	}
}

func TestEC2FailedTypeCanBeReplaced(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "running")
	wire.fail = "ModifyInstanceAttribute"
	if err := b.Resize(t.Context(), vm, 16<<30, "m7i.large"); err == nil {
		t.Fatal("expected type change failure")
	}
	vm, _ = b.state.Get(vm.Name)
	if err := b.Resize(t.Context(), vm, 0, config.DefaultEC2InstanceType); err != nil {
		t.Fatal(err)
	}
	vm, _ = b.state.Get(vm.Name)
	if vm.EC2.PendingResize != nil || vm.EC2.InstanceType != config.DefaultEC2InstanceType || vm.Disk != 16<<30 || !vm.EC2.Ready {
		t.Fatalf("failed to recover after choosing the original type: %+v", vm)
	}
}

func TestEC2RestartWhenOriginalGuestUnreachable(t *testing.T) {
	b, wire, vm := ec2Fixture(t, "running")
	b.bootIdentifier = func(context.Context, *state.VM) (string, error) { return "", errors.New("unreachable guest") }
	err := b.Restart(t.Context(), vm)
	if err == nil || !strings.Contains(err.Error(), "accepted the reboot") {
		t.Fatalf("restart outcome %v", err)
	}
	after, _ := b.state.Get(vm.Name)
	if after.EC2.Ready || !slices.Equal(wire.events, []string{"reboot"}) {
		t.Fatalf("reboot readiness=%v calls=%v", after.EC2.Ready, wire.events)
	}
}
