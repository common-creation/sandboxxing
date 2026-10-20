package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"

	"github.com/common-creation/sandboxxing/internal/process"
	"github.com/common-creation/sandboxxing/internal/tty"
)

// Server is the agent: the hook handler that Lambda calls and the data
// handler that the daemon connects to.
type Server struct {
	log *slog.Logger
	// Home holds the operations on the home directory. Tests replace it.
	Home HomeMounter

	mu sync.Mutex
	// cfg is the instance configuration received by the run hook. The data
	// port refuses every request until it is set.
	cfg     *RunConfig
	started time.Time
	// subscribers receive the events of the VM.
	subscribers map[chan Control]struct{}
	// groups are the process groups of the running commands, which the
	// terminate hook stops.
	groups map[int]struct{}
}

// HomeMounter mounts and unmounts the home directory of the instance.
type HomeMounter interface {
	Mount(ctx context.Context, cfg *RunConfig) error
	// Check reports whether the mounted home directory still answers.
	Check(ctx context.Context, cfg *RunConfig) error
	Unmount(ctx context.Context, cfg *RunConfig) error
}

// NewServer creates the agent.
func NewServer(log *slog.Logger) *Server {
	return &Server{
		log:         log,
		Home:        EFSMounter{},
		subscribers: map[chan Control]struct{}{},
		groups:      map[int]struct{}{},
	}
}

// config returns the instance configuration, or nil before the run hook.
func (s *Server) config() *RunConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// HookHandler serves the lifecycle hooks.
func (s *Server) HookHandler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	// The image build waits for ready before it takes the snapshot, and
	// validate runs on a VM started from the snapshot. The agent has nothing
	// to prepare for either.
	mux.HandleFunc("POST "+HookPath+"ready", ok)
	mux.HandleFunc("POST "+HookPath+"validate", ok)
	mux.HandleFunc("POST "+HookPath+"run", s.hookRun)
	mux.HandleFunc("POST "+HookPath+"resume", s.hookResume)
	mux.HandleFunc("POST "+HookPath+"suspend", s.hookSuspend)
	mux.HandleFunc("POST "+HookPath+"terminate", s.hookTerminate)
	return mux
}

