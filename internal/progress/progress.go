// Package progress carries the output streams of a long running operation
// from the SSH session down to the helper that produces the output.
//
// Long running commands such as `new` build an image, create a disk image and
// boot a container. Their output belongs to the client that started them, not
// to the daemon log, so it is passed through the context.
package progress

import (
	"context"
	"fmt"
	"io"
)

type contextKey struct{}

// Sink collects the output of one operation. A nil stream discards the data,
// which makes a zero Sink safe to use.
type Sink struct {
	out io.Writer
	err io.Writer
}

// New creates a sink that writes operation results to out and progress
// messages, including the output of the child processes, to err.
func New(out, err io.Writer) *Sink {
	return &Sink{out: out, err: err}
}

// With attaches the sink to the context.
func With(ctx context.Context, s *Sink) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// From returns the sink stored in the context. A discarded sink is returned
// when the context carries none, so callers never need to check for nil.
func From(ctx context.Context) *Sink {
	if s, ok := ctx.Value(contextKey{}).(*Sink); ok && s != nil {
		return s
	}
	return &Sink{}
}

// Out returns the stream for the result of an operation.
func (s *Sink) Out() io.Writer { return orDiscard(s.out) }

// Err returns the stream for progress messages. It never returns nil.
func (s *Sink) Err() io.Writer { return orDiscard(s.err) }

// Step reports one step of a long running operation. Steps are written to the
// error stream so that the standard output of a command keeps carrying only
// the requested result, such as a container name or JSON.
func (s *Sink) Step(format string, args ...any) {
	fmt.Fprintf(s.Err(), format+"\n", args...)
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
