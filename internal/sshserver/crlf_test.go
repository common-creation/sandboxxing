package sshserver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestCRLFWriter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare line feeds", "a\nb\n", "a\r\nb\r\n"},
		{"existing CRLF is kept", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"mixed line endings", "a\r\nb\n", "a\r\nb\r\n"},
		{"only a line feed", "\n", "\r\n"},
		{"no line feed", "abc", "abc"},
		{"empty write", "", ""},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		w := newCRLFWriter(&buf)
		n, err := w.Write([]byte(tc.in))
		if err != nil {
			t.Errorf("%s: Write: %v", tc.name, err)
			continue
		}
		if n != len(tc.in) {
			t.Errorf("%s: Write = %d, want %d", tc.name, n, len(tc.in))
		}
		if got := buf.String(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCRLFWriterSplitPair covers the echo of the line reader, which writes
// the carriage return and the line feed of an entered line in one call but
// must not be doubled when a writer splits the pair over two calls.
func TestCRLFWriterSplitPair(t *testing.T) {
	var buf bytes.Buffer
	w := newCRLFWriter(&buf)
	if _, err := w.Write([]byte("line\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "line\r\n" {
		t.Errorf("got %q, want %q", got, "line\r\n")
	}
}

// shortWriter accepts at most one byte per call and reports a partial write
// without an error, which the io.Writer contract allows.
type shortWriter struct{ buf bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		return w.buf.Write(p[:1])
	}
	return w.buf.Write(p)
}

// TestCRLFWriterShortWrite verifies that a partial write is reported as an
// error instead of being mistaken for a complete conversion.
func TestCRLFWriterShortWrite(t *testing.T) {
	w := newCRLFWriter(&shortWriter{})
	if _, err := w.Write([]byte("abc\ndef")); err != io.ErrShortWrite {
		t.Errorf("err = %v, want %v", err, io.ErrShortWrite)
	}
}

// runCommandWithPTY runs a command in a session that requested a pseudo
// terminal, exactly like an interactive ssh client.
func runCommandWithPTY(t *testing.T, client *ssh.Client, command string) commandResult {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RequestPty("xterm", 24, 200, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run(command)
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			result.code = exitErr.ExitStatus()
			return result
		}
		t.Fatalf("run %q: %v", command, err)
	}
	return result
}

// TestInteractiveControlSessionUsesCRLF guards the display of a control
// session. The session has no pseudo terminal, so the server must produce the
// CRLF pairs itself: with bare line feeds an interactive terminal advances
// the line without returning the carriage, and every line would start where
// the previous one ended.
func TestInteractiveControlSessionUsesCRLF(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	got := runCommandWithPTY(t, client, "ls")
	if got.code != 0 {
		t.Fatalf("ls = %+v", got)
	}
	if got.stdout != "no containers\r\n" {
		t.Errorf("stdout = %q, want %q", got.stdout, "no containers\r\n")
	}

	// The error path of the console must use the same convention.
	got = runCommandWithPTY(t, client, "'unterminated")
	if got.code == 0 {
		t.Errorf("an unterminated quote must fail: %+v", got)
	}
	if !strings.Contains(got.stdout, "unterminated quote in command\r\n") {
		t.Errorf("the failure does not end with CRLF: %q", got.stdout)
	}
}

// TestNonInteractiveControlSessionKeepsLF makes sure the conversion stays
// limited to interactive sessions: an exec session streams to a pipe or a
// file, where a carriage return would be unexpected.
func TestNonInteractiveControlSessionKeepsLF(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	got := runCommand(t, client, "ls")
	if got.code != 0 {
		t.Fatalf("ls = %+v", got)
	}
	if strings.Contains(got.stdout, "\r") {
		t.Errorf("stdout = %q, want plain line feeds", got.stdout)
	}
}

// TestInteractiveShellEchoUsesCRLF drives the console of an interactive
// session: the banner, the echoed prompt and the command output must all end
// their lines with CRLF, and the CRLF the line reader writes itself must not
// be doubled.
func TestInteractiveShellEchoUsesCRLF(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RequestPty("xterm", 24, 200, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	session.Stdout = &out
	session.Stderr = &out

	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	// Both lines are sent up front; the session ends after `exit` and Wait
	// returns when the output is complete.
	if _, err := fmt.Fprint(stdin, "ls\nexit\n"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interactive session did not finish")
	}

	text := out.String()
	for _, want := range []string{
		"sandboxxing console on test-host\r\n",
		"Type 'help' for the command list, 'exit' to quit.\r\n",
		"sandboxxing> ls\r\n",
		"no containers\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output is missing %q:\n%q", want, text)
		}
	}
	if strings.Contains(text, "\r\r") {
		t.Errorf("a CRLF was doubled:\n%q", text)
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Errorf("a bare line feed reached the client:\n%q", text)
	}
}
