package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

type ec2Transport struct {
	region string
	cfg    *config.EC2Config
	state  *state.State
	dial   func(context.Context, string) (net.Conn, error)
}

// dialVPC reaches the guest agent on its private address. The daemon must be
// inside the VPC or routed to it; the guest security group admits nothing else.
func dialVPC(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
}

func newEC2Transport(cloud aws.Config, cfg *config.EC2Config, st *state.State) *ec2Transport {
	return &ec2Transport{region: cloud.Region, cfg: cfg, state: st, dial: dialVPC}
}

func (t *ec2Transport) client(name string) (*agent.Client, error) {
	instance, ok := t.state.EC2Of(name)
	if !ok || instance.InstanceID == "" || instance.Secret == "" {
		return nil, fmt.Errorf("container %s has no configured EC2 instance", name)
	}
	if instance.Region != "" && instance.Region != t.region {
		return nil, fmt.Errorf("EC2 region of %s has changed from %s to %s", name, instance.Region, t.region)
	}
	if instance.FileSystemID != "" && instance.FileSystemID != t.cfg.FileSystemID {
		return nil, fmt.Errorf("configured EFS file system differs from that of EC2 sandbox %s", name)
	}
	if instance.PrivateIP == "" {
		return nil, fmt.Errorf("EC2 sandbox %s has no recorded private address: sandboxes created before the removal of Session Manager must be recreated", name)
	}
	addr := net.JoinHostPort(instance.PrivateIP, strconv.Itoa(agent.DataPort))
	return &agent.Client{
		BaseURL: "http://" + addr,
		Header: func(context.Context) (http.Header, error) {
			return http.Header{agent.SecretHeader: {instance.Secret}}, nil
		},
		HTTPClient: &http.Client{Transport: &http.Transport{
			// A connection must not outlive a stop or reboot of the guest.
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 90 * time.Second,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return t.dial(ctx, addr)
			},
		}},
	}, nil
}

//go:embed ec2_bootstrap.sh
var ec2Bootstrap string

//go:embed ec2_guest_config.sh
var ec2GuestConfig string

func (b *EC2) userData(_ context.Context, vm *state.VM, mounts []agent.ShareMount) ([]byte, error) {
	instance, ok := b.transport.state.EC2Of(vm.Name)
	if !ok || instance.Secret == "" {
		return nil, errors.New("missing EC2 agent secret")
	}
	cfg := b.transport.cfg
	// User data is readable through the EC2 API, so it carries only a digest.
	digest := sha256.Sum256([]byte(instance.Secret))
	guest, err := json.Marshal(agent.RunConfig{
		Name: vm.Name, SecretSHA256: hex.EncodeToString(digest[:]), Env: envList(vm.Env), FileSystemID: cfg.FileSystemID, Shares: mounts,
	})
	if err != nil {
		return nil, err
	}
	assets, err := json.Marshal(map[string]string{"agent": cfg.AgentS3URI, "efs": cfg.EFSUtilsS3URI, "region": b.transport.region})
	if err != nil {
		return nil, err
	}
	encode := base64.StdEncoding.EncodeToString
	data := strings.NewReplacer("@CONFIG@", encode(guest), "@ASSETS@", encode(assets), "@GUEST_CONFIG@", strings.TrimSpace(ec2GuestConfig)).Replace(ec2Bootstrap)
	if len(data) > 16<<10 {
		return nil, errors.New("EC2 bootstrap configuration exceeds the 16 KiB user-data limit")
	}
	return []byte(data), nil
}

func (b *EC2) waitAgent(ctx context.Context, vm *state.VM, mounts []agent.ShareMount) error {
	client, err := b.transport.client(vm.Name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, b.transport.cfg.StartTimeout.Duration())
	defer cancel()
	shares := agent.EC2Shares{FileSystemID: b.transport.cfg.FileSystemID, Shares: mounts}
	for {
		err = client.ConfigureShares(ctx, shares)
		if err == nil {
			return nil
		}
		if wait := sleep(ctx, 2*time.Second); wait != nil {
			return fmt.Errorf("wait for EC2 agent of %s: %w (last attempt: %v)", vm.Name, wait, err)
		}
	}
}

func (b *EC2) startGuest(ctx context.Context, vm *state.VM, e Exec, tty bool, rows, cols int) (*agent.Session, error) {
	client, err := b.transport.client(vm.Name)
	if err != nil {
		return nil, err
	}
	req := agent.ExecRequest{Argv: e.Argv, Env: e.Env, Dir: e.Dir, TTY: tty, Rows: rows, Cols: cols}
	return client.Start(ctx, req, e.Stdin, e.Stdout, e.Stderr)
}

