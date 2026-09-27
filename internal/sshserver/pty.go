package sshserver

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/vm"
)

// runPTY runs a command inside the container on a pseudo terminal and bridges
// it to the SSH channel.
//
// The terminal is allocated from the container's own devpts instance, not from
// the host, so that the pts node is resolvable inside the container and
// programs such as `tty` work. The master therefore carries the data and the
// slave is handed to the process that runs in the container.
func (s *Server) runPTY(ctx context.Context, name string, argv, env []string, dir string, ch ssh.Channel, session *sessionRequest) (int, error) {
	term, err := s.vms.Terminal(ctx, name)
	if err != nil {
		return 1, err
	}
	defer term.Close()

	rows, cols := session.size()
	if err := term.SetSize(rows, cols); err != nil {
		return 1, err
	}
	// The descriptor is captured before the goroutines start so that they
	// never read the field while Close clears it.
	master := term.Master

	resize := session.enableResize()
	resizeStopped := false
	stopResize := func() {
		if !resizeStopped {
			resizeStopped = true
			session.disableResize(resize)
		}
	}
	defer stopResize()

	cmd, err := s.vms.StartProcess(ctx, name, vm.Exec{
		Argv:   argv,
		Env:    env,
		Dir:    dir,
		Stdin:  term.Slave,
		Stdout: term.Slave,
		Stderr: term.Slave,
		PTY:    true,
	})
	if err != nil {
		return 1, err
	}

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(ch, master)
		// Tell the client that no more data follows. The exit status is sent
		// separately, after this function returns.
		_ = ch.CloseWrite()
	}()

	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		for size := range resize {
			_ = term.SetSize(size.rows, size.cols)
		}
	}()

	// The input pump forwards keystrokes to the container. It is not awaited
	// below: its read on the SSH channel only returns when the client closes
	// it, and an SSH client is not required to do so after the shell exits.
	go func() {
		_, _ = io.Copy(master, ch)
	}()

	waitErr := cmd.Wait()
	// Closing the container side releases the terminal: the output pump sees
	// the end of file and the input pump fails on its next write.
	_ = term.CloseSlave()
	<-outputDone

	stopResize()
	<-resizeDone

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, waitErr
	}
	return 0, nil
}

// runPipe runs a command without a terminal and forwards the standard streams
// through pipes so that stdout and stderr remain separated.
func (s *Server) runPipe(ctx context.Context, name string, argv, env []string, dir string, ch ssh.Channel) {
	inR, inW, err := os.Pipe()
	if err != nil {
		s.fail(ch, err)
		return
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		s.fail(ch, err)
		return
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		s.fail(ch, err)
		return
	}

	exit := make(chan int, 1)
	go func() {
		code, err := s.vms.Run(ctx, name, vm.Exec{Argv: argv, Env: env, Dir: dir, Stdin: inR, Stdout: outW, Stderr: errW})
		outW.Close()
		errW.Close()
		inR.Close()
		if err != nil {
			s.fail(ch, err)
			exit <- 1
			return
		}
		exit <- code
	}()

	go func() {
		_, _ = io.Copy(inW, ch)
		inW.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, outR)
		outR.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch.Stderr(), errR)
		errR.Close()
	}()
	code := <-exit
	wg.Wait()
	s.exitStatus(ch, code)
}

// stdio adapts an SSH channel to the io.Reader used by the interactive
// console of the control sessions.
type stdio struct {
	ch ssh.Channel
}

func (s *stdio) Read(p []byte) (int, error) { return s.ch.Read(p) }
