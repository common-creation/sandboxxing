// Command sandboxxing-agent runs inside a Lambda MicroVM. It serves the
// lifecycle hooks of the VM and lets the sandboxxing daemon run commands and
// open connections in it. See package agent for the protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/common-creation/sandboxxing/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sandboxxing-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("sandboxxing-agent", flag.ContinueOnError)
	var (
		dataAddr = fs.String("data-addr", ":"+strconv.Itoa(agent.DataPort), "listen address for the daemon")
		hookAddr = fs.String("hook-addr", ":"+strconv.Itoa(agent.HookPort), "listen address for the lifecycle hooks")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	// The output goes to CloudWatch Logs. Nothing a command prints is logged.
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	srv := agent.NewServer(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	servers := []*http.Server{
		{Addr: *hookAddr, Handler: srv.HookHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: *dataAddr, Handler: srv.DataHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		log.Info("listening", "addr", s.Addr)
		go func() { errs <- s.Serve(ln) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	return nil
}