func (b *EC2) StartProcess(ctx context.Context, vm *state.VM, e Exec) (Process, error) {
	return b.startGuest(ctx, vm, e, false, 0, 0)
}

func (b *EC2) StartTerminal(ctx context.Context, vm *state.VM, e Exec, rows, cols int) (Terminal, error) {
	return b.startGuest(ctx, vm, e, true, rows, cols)
}

func (b *EC2) Run(ctx context.Context, vm *state.VM, e Exec) (int, error) {
	p, err := b.StartProcess(ctx, vm, e)
	if err != nil {
		return 1, err
	}
	return p.Wait()
}

// SyncConfig applies the current guest settings of the bootstrap to a running
// instance, which received the settings of the version that launched it.
func (b *EC2) SyncConfig(ctx context.Context, vm *state.VM) error {
	var stderr bytes.Buffer
	code, err := b.Run(ctx, vm, Exec{
		Argv:   []string{"/bin/bash", "-c", ec2GuestConfig},
		Stdout: io.Discard, Stderr: &stderr,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sync the guest settings of %s (exit %d): %s", vm.Name, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (b *EC2) Dial(ctx context.Context, vm *state.VM, port int) (net.Conn, error) {
	client, err := b.transport.client(vm.Name)
	if err != nil {
		return nil, err
	}
	return client.Dial(ctx, port)
}

func (b *EC2) bootID(ctx context.Context, vm *state.VM) (string, error) {
	client, err := b.transport.client(vm.Name)
	if err != nil {
		return "", err
	}
	return client.BootID(ctx)
}

// Ubuntu 24.04 AMIs use a partitioned ext4 EBS root. Check the whole
// disk's visible size before accepting growpart's idempotent NOCHANGE.
const growEC2Root = `set -eu
dev=$(readlink -f "$(findmnt -nro SOURCE /)")
parent=$(lsblk -ndo PKNAME "$dev")
part=$(cat "/sys/class/block/${dev##*/}/partition")
disk="/dev/$parent"
[ -n "$parent" ] && [ "$(findmnt -nro FSTYPE /)" = ext4 ] || { echo "expected a partitioned ext4 root" >&2; exit 1; }
for i in $(seq 1 30); do
    if [ "$(blockdev --getsize64 "$disk")" -ge "$1" ]; then break; fi
    sleep 1
done
[ "$(blockdev --getsize64 "$disk")" -ge "$1" ] || { echo "guest still sees the old EBS disk size" >&2; exit 1; }
if ! out=$(LC_ALL=C growpart "$disk" "$part" 2>&1); then
    case "$out" in NOCHANGE:*) ;; *) echo "$out" >&2; exit 1 ;; esac
fi
resize2fs "$dev"`

func (b *EC2) growRoot(ctx context.Context, vm *state.VM) error {
	var stderr bytes.Buffer
	code, err := b.Run(ctx, vm, Exec{
		Argv:   []string{"/bin/sh", "-c", growEC2Root, "grow-root", strconv.FormatInt(vm.Disk, 10)},
		Stdout: io.Discard, Stderr: &stderr,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("grow EC2 root filesystem (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (b *EC2) Collect(ctx context.Context, vm *state.VM) (Metrics, error) {
	// /proc/stat exposes CPU time in clock ticks. getconf accounts for the
	// guest's tick rate; memory counters in /proc/meminfo are in KiB.
	const sample = `set -eu
awk -v hz="$(getconf CLK_TCK)" '/^cpu / { printf "%.0f ", ($2+$3+$4+$7+$8+$9)*1000000000/hz; exit }' /proc/stat
awk '/^MemTotal:/ {t=$2} /^MemAvailable:/ {a=$2} /^SwapTotal:/ {s=$2} /^SwapFree:/ {f=$2} END {printf "%.0f %.0f %.0f ", (t-a)*1024, (s-f)*1024, t*1024}' /proc/meminfo
set -- /proc/[0-9]*; echo "$#"`
	var out bytes.Buffer
	code, err := b.Run(ctx, vm, Exec{Argv: []string{"/bin/sh", "-c", sample}, Stdout: &out})
	if err != nil {
		return Metrics{}, err
	}
	if code != 0 {
		return Metrics{}, fmt.Errorf("EC2 metrics exited with status %d", code)
	}
	var m Metrics
	_, err = fmt.Fscan(&out, &m.CPUUsageNS, &m.MemoryBytes, &m.MemorySwap, &m.MemoryLimit, &m.ProcessCount)
	return m, err
}
