package backend

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/process"
	"golang.org/x/sys/unix"
)

// dialSSM uses the Session Manager plugin in its stdio forwarding mode.
// The daemon can be outside the VPC. No local listening port is allocated.
func dialSSM(ctx context.Context, region, instanceID string) (net.Conn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "ssm-client")
	child := os.NewFile(uintptr(fds[1]), "ssm-plugin")
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		child.Close()
		return nil, err
	}
	// The resulting connection must outlive its dial context, like TCP.
	runCtx, cancel := context.WithCancel(context.Background())
	cmd := process.Command(runCtx, "aws", "--region", region, "ssm", "start-session",
		"--target", instanceID, "--document-name", "AWS-StartSSHSession",
		"--parameters", "portNumber="+strconv.Itoa(agent.DataPort))
	cmd.Stdin, cmd.Stdout = child, child
	if err := cmd.Start(); err != nil {
		cancel()
		conn.Close()
		child.Close()
		return nil, fmt.Errorf("start the Session Manager tunnel: %w", err)
	}
	// Release the child's socket at once; a failed CLI must result in EOF.
	_ = child.Close()
	go func() { _ = cmd.Wait(); cancel() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	id, err := readSSMBanner(reader)
	stop()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("open Session Manager tunnel for %s (check CLI, plugin and instance connectivity): %w", instanceID, err)
	}
	return &ssmConn{Conn: conn, reader: reader, release: func() {
		cancel()
		// Closing the local plugin alone can leave the SSM session on the
		// service until its idle timeout.
		go func() {
			cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			_ = process.Command(cleanup, "aws", "--region", region, "ssm", "terminate-session", "--session-id", id).Run()
		}()
	}}, nil
}

// AWS writes an empty line and a session announcement to stdout before it
// forwards data. Leave any subsequent buffered protocol bytes untouched.
func readSSMBanner(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice('\n')
	if err == nil && strings.TrimSpace(string(line)) == "" {
		line, err = r.ReadSlice('\n')
	}
	if err != nil {
		return "", err
	}
	id, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "Starting session with SessionId: ")
	if !ok || id == "" || strings.HasPrefix(id, "-") || strings.ContainsAny(id, " \t\r\n") {
		return "", errors.New("unexpected response from the Session Manager plugin")
	}
	return id, nil
}

type ssmConn struct {
	net.Conn
	reader  *bufio.Reader
	release func()
	once    sync.Once
}

func (c *ssmConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *ssmConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
