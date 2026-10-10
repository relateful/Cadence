package analysis

import (
	"io"
	"log/slog"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
