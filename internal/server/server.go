package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
)

type state struct {
	cfg       *config.Config
	providers map[string]provider.Provider
	logger    *slog.Logger
}

type stateKey struct{}
type logRecordKey struct{}

type Server struct {
	state      atomic.Pointer[state]
	mux        *http.ServeMux
	httpServer *http.Server
	startedAt  time.Time
	reloadMu   sync.Mutex
}

func New(cfg *config.Config, providers map[string]provider.Provider, logger *slog.Logger) *Server {
	if providers == nil {
		providers = map[string]provider.Provider{}
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{startedAt: time.Now()}
	s.state.Store(&state{cfg: cfg, providers: providers, logger: logger})

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/", s.handleNotFound)

	s.httpServer = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           s,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	return s
}

func (s *Server) ListenAndServe() error {
	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) ShutdownTimeout() time.Duration {
	return s.state.Load().cfg.Server.ShutdownTimeout
}

// Reload atomically swaps the configuration, providers, and logger. Server
// settings tied to the already-running listener cannot be changed without a
// restart.
func (s *Server) Reload(cfg *config.Config, providers map[string]provider.Provider, logger *slog.Logger) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	old := s.state.Load()
	if cfg.Server.Listen != old.cfg.Server.Listen {
		return fmt.Errorf("server.listen changes require a restart")
	}
	if cfg.Server.ReadHeaderTimeout != old.cfg.Server.ReadHeaderTimeout {
		return fmt.Errorf("server.read_header_timeout changes require a restart")
	}
	if providers == nil {
		providers = map[string]provider.Provider{}
	}
	if logger == nil {
		logger = old.logger
	}
	s.state.Store(&state{cfg: cfg, providers: providers, logger: logger})
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	lr := &requestLog{
		requestID:  requestIDFrom(r),
		method:     r.Method,
		path:       r.URL.Path,
		remoteAddr: r.RemoteAddr,
	}

	w.Header().Set("X-Request-ID", lr.requestID)

	ctx := context.WithValue(r.Context(), stateKey{}, st)
	ctx = context.WithValue(ctx, logRecordKey{}, lr)
	r = r.WithContext(ctx)

	rec := &responseRecorder{ResponseWriter: w}
	start := time.Now()
	s.mux.ServeHTTP(rec, r)

	status := rec.status
	if status == 0 {
		if r.Context().Err() != nil {
			status = 499 // conventional client-closed-request status
			lr.clientCanceled = true
		} else {
			status = http.StatusOK
		}
	}

	attrs := []any{
		"request_id", lr.requestID,
		"method", lr.method,
		"path", lr.path,
		"status", status,
		"duration_ms", float64(time.Since(start).Microseconds()) / 1000.0,
		"bytes_out", rec.bytes,
	}
	if lr.remoteAddr != "" {
		attrs = append(attrs, "remote_addr", lr.remoteAddr)
	}
	if lr.keyName != "" {
		attrs = append(attrs, "key_name", lr.keyName)
	}
	if lr.alias != "" {
		attrs = append(attrs, "alias", lr.alias)
	}
	if lr.provider != "" {
		attrs = append(attrs, "provider", lr.provider)
	}
	if lr.upstream != "" {
		attrs = append(attrs, "upstream_model", lr.upstream)
	}
	if lr.stream {
		attrs = append(attrs, "stream", true)
	}
	if lr.errorCode != "" {
		attrs = append(attrs, "error_code", lr.errorCode)
	}
	if lr.errorDetail != "" {
		attrs = append(attrs, "error", lr.errorDetail)
	}
	if lr.clientCanceled {
		attrs = append(attrs, "client_canceled", true)
	}
	if lr.usage != nil {
		attrs = append(attrs,
			"prompt_tokens", lr.usage.PromptTokens,
			"completion_tokens", lr.usage.CompletionTokens,
			"total_tokens", lr.usage.TotalTokens,
		)
		if lr.usage.Cost != nil {
			attrs = append(attrs, "cost_usd", *lr.usage.Cost)
		}
	}

	level := slog.LevelInfo
	if lr.path == "/healthz" {
		level = slog.LevelDebug
	}
	st.logger.Log(r.Context(), level, "request", attrs...)
}

func stateFrom(ctx context.Context) *state {
	st, _ := ctx.Value(stateKey{}).(*state)
	return st
}

func logRecordFrom(ctx context.Context) *requestLog {
	lr, _ := ctx.Value(logRecordKey{}).(*requestLog)
	return lr
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status != 0 {
		return
	}
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
