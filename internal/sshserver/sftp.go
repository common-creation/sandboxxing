package sshserver

import (
	"context"
	"errors"
	"io"
	"os/exec"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/vm"
)

// sftpSubsystemName is the subsystem that OpenSSH 9.0 and later uses for file
// transfer. Both `sftp` and modern `scp` (which defaults to SFTP) open a
// session channel and request this subsystem.
const sftpSubsystemName = "sftp"

// sftpServerCandidates lists where sftp-server lives inside the containers.
// Arch Linux (openssh package) uses /usr/lib/ssh/sftp-server; other layouts
// are probed as a fallback so a rebuilt image keeps working.
var sftpServerCandidates = []string{
	"/usr/lib/ssh/sftp-server",
	"/usr/lib/openssh/sftp-server",
	"/usr/libexec/sftp-server",
	"/usr/libexec/openssh/sftp-server",
}

// handleSFTPSubsystem serves subsystem "sftp" for a container login. The
// container is started on demand and its sftp-server binary is bridged to the
// SSH channel, which makes `sftp <name>@host` and `scp <file> <name>@host:/path`
// (OpenSSH 9.0+, SFTP mode) work without a dedicated client.
func (s *Server) handleSFTPSubsystem(ctx context.Context, session *sessionRequest, ch ssh.Channel, user string) {
	if s.cfg.IsAdminUser(user) {
		_, _ = io.WriteString(ch.Stderr(), "sandboxxing: sftp is not supported for control users; use a container name (sftp <name>@host)\n")
		s.log.Debug("sftp requested for a control user", "user", user)
		s.exitStatus(ch, 1)
		return
	}

	name := state.StripDomain(user)
	if !state.ValidName(name) {
		_, _ = io.WriteString(ch.Stderr(), "sandboxxing: invalid container name "+user+"\n")
		s.exitStatus(ch, 1)
		return
	}
	if _, ok := s.state.Get(name); !ok {
		_, _ = io.WriteString(ch.Stderr(), "sandboxxing: container "+name+" not found\n")
		_, _ = io.WriteString(ch.Stderr(), "list containers with: ssh "+s.runner.Host()+" ls\n")
		s.exitStatus(ch, 127)
		return
	}

	// Tests replace the container execution with a host process so that the
	// SFTP round trip can be verified without systemd-nspawn.
	if s.sftpRun != nil {
		code, err := s.sftpRun(ctx, name, "", nil, ch)
		if err != nil {
			_, _ = io.WriteString(ch.Stderr(), "sandboxxing: "+err.Error()+"\n")
			s.exitStatus(ch, 1)
			return
		}
		s.exitStatus(ch, code)
		return
	}

	if err := s.vms.EnsureStarted(ctx, name); err != nil {
		_, _ = io.WriteString(ch.Stderr(), "sandboxxing: "+err.Error()+"\n")
		s.exitStatus(ch, 1)
		return
	}

	env := session.environment()
	home := s.vms.HomeDir(ctx, name, enterAs, "/root")
	env = append(env, "HOME="+home)

	code, err := s.serveContainerSFTP(ctx, name, env, home, ch)
	if err != nil {
		_, _ = io.WriteString(ch.Stderr(), "sandboxxing: "+err.Error()+"\n")
		s.exitStatus(ch, 1)
		return
	}
	s.exitStatus(ch, code)
}

// serveContainerSFTP bridges the SSH channel to sftp-server running inside the
// container. The process runs in the container namespaces with its root as /,
// so paths resolve exactly like an interactive login (shares, /tmp mounts and
// symlinks included). Standard input and output carry the binary SFTP stream;
// standard error carries diagnostics and is forwarded to the channel's
// extended data, where sftp/scp clients display it.
func (s *Server) serveContainerSFTP(ctx context.Context, name string, env []string, dir string, ch ssh.Channel) (int, error) {
	path, err := s.lookupContainerSFTPServer(ctx, name)
	if err != nil {
		return 1, err
	}
	s.log.Info("sftp session started", "container", name, "server", path)
	cmd, err := s.vms.StartProcess(ctx, name, vm.Exec{
		Argv:   []string{path},
		Env:    env,
		Dir:    dir,
		Stdin:  ch,
		Stdout: ch,
		Stderr: ch.Stderr(),
	})
	if err != nil {
		return 1, err
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// lookupContainerSFTPServer finds an executable sftp-server inside the
// container. The image is built from the openssh package, so the first
// candidate normally wins; the others keep custom images working.
func (s *Server) lookupContainerSFTPServer(ctx context.Context, name string) (string, error) {
	for _, path := range sftpServerCandidates {
		if _, err := s.vms.Output(ctx, name, []string{"test", "-x", path}); err == nil {
			return path, nil
		}
	}
	return "", errors.New("sftp-server not found inside the container (install openssh)")
}
