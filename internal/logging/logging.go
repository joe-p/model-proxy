package logging

import (
	"context"
	"io"
	"log/slog"
)

// Sink receives serialized JSON access records. Implementations must not block
// the caller.
type Sink interface {
	io.Writer
}

// New builds a logger with the configured format and level. Optional sinks
// receive only access records (msg == "request"), always as JSON lines.
func New(format string, level slog.Level, w io.Writer, sinks ...Sink) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}

	handlers := []slog.Handler{handler}
	for _, sink := range sinks {
		if sink != nil {
			handlers = append(handlers, &accessHandler{next: slog.NewJSONHandler(sink, opts)})
		}
	}
	if len(handlers) == 1 {
		return slog.New(handlers[0])
	}
	return slog.New(&multiHandler{handlers: handlers})
}

// multiHandler fans a record out to every handler. Handler errors are ignored
// because stdout logging must not fail a request when a secondary sink is down.
type multiHandler struct {
	handlers []slog.Handler
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			_ = handler.Handle(ctx, r)
		}
	}
	return nil
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		next[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		next[i] = handler.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

// accessHandler forwards only request records to the secondary sink, keeping
// startup, reload, and internal diagnostic logs on stdout alone.
type accessHandler struct {
	next slog.Handler
}

func (h *accessHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *accessHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message != "request" {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *accessHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &accessHandler{next: h.next.WithAttrs(attrs)}
}

func (h *accessHandler) WithGroup(name string) slog.Handler {
	return &accessHandler{next: h.next.WithGroup(name)}
}
