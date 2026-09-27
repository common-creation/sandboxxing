// Port forwarding over an SSH session.
//
// Two directions are supported, matching the OpenSSH client:
//
//   - local forward (`ssh -L [bind:]port:host:hostport`): the client opens a
//     "direct-tcpip" channel and the daemon connects to the target. When the
//     target is a container port, naming the container in the SSH user makes
//     `ssh -L 8080:localhost:80 demo@host` reach the service inside "demo".
//
//   - remote forward (`ssh -R [bind:]port:host:hostport`): the client asks the
//     daemon to listen with a "tcpip-forward" request and the daemon opens a
//     "forwarded-tcpip" channel for every incoming connection. This is how a
//     process inside a container can expose a server to the host or, with
//     "GatewayPorts" style addresses, to the network.
package sshserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/state"
)

// directTCPIPPayload is the RFC 4254 section 7.2 message that a client sends
// when it asks for a connection to a target through the tunnel.
type directTCPIPPayload struct {
	Host       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

// forwardedTCPIPPayload is the same message in the other direction, sent by
// the daemon when a forwarded connection arrives.
type forwardedTCPIPPayload struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

// tcpipForwardPayload is the RFC 4254 section 7.1 message that requests a
// listening socket. A port of zero asks the daemon to pick one.
type tcpipForwardPayload struct {
	Addr string
	Port uint32
}

// cancelTCPIPForwardPayload answers a successful tcpip-forward request.
type cancelTCPIPForwardPayload struct {
	Addr string
	Port uint32
}

// forwardState tracks the listeners of one SSH connection so that they can be
// closed when the connection ends.
type forwardState struct {
	mu        sync.Mutex
	listeners map[string]net.Listener
}

// add registers a listener under the address the client asked for.
func (f *forwardState) add(key string, ln net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listeners == nil {
		f.listeners = map[string]net.Listener{}
	}
	f.listeners[key] = ln
}

// remove closes and forgets a listener.
func (f *forwardState) remove(key string) bool {
	f.mu.Lock()
	ln, ok := f.listeners[key]
	if ok {
		delete(f.listeners, key)
	}
	f.mu.Unlock()
	if ok {
		_ = ln.Close()
	}
	return ok
}

// closeAll closes every listener, which ends the goroutines that serve them.
func (f *forwardState) closeAll() {
	f.mu.Lock()
	listeners := f.listeners
	f.listeners = nil
	f.mu.Unlock()
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

// serveGlobalRequests handles the requests a client sends outside of any
// channel. Only the port forwarding requests are answered; the rest is
// declined so that a client does not hang waiting for a reply.
func (s *Server) serveGlobalRequests(ctx context.Context, conn *ssh.ServerConn, requests <-chan *ssh.Request, state *forwardState) {
	for req := range requests {
		switch req.Type {
		case "tcpip-forward":
			s.handleTCPIPForward(ctx, conn, req, state)
		case "cancel-tcpip-forward":
			s.handleCancelTCPIPForward(req, state)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	state.closeAll()
}

// handleTCPIPForward implements `ssh -R`.
func (s *Server) handleTCPIPForward(ctx context.Context, conn *ssh.ServerConn, req *ssh.Request, state *forwardState) {
	var payload tcpipForwardPayload
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
		s.log.Warn("malformed tcpip-forward request", "error", err)
		_ = req.Reply(false, nil)
		return
	}
	bindAddr, err := resolveBindAddress(payload.Addr)
	if err != nil {
		s.log.Warn("refusing a remote forward", "address", payload.Addr, "error", err)
		_ = req.Reply(false, nil)
		return
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, strconv.Itoa(int(payload.Port))))
	if err != nil {
		s.log.Warn("cannot listen for a remote forward",
			"address", payload.Addr, "port", payload.Port, "error", err)
		_ = req.Reply(false, nil)
		return
	}

	// A request with port 0 asks the daemon to choose one; the client learns
	// the number from the reply.
	actualPort := payload.Port
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		actualPort = uint32(tcp.Port)
	}
	key := net.JoinHostPort(payload.Addr, strconv.Itoa(int(actualPort)))
	state.add(key, ln)

	reply := struct{ Port uint32 }{actualPort}
	if req.WantReply {
		_ = req.Reply(true, ssh.Marshal(&reply))
	}
	s.log.Info("remote forward listening",
		"user", conn.User(), "bind", ln.Addr().String(), "requested", payload.Addr)

	go s.serveForwarded(conn, ln, payload.Addr, actualPort)
}

// handleCancelTCPIPForward implements the counterpart of `ssh -R` shutdown.
func (s *Server) handleCancelTCPIPForward(req *ssh.Request, state *forwardState) {
	var payload cancelTCPIPForwardPayload
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
		_ = req.Reply(false, nil)
		return
	}
	ok := state.remove(net.JoinHostPort(payload.Addr, strconv.Itoa(int(payload.Port))))
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// serveForwarded accepts connections on a forwarded listener and hands each
// one to the client over a "forwarded-tcpip" channel.
func (s *Server) serveForwarded(conn *ssh.ServerConn, ln net.Listener, requestAddr string, requestPort uint32) {
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		go s.forwardConnection(conn, local, requestAddr, requestPort)
	}
}