// hookRun configures the VM for its instance: the host name and the home
// directory. Traffic reaches the data port only after it returned.
func (s *Server) hookRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid run request", http.StatusBadRequest)
		return
	}
	var cfg RunConfig
	if err := json.Unmarshal([]byte(req.RunHookPayload), &cfg); err != nil || cfg.Secret == "" {
		http.Error(w, "invalid run hook payload", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.cfg != nil {
		// Lambda runs the hook once. A second call can only come from
		// inside the VM and must not change the instance.
		s.mu.Unlock()
		http.Error(w, "already running", http.StatusConflict)
		return
	}
	s.mu.Unlock()

	if cfg.Name != "" {
		if err := unix.Sethostname([]byte(cfg.Name)); err != nil {
			s.log.Warn("set the host name", "err", err)
		}
	}
	if cfg.FileSystemID != "" {
		if err := s.Home.Mount(r.Context(), &cfg); err != nil {
			s.log.Error("mount the home directory", "err", err)
			http.Error(w, "mount the home directory failed", http.StatusInternalServerError)
			return
		}
	}

	s.mu.Lock()
	s.cfg = &cfg
	s.started = time.Now()
	s.mu.Unlock()
	s.log.Info("instance configured", "name", cfg.Name, "microvm", req.MicrovmID)
	w.WriteHeader(http.StatusOK)
}

// hookResume makes sure that the home directory survived the suspension. An
// NFS mount whose connection did not come back is mounted again.
func (s *Server) hookResume(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	if cfg != nil && cfg.FileSystemID != "" {
		if err := s.Home.Check(r.Context(), cfg); err != nil {
			s.log.Warn("the home directory did not survive the suspension, mounting it again", "err", err)
			_ = s.Home.Unmount(r.Context(), cfg)
			if err := s.Home.Mount(r.Context(), cfg); err != nil {
				s.log.Error("mount the home directory", "err", err)
				http.Error(w, "mount the home directory failed", http.StatusInternalServerError)
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// hookSuspend flushes the pending writes before the memory is checkpointed.
func (s *Server) hookSuspend(w http.ResponseWriter, r *http.Request) {
	unix.Sync()
	w.WriteHeader(http.StatusOK)
}

// terminateGrace is how long the commands get to exit after SIGTERM. Lambda
// gives the whole hook at most a minute.
const terminateGrace = 5 * time.Second

// hookTerminate tells the daemon that the VM goes away, so that it can start
// the next one, and leaves the home directory consistent: the commands are
// stopped, the data is flushed and the file system is unmounted.
func (s *Server) hookTerminate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg, started := s.cfg, s.started
	s.mu.Unlock()
	reason := ReasonTerminated
	// The maximum duration counts from the start of the VM; a termination
	// shortly before it is the platform enforcing it.
	if cfg != nil && cfg.MaximumDuration > 0 && time.Since(started) >= cfg.MaximumDuration-time.Minute {
		reason = ReasonExpired
	}
	s.log.Info("the VM is being terminated", "reason", reason)
	s.broadcast(Control{Type: ControlTerminating, Reason: reason})

	unix.Sync()
	s.signalGroups(syscall.SIGTERM)
	deadline := time.Now().Add(terminateGrace)
	for time.Now().Before(deadline) && s.runningGroups() > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	s.signalGroups(syscall.SIGKILL)
	unix.Sync()
	if cfg != nil && cfg.FileSystemID != "" {
		if err := s.Home.Unmount(r.Context(), cfg); err != nil {
			s.log.Warn("unmount the home directory", "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// broadcast sends an event to every connected daemon.
func (s *Server) broadcast(ev Control) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) signalGroups(sig syscall.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pgid := range s.groups {
		_ = syscall.Kill(-pgid, sig)
	}
}

func (s *Server) runningGroups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.groups)
}

func (s *Server) track(pgid int, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if running {
		s.groups[pgid] = struct{}{}
	} else {
		delete(s.groups, pgid)
	}
}

// DataHandler serves the daemon.
func (s *Server) DataHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathPing, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET "+PathExec, s.serveExec)
	mux.HandleFunc("GET "+PathDial, s.serveDial)
	mux.HandleFunc("GET "+PathEvents, s.serveEvents)
	return s.authenticate(mux)
}

// authenticate admits the requests that present the secret of the instance.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.config()
		if cfg == nil {
			http.Error(w, "the instance is not configured yet", http.StatusServiceUnavailable)
			return
		}
		got := r.Header.Get(SecretHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(ReadLimit)
	return c, nil
}

// serveEvents keeps a connection open on which the events of the VM are sent.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	c, err := accept(w, r)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ch := make(chan Control, 4)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
	}()
	// The daemon never sends anything but pings, which CloseRead answers.
	ctx := c.CloseRead(r.Context())
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			if err := writeControl(ctx, c, ev); err != nil {
				return
			}
		}
	}
}

