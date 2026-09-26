package sshserver

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// stubConn implements just enough of ssh.Conn to drive watchSession.
type stubConn struct {
	done chan struct{}
}

func (s *stubConn) Wait() error { <-s.done; return nil }
func (s *stubConn) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}
func (s *stubConn) User() string          { return "sandbox" }
func (s *stubConn) SessionID() []byte     { return nil }
func (s *stubConn) ClientVersion() []byte { return nil }
func (s *stubConn) ServerVersion() []byte { return nil }
func (s *stubConn) RemoteAddr() net.Addr  { return stubAddr{} }
func (s *stubConn) LocalAddr() net.Addr   { return stubAddr{} }
func (s *stubConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return false, nil, nil
}
func (s *stubConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, nil
}

type stubAddr struct{}

func (stubAddr) Network() string { return "stub" }
func (stubAddr) String() string  { return "stub" }

// TestWatchSessionCancelsOnDisconnect verifies that the work started by a
// session is cancelled when the SSH connection goes away: this is what makes
// Ctrl-C during `new` abort the process tree it started.
func TestWatchSessionCancelsOnDisconnect(t *testing.T) {
	srv := &Server{log: discardLogger()}
	conn := &stubConn{done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.watchSession(ctx, conn, cancel)
	}()

	select {
	case <-done:
		t.Fatal("the watch returned before the connection closed")
	case <-time.After(100 * time.Millisecond):
	}

	conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the disconnection was not noticed")
	}
	if ctx.Err() == nil {
		t.Fatal("the session context was not cancelled")
	}
}
