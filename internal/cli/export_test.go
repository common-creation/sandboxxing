package cli

import (
	"io"
	"log/slog"

	"github.com/common-creation/sandboxxing/internal/vm"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var _ *vm.Manager
