package sshserver

import (
	"context"
	"io"
	"sort"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/cli"
	"github.com/common-creation/sandboxxing/internal/state"
)

// sessionRequest captures the parameters of one session channel.
type sessionRequest struct {
	mu       sync.Mutex
	pty      bool
	term     string
	rows     int
	cols     int
	exec     string
	hasExec  bool
	shell    bool
	subsys   string
	env      map[string]string
	winsize  chan windowSize
	ptyReady bool
}

// windowSize carries a terminal resize event.
type windowSize struct {
	rows int
	cols int
}

// environment returns the variables that must be visible inside the
// container: the terminal type from the pty request and the variables the
// client asked to forward with an env request. Without TERM programs such as
// htop, vim and less cannot select a terminal description and either fail or
// draw nothing.
func (s *sessionRequest) environment() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := make([]string, 0, len(s.env)+1)
	if s.term != "" {
		env = append(env, "TERM="+s.term)
	}
	names := make([]string, 0, len(s.env))
	for name := range s.env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+s.env[name])
	}
	return env
}

// size returns the last known terminal size, defaulting to 24x80.
func (s *sessionRequest) size() (rows, cols int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return normaliseSize(s.rows, s.cols)
}

// enableResize turns on resize forwarding and returns the channel that
// carries the events.
func (s *sessionRequest) enableResize() chan windowSize {
	ch := make(chan windowSize, 4)
	s.mu.Lock()
	s.winsize = ch
	s.ptyReady = true
	s.mu.Unlock()
	return ch
}

// disableResize stops resize forwarding. It is safe to call more than once
// for the same channel.
func (s *sessionRequest) disableResize(ch chan windowSize) {
	s.mu.Lock()
	if s.winsize == ch {
		s.winsize = nil
		s.ptyReady = false
		close(ch)
	}
	s.mu.Unlock()
}

func (s *sessionRequest) setSize(rows, cols int) {
	s.mu.Lock()
	s.rows, s.cols = rows, cols
	ch := s.winsize
	ready := s.ptyReady
	if ch != nil && ready {
		// The send happens under the same lock that guards the close in
		// disableResize, so a resize can never race with the shutdown.
		select {
		case ch <- windowSize{rows: rows, cols: cols}:
		default:
		}
	}
	s.mu.Unlock()
}

// handleSession services one SSH session channel. Exactly one of exec, shell
// or subsystem is expected, as in OpenSSH.
//
// The session context is cancelled as soon as the client goes away, which
// aborts the process tree of a running command: closing the SSH connection or
// pressing Ctrl-C during `new` stops pacstrap and leaves no half-built image.
func (s *Server) handleSession(ctx context.Context, conn *ssh.ServerConn, ch ssh.Channel, requests <-chan *ssh.Request) {
	session := &sessionRequest{env: map[string]string{}, rows: 24, cols: 80}
	start := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.watchSession(ctx, conn, cancel)

	go func() {
		defer once.Do(func() { close(start) })
		for req := range requests {
			switch req.Type {
			case "pty-req":
				var payload struct {
					Term   string
					Cols   uint32
					Rows   uint32
					Width  uint32
					Height uint32
					Modes  string
				}
				if err := ssh.Unmarshal(req.Payload, &payload); err == nil {
					session.mu.Lock()
					session.pty = true
					session.term = payload.Term
					session.mu.Unlock()
					session.setSize(int(payload.Rows), int(payload.Cols))
				}
				req.Reply(true, nil)
			case "window-change":
				var payload struct {
					Cols   uint32
					Rows   uint32
					Width  uint32
					Height uint32
				}
				if err := ssh.Unmarshal(req.Payload, &payload); err == nil {
					session.setSize(int(payload.Rows), int(payload.Cols))
				}
				if req.WantReply {
					req.Reply(true, nil)
				}
			case "env":
				var payload struct {
					Name  string
					Value string
				}
				if err := ssh.Unmarshal(req.Payload, &payload); err == nil {
					session.mu.Lock()
					session.env[payload.Name] = payload.Value
					session.mu.Unlock()
				}
				req.Reply(true, nil)
			case "exec":
				var payload struct {
					Command string
				}
				if err := ssh.Unmarshal(req.Payload, &payload); err == nil {
					session.mu.Lock()
					session.exec = payload.Command
					session.hasExec = true
					session.mu.Unlock()
				}
				req.Reply(true, nil)
				once.Do(func() { close(start) })
				// The loop keeps running: window-change requests arrive
				// after this message and must keep being handled.
			case "shell":
				session.mu.Lock()
				session.shell = true
				session.mu.Unlock()
				req.Reply(true, nil)
				once.Do(func() { close(start) })
			case "subsystem":
				var payload struct {
					Name string
				}
				if err := ssh.Unmarshal(req.Payload, &payload); err == nil {
					session.mu.Lock()
					session.subsys = payload.Name
					session.mu.Unlock()
				}
				req.Reply(false, nil)
				once.Do(func() { close(start) })
			default:
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
		once.Do(func() { close(start) })
	}()

	select {
	case <-start:
	case <-ctx.Done():
		ch.Close()
		return
	}

	user := conn.User()
	session.mu.Lock()
	subsys := session.subsys
	interactive := session.hasExec || session.shell
	session.mu.Unlock()

	if subsys != "" || !interactive {
		if subsys != "" {
			s.log.Debug("unsupported subsystem requested", "subsystem", subsys, "user", user)
			s.exitStatus(ch, 1)
		}
		ch.Close()
		return
	}

	if s.cfg.IsAdminUser(user) {
		s.runControl(ctx, session, ch)
	} else {
		s.runContainer(ctx, session, ch, user)
	}
	ch.Close()
}

// watchSession cancels the session context when the SSH connection ends, so
// that a cancelled or dropped client aborts the work it started.
func (s *Server) watchSession(ctx context.Context, conn ssh.Conn, cancel context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conn.Wait()
	}()
	select {
	case <-ctx.Done():
	case <-done:
		s.log.Debug("client disconnected, cancelling the running command", "remote", conn.RemoteAddr().String())
	}
	cancel()
}

// runControl serves the command interface for a control user.
func (s *Server) runControl(ctx context.Context, session *sessionRequest, ch ssh.Channel) {
	rows, cols := session.size()
	interactive := session.ptyEnabled()
	out := io.Writer(ch)
	errOut := io.Writer(ch.Stderr())
	if interactive {
		// The control session has no pseudo terminal, so the line
		// discipline cannot turn the LF of the command output into CRLF.
		// An interactive client has its terminal in raw mode and would
		// keep the column on LF, which makes every line start where the
		// previous one ended.
		out = newCRLFWriter(out)
		errOut = newCRLFWriter(errOut)
	}
	cio := &cli.IO{
		In:   &stdio{ch: ch},
		Out:  out,
		Err:  errOut,
		TTY:  interactive,
		Rows: rows,
		Cols: cols,
	}

	session.mu.Lock()
	commandLine := session.exec
	hasExec := session.hasExec
	session.mu.Unlock()

	if !hasExec {
		s.exitStatus(ch, s.runner.Shell(ctx, cio))
		return
	}
	argv, err := shlex(commandLine)
	if err != nil {
		s.exitStatus(ch, s.failTo(out, err))
		return
	}
	if len(argv) == 0 {
		s.exitStatus(ch, s.runner.Shell(ctx, cio))
		return
	}
	s.exitStatus(ch, s.runner.Run(ctx, argv, cio))
}

func (s *sessionRequest) ptyEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pty
}

