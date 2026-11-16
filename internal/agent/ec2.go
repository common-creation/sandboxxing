package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/common-creation/sandboxxing/internal/process"
	"golang.org/x/sys/unix"
)

// EC2Shares is the desired set of EFS mounts on an EC2 instance. Its home
// directory always lives on the EBS root filesystem.
type EC2Shares struct {
	FileSystemID string       `json:"fs,omitempty"`
	Shares       []ShareMount `json:"shares,omitempty"`
}

const (
	PathShares = "/v1/shares"
	PathBootID = "/v1/boot-id"
)

// EC2Handler serves the data protocol to the daemon inside the VPC. The last desired
// mounts survive reboot on EBS. The mount journal in /run survives a service
// restart and is automatically cleared when the kernel loses its mounts.
func (s *Server) EC2Handler(ctx context.Context, configPath, journalPath string) (http.Handler, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg RunConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.SecretSHA256) != sha256.Size*2 {
		return nil, errors.New("invalid EC2 agent configuration")
	}
	if cfg.Name != "" {
		if err := unix.Sethostname([]byte(cfg.Name)); err != nil {
			s.log.Warn("set EC2 host name", "err", err)
		}
	}
	s.cfg = &cfg
	initial := EC2Shares{cfg.FileSystemID, cfg.Shares}
	mounts := &ec2Mounts{desiredPath: filepath.Join(filepath.Dir(configPath), "shares.json"), journalPath: journalPath}
	// Keep the management endpoint accessible when the old shares can no
	// longer be mounted (e.g. a share was revoked while stopped).
	mounts.mu.Lock()
	go func() {
		defer mounts.mu.Unlock()
		if err := mounts.restore(ctx, initial); err != nil {
			s.log.Warn("restore EC2 shares", "err", err)
		}
	}()
	data := s.dataRoutes()
	return s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == PathBootID {
			// Reboot verification must succeed even if a previous share
			// has been revoked and its replacement has not yet been sent.
			id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
			if err != nil {
				http.Error(w, "cannot read boot ID", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(id)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == PathShares {
			mounts.configure(w, r)
			return
		}
		if !mounts.ready.Load() {
			http.Error(w, "EC2 shares are not ready", http.StatusServiceUnavailable)
			return
		}
		data.ServeHTTP(w, r)
	})), nil
}

type ec2Mounts struct {
	mu          sync.Mutex
	ready       atomic.Bool
	desiredPath string
	journalPath string
}

func (m *ec2Mounts) restore(ctx context.Context, initial EC2Shares) error {
	raw, err := os.ReadFile(m.desiredPath)
	switch {
	case err == nil:
		initial = EC2Shares{}
		if err := json.Unmarshal(raw, &initial); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return m.apply(ctx, initial)
}

func (m *ec2Mounts) configure(w http.ResponseWriter, r *http.Request) {
	var desired EC2Shares
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&desired); err != nil {
		http.Error(w, "invalid EC2 share configuration", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.apply(r.Context(), desired); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sandboxxing-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (m *ec2Mounts) apply(ctx context.Context, desired EC2Shares) error {
	// Parents are mounted before children and unmounted after them.
	desired.Shares = slices.Clone(desired.Shares)
	slices.SortFunc(desired.Shares, func(a, b ShareMount) int { return strings.Compare(a.Target, b.Target) })
	m.ready.Store(false)
	ctx, cancel := context.WithTimeout(ctx, mountTimeout)
	defer cancel()
	var journal EC2Shares
	raw, err := os.ReadFile(m.journalPath)
	if err == nil {
		if err := json.Unmarshal(raw, &journal); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The journal records the intent before each mount, so a restart in the
	// middle of a mount can reconcile the kernel's actual state.
	if journal.FileSystemID != desired.FileSystemID || !slices.Equal(journal.Shares, desired.Shares) {
		for _, share := range slices.Backward(journal.Shares) {
			mounted, err := isMountpoint(ctx, share.Target)
			if err != nil {
				return err
			}
			if mounted {
				if err := unmount(ctx, share.Target); err != nil {
					return err
				}
			}
		}
		journal = EC2Shares{FileSystemID: desired.FileSystemID}
		if err := saveJSON(m.journalPath, journal); err != nil {
			return err
		}
	}
	for _, share := range desired.Shares {
		mounted, err := isMountpoint(ctx, share.Target)
		if err != nil {
			return err
		}
		if !slices.Contains(journal.Shares, share) {
			if mounted {
				return fmt.Errorf("share target %s already has a mount", share.Target)
			}
			journal.Shares = append(journal.Shares, share)
			if err := saveJSON(m.journalPath, journal); err != nil {
				return err
			}
		}
		if !mounted {
			if err := mountShare(ctx, desired.FileSystemID, share); err != nil {
				return err
			}
		}
	}
	if err := saveJSON(m.desiredPath, desired); err != nil {
		return err
	}
	m.ready.Store(true)
	return nil
}

func isMountpoint(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	out, err := process.Output(ctx, "mountpoint", "-q", "--", path)
	var status interface{ ExitCode() int }
	if errors.As(err, &status) && status.ExitCode() == 32 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect mount %s: %w: %s", path, err, out)
	}
	return true, nil
}

// ConfigureShares returns only after the guest has applied the desired mounts.
func (c *Client) ConfigureShares(ctx context.Context, shares EC2Shares) error {
	body, err := json.Marshal(shares)
	if err != nil {
		return err
	}
	_, err = c.ec2Request(ctx, http.MethodPost, PathShares, bytes.NewReader(body))
	return err
}

func (c *Client) BootID(ctx context.Context) (string, error) {
	body, err := c.ec2Request(ctx, http.MethodGet, PathBootID, nil)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(body))
	if len(id) != 36 {
		return "", errors.New("invalid boot ID from EC2 agent")
	}
	return id, nil
}

func (c *Client) ec2Request(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header, err = c.Header(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = http1Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	msg, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("EC2 agent %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	return msg, nil
}
