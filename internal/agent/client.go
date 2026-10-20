package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// KeepaliveInterval is how often an idle connection is pinged. The MicroVM
// endpoint closes a WebSocket that carried nothing for a minute, which would
// end an idle login.
var KeepaliveInterval = 20 * time.Second

// keepalive pings conn until ctx ends or a ping fails. A ping only completes
// while the connection is being read, which every user of a connection does.
func keepalive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(KeepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, KeepaliveInterval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// Client connects the daemon to the agent of one VM.
type Client struct {
	// BaseURL is the endpoint of the VM, for example
	// "https://abc.lambda-microvm.us-east-1.on.aws".
	BaseURL string
	// Header returns the headers that authenticate a request: the token of
	// the MicroVM endpoint and the secret of the instance. It is called for
	// every connection so that expiring tokens can be renewed.
	Header func(ctx context.Context) (http.Header, error)
	// HTTPClient is used for the connections. It defaults to one that speaks
	// HTTP/1.1, which a WebSocket handshake needs.
	HTTPClient *http.Client
}

// http1Client negotiates HTTP/1.1 with the endpoint. The endpoint prefers
// HTTP/2, on which the WebSocket upgrade of the handshake does not exist.
var http1Client = &http.Client{Transport: &http.Transport{
	Proxy:             http.ProxyFromEnvironment,
	ForceAttemptHTTP2: false,
	TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	TLSClientConfig:   &tls.Config{NextProtos: []string{"http/1.1"}},
}}

func (c *Client) dial(ctx context.Context, path string, query url.Values) (*websocket.Conn, error) {
	header, err := c.Header(ctx)
	if err != nil {
		return nil, err
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	client := c.HTTPClient
	if client == nil {
		client = http1Client
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: client, HTTPHeader: header})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("connect to the agent: %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("connect to the agent: %w", err)
	}
	conn.SetReadLimit(ReadLimit)
	return conn, nil
}

// Ping checks that the agent answers.
func (c *Client) Ping(ctx context.Context) error {
	header, err := c.Header(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+PathPing, nil)
	if err != nil {
		return err
	}
	req.Header = header
	client := c.HTTPClient
	if client == nil {
		client = http1Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent answered %s", resp.Status)
	}
	return nil
}

// Dial connects to a TCP port of the VM.
func (c *Client) Dial(ctx context.Context, port int) (net.Conn, error) {
	conn, err := c.dial(ctx, PathDial, url.Values{"port": {strconv.Itoa(port)}})
	if err != nil {
		return nil, err
	}
	// The connection outlives the dial, so it is not bound to its context.
	connCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go keepalive(connCtx, conn)
	return &keptConn{Conn: websocket.NetConn(connCtx, conn, websocket.MessageBinary), stop: cancel}, nil
}

// keptConn stops the keepalive of a dialled connection when it is closed.
type keptConn struct {
	net.Conn
	stop context.CancelFunc
}

func (c *keptConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

// Events calls fn for each event of the VM until the connection ends or ctx
// is cancelled. Reading also answers the keepalive pings of the agent.
func (c *Client) Events(ctx context.Context, fn func(Control)) error {
	conn, err := c.dial(ctx, PathEvents, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	pingCtx, stop := context.WithCancel(ctx)
	defer stop()
	go keepalive(pingCtx, conn)
	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var ev Control
		if json.Unmarshal(msg, &ev) == nil {
			fn(ev)
		}
	}
}

// Session is a command running in the VM.
type Session struct {
	conn *websocket.Conn
	ctx  context.Context

	done chan struct{}
	code int
	err  error
	// output receives the terminal output of a TTY session.
	output *io.PipeReader

	writeMu sync.Mutex
	closed  bool
}

// Start runs a command in the VM. For a command without a terminal, stdin is
// copied to it and its output to stdout and stderr; nil streams are left
// unconnected. For a TTY command the streams are ignored and the Session is
// read from and written to instead.
func (c *Client) Start(ctx context.Context, req ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (*Session, error) {
	conn, err := c.dial(ctx, PathExec, nil)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("start the command: %w", err)
	}
	s := &Session{conn: conn, ctx: ctx, done: make(chan struct{})}
	if req.TTY {
		pr, pw := io.Pipe()
		s.output = pr
		stdout, stderr = pw, pw
		go func() {
			<-s.done
			pw.Close()
		}()
	}
	go s.readOutput(stdout, stderr)
	pingCtx, stop := context.WithCancel(ctx)
	go func() {
		<-s.done
		stop()
	}()
	go keepalive(pingCtx, conn)
	if stdin != nil && !req.TTY {
		go s.copyInput(stdin)
	}
	return s, nil
}

// readOutput dispatches the messages of the agent until the exit status
// arrives.
func (s *Session) readOutput(stdout, stderr io.Writer) {
	defer close(s.done)
	for {
		typ, msg, err := s.conn.Read(s.ctx)
		if err != nil {
			s.code, s.err = 1, fmt.Errorf("lost the connection to the command: %w", err)
			s.conn.CloseNow()
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if len(msg) < 1 {
				continue
			}
			var w io.Writer
			switch msg[0] {
			case StreamStdout:
				w = stdout
			case StreamStderr:
				w = stderr
			}
			if w != nil {
				_, _ = w.Write(msg[1:])
			}
		case websocket.MessageText:
			var ctl Control
			if json.Unmarshal(msg, &ctl) != nil || ctl.Type != ControlExit {
				continue
			}
			s.code = ctl.Code
			if ctl.Error != "" {
				s.code, s.err = 1, errors.New(ctl.Error)
			}
			s.conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

func (s *Session) copyInput(stdin io.Reader) {
	buf := make([]byte, MaxChunk)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			_ = s.control(Control{Type: ControlEOF})
			return
		}
	}
}

// Read returns the terminal output of a TTY session.
func (s *Session) Read(p []byte) (int, error) {
	if s.output == nil {
		return 0, io.EOF
	}
	return s.output.Read(p)
}

// Write sends standard input to the command.
func (s *Session) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return 0, net.ErrClosed
	}
	return writeStream(s.ctx, s.conn, StreamStdin, p)
}

// Resize changes the size of the terminal.
func (s *Session) Resize(rows, cols int) error {
	return s.control(Control{Type: ControlResize, Rows: rows, Cols: cols})
}

func (s *Session) control(ctl Control) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	return writeControl(s.ctx, s.conn, ctl)
}

// Wait waits for the command to exit and returns its exit status.
func (s *Session) Wait() (int, error) {
	<-s.done
	s.writeMu.Lock()
	s.closed = true
	s.writeMu.Unlock()
	return s.code, s.err
}

// Close ends the session. A command that still runs is killed by the agent.
func (s *Session) Close() error {
	s.writeMu.Lock()
	s.closed = true
	s.writeMu.Unlock()
	return s.conn.CloseNow()
}
