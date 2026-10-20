package sshserver

import (
	"context"
	"io"
	"os"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/vm"
)

// runPTY runs a command inside the container on a pseudo terminal and bridges
// it to the SSH channel.
//
// The backend allocates the terminal where the command runs, so that the
// terminal is resolvable inside the container and programs such as `tty`
// work. The SSH channel is connected to the other side of the terminal.
func (s *Server) runPTY(ctx context.Context, name string, argv, env []string, dir string, ch ssh.Channel, session *sessionRequest) (int, error) {
	rows, cols := session.size()
	term, err := s.vms.StartTerminal(ctx, name, vm.Exec{
		Argv: argv,
		Env:  env,
		Dir:  dir,
	}, rows, cols)
	if err != nil {
		return 1, err
	}
	defer term.Close()

	resize := session.enableResize()
	resizeStopped := false
	stopResize := func() {
		if !resizeStopped {
			resizeStopped = true
			session.disableResize(resize)
		}
	}
	defer stopResize()

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(ch, term)
		// Tell the client that no more data follows. The exit status is sent
		// separately, after this function returns.
		_ = ch.CloseWrite()
	}()

	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		for size := range resize {
			_ = term.Resize(size.rows, size.cols)
		}
	}()

	// The input pump forwards keystrokes to the container. It is not awaited
	// below: its read on the SSH channel only returns when the client closes
	// it, and an SSH client is not required to do so after the shell exits.
	go func() {
		_, _ = io.Copy(term, ch)
	}()

	// Waiting releases the container side of the terminal: the output pump
	// sees the end of file and the input pump fails on its next write.
	code, waitErr := term.Wait()
	<-outputDone

	stopResize()
	<-resizeDone

	if waitErr != nil {
		return 1, waitErr
	}
	return code, nil
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
