package progress

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestFromWithoutSinkIsSafe(t *testing.T) {
	s := From(context.Background())
	s.Step("ignored %d", 1)
	if _, err := s.Out().Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	s.Err().Write([]byte("x"))
}

func TestSinkRoutesStreams(t *testing.T) {
	var out, errBuf bytes.Buffer
	ctx := With(context.Background(), New(&out, &errBuf))
	s := From(ctx)
	s.Step("step %d", 2)
	s.Out().Write([]byte("result\n"))
	s.Err().Write([]byte("pacman output\n"))

	if got := out.String(); got != "result\n" {
		t.Errorf("stdout = %q", got)
	}
	want := "step 2\npacman output\n"
	if got := errBuf.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if strings.Contains(out.String(), "step") {
		t.Error("steps must not pollute stdout")
	}
}