// enterAs is the account a container login starts as. Every container name is
// a user name at the SSH level, and the container side is always entered as
// root.
const enterAs = "root"

// runContainer turns the session into a login inside the named container.
// The name may carry a DNS style suffix, which `User %n` in ssh_config
// produces when a host alias such as demo.sbx is used.
func (s *Server) runContainer(ctx context.Context, session *sessionRequest, ch ssh.Channel, user string) {
	name := state.StripDomain(user)
	if !state.ValidName(name) {
		s.fail(ch, &notFoundError{user: user})
		return
	}
	if _, ok := s.state.Get(name); !ok {
		s.fail(ch, &notFoundError{user: name})
		return
	}
	if err := s.vms.EnsureStarted(ctx, name); err != nil {
		s.fail(ch, err)
		return
	}

	usePTY := session.ptyEnabled()
	session.mu.Lock()
	commandLine := session.exec
	hasExec := session.hasExec
	session.mu.Unlock()

	argv := []string{"/bin/bash", "-l"}
	if hasExec && commandLine != "" {
		argv = []string{"/bin/bash", "-lc", commandLine}
	}
	// TERM and the variables the client forwarded must reach the program
	// inside the container, otherwise full screen applications cannot pick a
	// terminal description.
	env := session.environment()

	// The login starts in the home directory of root instead of the container
	// root. HOME is looked up inside the container, and the working directory
	// is set with it because a login shell does not change directory on its
	// own. The lookup falls back to "/" when the directory does not exist,
	// because nsenter refuses to start in a missing directory.
	home := s.vms.HomeDir(ctx, name, enterAs, "/root")
	env = append(env, "HOME="+home)
	dir := home

	if usePTY {
		code, err := s.runPTY(ctx, name, argv, env, dir, ch, session)
		if err != nil {
			s.fail(ch, err)
			return
		}
		s.exitStatus(ch, code)
		return
	}
	s.runPipe(ctx, name, argv, env, dir, ch)
}

// notFoundError reports an unknown container name with a friendly hint.
type notFoundError struct{ user string }

func (e *notFoundError) Error() string { return "container " + e.user + " not found" }

func (s *Server) fail(ch ssh.Channel, err error) {
	s.exitStatus(ch, s.failTo(ch, err))
}

// failTo reports err on w and returns the exit status for it. A caller that
// owns a wrapped output stream passes it here so that the message follows the
// line ending convention of the session.
func (s *Server) failTo(w io.Writer, err error) int {
	if nf, ok := err.(*notFoundError); ok {
		_, _ = io.WriteString(w, "sandboxxing: "+nf.Error()+"\n")
		_, _ = io.WriteString(w, "list containers with: ssh "+s.runner.Host()+" ls\n")
		return 127
	}
	_, _ = io.WriteString(w, "sandboxxing: "+err.Error()+"\n")
	return 1
}
