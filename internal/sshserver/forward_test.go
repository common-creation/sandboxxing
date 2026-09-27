package sshserver

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestResolveBindAddress covers the bind addresses a client may request for a
// remote forward. A wildcard must listen on every interface, and a loopback
// request must stay on the host.
func TestResolveBindAddress(t *testing.T) {
	cases := map[string]string{
		"":          "0.0.0.0",
		"*":         "0.0.0.0",
		"0.0.0.0":   "0.0.0.0",
		"localhost": "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"::1":       "127.0.0.1",
		"[::1]":     "127.0.0.1",
		"10.0.0.5":  "10.0.0.5",
	}
	for input, want := range cases {
		got, err := resolveBindAddress(input)
		if err != nil {
			t.Errorf("resolveBindAddress(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("resolveBindAddress(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := resolveBindAddress("not-an-address.example.com"); err == nil {
		t.Error("a host name that is not an address must be rejected")
	}
}

// TestIsLoopbackHost documents which targets mean "the container itself".
func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "[::1]", ""} {
		if !isLoopbackHost(host) {
			t.Errorf("%q should be treated as loopback", host)
		}
	}
	for _, host := range []string{"db.example.com", "10.0.0.5", "192.168.1.1"} {
		if isLoopbackHost(host) {
			t.Errorf("%q must not be treated as loopback", host)
		}
	}
}

// TestSplitHostPort feeds the SSH payloads, which carry the address and the
// port as separate fields.
func TestSplitHostPort(t *testing.T) {
	host, port := splitHostPort(&net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8080})
	if host != "10.0.0.5" || port != 8080 {
		t.Errorf("splitHostPort = %s, %d", host, port)
	}
	host, port = splitHostPort(&net.TCPAddr{IP: net.IPv4zero, Port: 1234})
	if host != "0.0.0.0" || port != 1234 {
		t.Errorf("wildcard = %s, %d", host, port)
	}
}

// TestForwardStateTracksListeners keeps the cancellation path honest.
func TestForwardStateTracksListeners(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	state := &forwardState{}
	state.add("127.0.0.1:1234", ln)
	if !state.remove("127.0.0.1:1234") {
		t.Error("the listener should have been found")
	}
	if state.remove("127.0.0.1:1234") {
		t.Error("removing twice must report that it is gone")
	}
	if _, err := ln.Accept(); err == nil {
		t.Error("the listener should be closed after removal")
	}
}

// TestForwardStateCloseAll makes the connection teardown release every socket.
func TestForwardStateCloseAll(t *testing.T) {
	state := &forwardState{}
	var listeners []net.Listener
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		state.add(ln.Addr().String(), ln)
	}
	state.closeAll()
	for _, ln := range listeners {
		if _, err := ln.Accept(); err == nil {
			t.Error("a listener is still open after closeAll")
		}
	}
}

// TestRemoteForwardWithClient drives `ssh -R` against a running server: the
// SSH client asks for a listener and every connection to it must be handed to
// the client over a forwarded channel.
func TestRemoteForwardWithClient(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	listener, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the remote forward request failed: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	// Connect from the daemon side, which is where the listener lives.
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("cannot reach the forwarded listener: %v", err)
	}
	defer conn.Close()

	select {
	case server := <-accepted:
		defer server.Close()
		// The client side received the connection: the forward works.
		if _, err := conn.Write([]byte("hello\n")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 6)
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(server, buf); err != nil {
			t.Fatalf("the forwarded payload did not arrive: %v", err)
		}
		if string(buf) != "hello\n" {
			t.Errorf("payload = %q", buf)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no forwarded connection reached the client")
	}
}

// TestLocalForwardToHost drives `ssh -L` against a server: the client opens a
// direct-tcpip channel and the daemon connects to the target from the host.
func TestLocalForwardToHost(t *testing.T) {
	_, addr := newTestServer(t)

	// A stand-in for the service the client wants to reach.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("from-target\n"))
	}()

	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn, err := client.Dial("tcp", target.Addr().String())
	if err != nil {
		t.Fatalf("the local forward failed: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len("from-target\n"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("no data arrived through the tunnel: %v", err)
	}
	if string(buf) != "from-target\n" {
		t.Errorf("payload = %q", buf)
	}
}