// serveDial connects the WebSocket to a TCP port of the VM.
func (s *Server) serveDial(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	target, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(r.Context(), "tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer target.Close()
	c, err := accept(w, r)
	if err != nil {
		return
	}
	defer c.CloseNow()
	conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, conn); closeWrite(target); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, target); done <- struct{}{} }()
	// The connection ends when either side is done: a WebSocket cannot be
	// half closed, so waiting for both could hang.
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// serveExec runs one command.
func (s *Server) serveExec(w http.ResponseWriter, r *http.Request) {
	c, err := accept(w, r)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()

	typ, msg, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return
	}
	var req ExecRequest
	if err := json.Unmarshal(msg, &req); err != nil || len(req.Argv) == 0 {
		_ = writeControl(ctx, c, Control{Type: ControlExit, Code: 1, Error: "invalid exec request"})
		c.Close(websocket.StatusPolicyViolation, "invalid exec request")
		return
	}
	cfg := s.config()
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = process.MergeEnv(process.MergeEnv(os.Environ(), cfg.Env), req.Env)
	cmd.Dir = req.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}

	var code int
	if req.TTY {
		code, err = s.runTerminal(ctx, c, cmd, req)
	} else {
		code, err = s.runPipes(ctx, c, cmd)
	}
	exit := Control{Type: ControlExit, Code: code}
	if err != nil {
		exit = Control{Type: ControlExit, Code: 1, Error: err.Error()}
	}
	if err := writeControl(ctx, c, exit); err != nil {
		return
	}
	c.Close(websocket.StatusNormalClosure, "")
}

// runPipes runs a command whose standard streams are separate pipes.
func (s *Server) runPipes(ctx context.Context, c *websocket.Conn, cmd *exec.Cmd) (int, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	cmd.Stdout = &streamWriter{ctx: ctx, c: c, stream: StreamStdout}
	cmd.Stderr = &streamWriter{ctx: ctx, c: c, stream: StreamStderr}
	// The command and its children form a process group, so that all of them
	// can be stopped together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	pgid := cmd.Process.Pid
	s.track(pgid, true)
	defer s.track(pgid, false)

	exited := make(chan struct{})
	go s.readInput(ctx, c, exited, pgid, stdin, nil)
	code, err := waitCode(cmd)
	close(exited)
	return code, err
}

// runTerminal runs a command on a pseudo terminal. The terminal is allocated
// from the devpts of the VM, so that the command can resolve it.
func (s *Server) runTerminal(ctx context.Context, c *websocket.Conn, cmd *exec.Cmd, req ExecRequest) (int, error) {
	term, err := tty.Open(os.Getpid())
	if err != nil {
		return 1, err
	}
	defer term.Close()
	if err := term.SetSize(req.Rows, req.Cols); err != nil {
		return 1, err
	}
	master := term.Master
	cmd.Stdin, cmd.Stdout, cmd.Stderr = term.Slave, term.Slave, term.Slave
	// The terminal becomes the controlling terminal of a new session, which is
	// what enables job control. The session is also a new process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	pgid := cmd.Process.Pid
	s.track(pgid, true)
	defer s.track(pgid, false)

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(&streamWriter{ctx: ctx, c: c, stream: StreamStdout}, master)
	}()
	exited := make(chan struct{})
	go s.readInput(ctx, c, exited, pgid, master, term)
	code, err := waitCode(cmd)
	close(exited)
	// Closing the slave lets the output pump see the end of the terminal once
	// the remaining output has been read.
	_ = term.CloseSlave()
	<-outputDone
	return code, err
}

// readInput forwards the messages of the daemon to the command: the standard
// input, the end of it and terminal size changes. When the daemon goes away
// while the command still runs, the command is killed, as a host process
// would be when its context is cancelled.
func (s *Server) readInput(ctx context.Context, c *websocket.Conn, exited <-chan struct{}, pgid int, stdin io.WriteCloser, term *tty.Terminal) {
	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			select {
			case <-exited:
			default:
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if len(msg) > 1 && msg[0] == StreamStdin {
				_, _ = stdin.Write(msg[1:])
			}
		case websocket.MessageText:
			var ctl Control
			if json.Unmarshal(msg, &ctl) != nil {
				continue
			}
			switch ctl.Type {
			case ControlEOF:
				if term == nil {
					_ = stdin.Close()
				} else {
					// A terminal has no end of input; ^D is what a user
					// types instead.
					_, _ = stdin.Write([]byte{4})
				}
			case ControlResize:
				if term != nil {
					_ = term.SetSize(ctl.Rows, ctl.Cols)
				}
			}
		}
	}
}