func (s *Server) forwardConnection(conn *ssh.ServerConn, local net.Conn, requestAddr string, requestPort uint32) {
	defer local.Close()

	originHost, originPort := splitHostPort(local.RemoteAddr())
	channel, requests, err := conn.OpenChannel("forwarded-tcpip", ssh.Marshal(&forwardedTCPIPPayload{
		Addr:       requestAddr,
		Port:       requestPort,
		OriginAddr: originHost,
		OriginPort: originPort,
	}))
	if err != nil {
		s.log.Warn("the client refused a forwarded connection", "error", err)
		return
	}
	go ssh.DiscardRequests(requests)
	bridge(channel, local)
}

// handleDirectTCPIP implements `ssh -L`.
func (s *Server) handleDirectTCPIP(ctx context.Context, conn *ssh.ServerConn, newChannel ssh.NewChannel) {
	var payload directTCPIPPayload
	if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "could not parse the direct-tcpip payload")
		return
	}
	target := net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port)))

	remote, err := s.dialTunnelTarget(ctx, conn.User(), payload.Host, uint32(payload.Port))
	if err != nil {
		s.log.Warn("local forward failed",
			"user", conn.User(), "target", target, "error", err)
		_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		remote.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	s.log.Info("local forward connected", "user", conn.User(), "target", target)
	bridge(channel, remote)
}

// dialTunnelTarget connects to the target of a local forward.
//
// A loopback target of a container session refers to the container itself,
// which is what a client writes for `ssh -L 8080:localhost:80 name@host`. The
// container is reachable over the bridge with the address the daemon assigned
// to it, so the connection is made from the host to that address. Every other
// target is a plain TCP connection from the host, which is what
// `ssh -L 5432:db.example.com:5432` needs.
func (s *Server) dialTunnelTarget(ctx context.Context, user, host string, port uint32) (net.Conn, error) {
	address := net.JoinHostPort(host, strconv.Itoa(int(port)))
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	name, isContainer := s.containerFor(user)
	if !isContainer || !isLoopbackHost(host) {
		return dialer.DialContext(ctx, "tcp", address)
	}

	vm, ok := s.state.Get(name)
	if !ok || vm.IP == "" {
		return nil, fmt.Errorf("container %q has no address yet", name)
	}
	if err := s.vms.EnsureStarted(ctx, name); err != nil {
		return nil, err
	}
	containerAddress := net.JoinHostPort(vm.IP, strconv.Itoa(int(port)))
	conn, err := dialer.DialContext(ctx, "tcp", containerAddress)
	if err != nil {
		return nil, fmt.Errorf("connect to %s in container %s: %w", address, name, err)
	}
	return conn, nil
}

// containerFor reports whether the SSH user names a container and returns its
// name.
func (s *Server) containerFor(user string) (string, bool) {
	if s.cfg.IsAdminUser(user) {
		return "", false
	}
	name := state.StripDomain(user)
	if !state.ValidName(name) {
		return "", false
	}
	if _, ok := s.state.Get(name); !ok {
		return "", false
	}
	return name, true
}

// isLoopbackHost reports whether a forward target refers to the machine the
// daemon runs on. For a container session that means the container itself,
// which is what a client writes for `ssh -L 8080:localhost:80 name@host`.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]", "":
		return true
	}
	return false
}

// resolveBindAddress turns the address of a forwarding request into something
// net.Listen accepts. A wildcard and a missing address bind to every
// interface, which is what OpenSSH does when GatewayPorts is enabled; the
// client decides with the address it asks for.
func resolveBindAddress(addr string) (string, error) {
	switch strings.ToLower(addr) {
	case "", "*", "0.0.0.0":
		return "0.0.0.0", nil
	case "localhost", "::1", "[::1]":
		return "127.0.0.1", nil
	}
	host := strings.Trim(addr, "[]")
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid bind address %q", addr)
	}
	return host, nil
}

// splitHostPort splits a net.Addr into an IP string and a port number for the
// SSH payloads, which carry them as separate fields.
func splitHostPort(addr net.Addr) (string, uint32) {
	host, portText, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "0.0.0.0", 0
	}
	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil {
		port = 0
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return host, uint32(port)
}

// bridge copies data between an SSH channel and a TCP connection until either
// side ends.
func bridge(channel ssh.Channel, conn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(channel, conn)
		// Half close: the peer may still have data to send.
		_ = channel.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, channel)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	wg.Wait()
	_ = channel.Close()
	_ = conn.Close()
}
