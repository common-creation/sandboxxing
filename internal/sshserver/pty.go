package sshserver

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/vm"
)

// runPTY runs a command inside the container on a pseudo terminal and bridges
// it to the SSH channel. The PTY is allocated on the host and passed to
// nsenter(1), which places the already opened terminal inside the container.
func (s *Server) runPTY(ctx context.Context, name string, argv []string, ch ssh.Channel, session *sessionRequest) (int, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return 1, err
	}
	defer ptmx.Close()
	rows, cols := session.size()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		tty.Close()
		return 1, err
	}

	resize := session.enableResize()
	defer session.disableResize(resize)

	cmd, err := s.vms.StartProcess(ctx, name, vm.Exec{
		Argv:   argv,
		Stdin:  tty,
		Stdout: tty,
		Stderr: tty,
		PTY:    true,
	})
	if err != nil {
		tty.Close()
		return 1, err
	}
	tty.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ptmx, ch)
	}()
	go func() {
		for size := range resize {
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(size.rows), Cols: uint16(size.cols)})
		}
	}()

	_, _ = io.Copy(ch, ptmx)
	ptmx.Close()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// runPipe runs a command without a terminal and forwards the standard streams
// through pipes so that stdout and stderr remain separated.
func (s *Server) runPipe(ctx context.Context, name string, argv []string, ch ssh.Channel) {
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
		code, err := s.vms.Run(ctx, name, vm.Exec{Argv: argv, Stdin: inR, Stdout: outW, Stderr: errW})
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
	ch   ssh.Channel
	rows int
	cols int
}

func (s *stdio) Read(p []byte) (int, error) { return s.ch.Read(p) }