// waitCode waits for a command and returns its exit status.
func waitCode(cmd *exec.Cmd) (int, error) {
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// streamWriter sends what is written as messages of one stream.
type streamWriter struct {
	ctx    context.Context
	c      *websocket.Conn
	stream byte
}

func (w *streamWriter) Write(p []byte) (int, error) {
	return writeStream(w.ctx, w.c, w.stream, p)
}

// writeStream sends p as messages of a stream, in chunks of at most MaxChunk.
func writeStream(ctx context.Context, c *websocket.Conn, stream byte, p []byte) (int, error) {
	written := 0
	buf := make([]byte, 0, MaxChunk+1)
	for len(p) > 0 {
		n := min(len(p), MaxChunk)
		buf = append(buf[:0], stream)
		buf = append(buf, p[:n]...)
		if err := c.Write(ctx, websocket.MessageBinary, buf); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func writeControl(ctx context.Context, c *websocket.Conn, ctl Control) error {
	b, err := json.Marshal(ctl)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// EFSMounter mounts the home directory from an EFS access point with the EFS
// mount helper. IAM authorisation is used, so that the role of the VM decides
// which access points it may mount, and TLS is required by access points.
type EFSMounter struct{}

// mountTimeout keeps a hook within the time Lambda gives it.
const mountTimeout = 40 * time.Second

func (EFSMounter) Mount(ctx context.Context, cfg *RunConfig) error {
	ctx, cancel := context.WithTimeout(ctx, mountTimeout)
	defer cancel()
	home := homeOf(cfg)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	out, err := process.Output(ctx, "mount", "-t", "efs",
		"-o", "tls,iam,accesspoint="+cfg.AccessPointID,
		cfg.FileSystemID+":/", home)
	if err != nil {
		return fmt.Errorf("mount %s: %w: %s", cfg.AccessPointID, err, out)
	}
	startWatchdog()
	// A new home directory starts with the files of /etc/skel, as one made
	// by useradd would.
	entries, err := os.ReadDir(home)
	if err == nil && len(entries) == 0 {
		if out, err := process.Output(ctx, "cp", "-a", "/etc/skel/.", home); err != nil {
			return fmt.Errorf("populate the home directory: %w: %s", err, out)
		}
		// Copying "." also copies the mode of /etc/skel itself.
		if err := os.Chmod(home, 0o700); err != nil {
			return fmt.Errorf("populate the home directory: %w", err)
		}
	}
	return nil
}

// watchdog is the helper of the EFS mount helper that restarts the TLS proxy
// of a mount and renews its IAM certificate. The mount helper starts it
// through the init system, which it does not recognise in the VM, so the
// agent runs it.
const watchdog = "/usr/bin/amazon-efs-mount-watchdog"

var watchdogOnce sync.Once

func startWatchdog() {
	watchdogOnce.Do(func() {
		if _, err := os.Stat(watchdog); err != nil {
			return
		}
		go func() {
			// It runs for the life of the VM; a crash is followed by a
			// restart.
			for {
				cmd := exec.Command(watchdog)
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				_ = cmd.Run()
				time.Sleep(5 * time.Second)
			}
		}()
	})
}

func (EFSMounter) Check(ctx context.Context, cfg *RunConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// stat(1) on the mount point needs an answer from the server, unlike a
	// look at the mount table.
	if out, err := process.Output(ctx, "stat", "-f", homeOf(cfg)); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (EFSMounter) Unmount(ctx context.Context, cfg *RunConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := process.Output(ctx, "umount", homeOf(cfg)); err == nil {
		return nil
	}
	// A busy or unresponsive mount is detached instead.
	if out, err := process.Output(context.Background(), "umount", "-l", homeOf(cfg)); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func homeOf(cfg *RunConfig) string {
	if cfg.Home != "" {
		return cfg.Home
	}
	return "/root"
}
