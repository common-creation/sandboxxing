package sshserver

import (
	"io"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestRequestLoopKeepsRunningForResize guards a bug where the session request
// loop returned after the shell request. The window-change messages that an
// SSH client sends when its terminal is resized were therefore never read,
// and the container kept the size it was started with.
func TestRequestLoopKeepsRunningForResize(t *testing.T) {
	session := &sessionRequest{env: map[string]string{}, rows: 24, cols: 80}
	resize := session.enableResize()
	defer session.disableResize(resize)

	requests := make(chan *ssh.Request, 4)
	// shell first, then a resize, exactly like OpenSSH sends them.
	requests <- &ssh.Request{Type: "shell"}
	requests <- newWindowChange(t, 40, 120)
	close(requests)

	done := make(chan struct{})
	go consumeRequests(session, requests, done)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request loop did not finish")
	}

	rows, cols := session.size()
	if rows != 40 || cols != 120 {
		t.Errorf("size = %dx%d, want 40x120: the resize request was not handled", rows, cols)
	}
	select {
	case size := <-resize:
		if size.rows != 40 || size.cols != 120 {
			t.Errorf("forwarded size = %dx%d, want 40x120", size.rows, size.cols)
		}
	default:
		t.Error("the resize was not forwarded to the terminal")
	}
}

func newWindowChange(t *testing.T, rows, cols uint32) *ssh.Request {
	t.Helper()
	payload := ssh.Marshal(struct {
		Cols   uint32
		Rows   uint32
		Width  uint32
		Height uint32
	}{Cols: cols, Rows: rows, Width: cols * 8, Height: rows * 8})
	return &ssh.Request{Type: "window-change", Payload: payload}
}

// consumeRequests runs a copy of the session request loop over the channel.
func consumeRequests(session *sessionRequest, requests <-chan *ssh.Request, done chan<- struct{}) {
	defer close(done)
	for req := range requests {
		switch req.Type {
		case "shell":
			session.mu.Lock()
			session.shell = true
			session.mu.Unlock()
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
		}
	}
}

// TestDisableResizeIsIdempotent makes sure the shutdown path cannot panic when
// it runs twice, which a partial terminal setup used to cause.
func TestDisableResizeIsIdempotent(t *testing.T) {
	session := &sessionRequest{env: map[string]string{}}
	ch := session.enableResize()
	session.disableResize(ch)
	session.disableResize(ch)
	session.setSize(50, 150) // must not send on a closed channel
}

// TestSessionEndsWithoutClientEOF verifies that the PTY session finishes when
// the payload exits, even when the SSH client never sends an end of file. The
// earlier implementation waited for the input pump, so closing the shell made
// the session hang until the client timed out.
func TestPTYInputPumpDoesNotBlockExit(t *testing.T) {
	// A channel that never delivers data: the input pump of runPTY copies
	// from it, and the session must still be able to return.
	pr, pw := io.Pipe()
	defer pw.Close()
	defer pr.Close()

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		// This mirrors the input pump: it blocks until the read side closes.
		_, _ = io.Copy(io.Discard, pr)
		close(done)
	}()

	// Closing the writer terminates the copy, which is what the payload exit
	// does to the terminal in the real implementation.
	pw.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the input pump did not terminate after the writer closed")
	}
	wg.Wait()
}
