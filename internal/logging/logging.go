package logging

import (
	"io"
	"log/slog"
)

// New builds a logger with the configured format and level.
func New(format string, level slog.Level, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}
