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
	pricing   CostEstimator
	logger    *slog.Logger
}

type stateKey struct{}
type logRecordKey struct{}

const (
	// modelDiscoveryTimeout bounds a single upstream model listing.
	modelDiscoveryTimeout = 10 * time.Second
	// modelDiscoveryTTL is how long discovered model ids are reused before the
	// upstream is asked again. Failures are cached for the same period so a
	// broken upstream is not probed on every /v1/models call.
	modelDiscoveryTTL = time.Minute
)

type discoveredModels struct {
	ids     []string
	expires time.Time
}

// modelsCache holds the upstream model ids reported by passthrough providers.
// It is keyed by provider name and cleared whenever the configuration reloads.
type modelsCache struct {
	mu      sync.Mutex
	entries map[string]discoveredModels
}

func (c *modelsCache) get(name string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[name]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.ids, true
}

func (c *modelsCache) put(name string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]discoveredModels)
	}
	c.entries[name] = discoveredModels{ids: ids, expires: time.Now().Add(modelDiscoveryTTL)}
}

func (c *modelsCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

type Server struct {
	state      atomic.Pointer[state]
	mux        *http.ServeMux
	httpServer *http.Server
	startedAt  time.Time
	reloadMu   sync.Mutex
	models     modelsCache
}

// passthroughModels returns the upstream model ids a passthrough provider
// advertises, consulting the cache first.
func (s *Server) passthroughModels(ctx context.Context, st *state, providerName string) []string {
	if ids, ok := s.models.get(providerName); ok {
		return ids
	}

	lister, ok := st.providers[providerName].(provider.ModelLister)
	if !ok {
		st.logger.Warn("model discovery is not supported by provider", "provider", providerName)
		s.models.put(providerName, nil)
		return nil
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, modelDiscoveryTimeout)
	defer cancel()
	ids, err := lister.Models(discoveryCtx)
	if err != nil {
		st.logger.Warn("model discovery failed", "provider", providerName, "error", err)
		s.models.put(providerName, nil)
		return nil
	}
	st.logger.Debug("model discovery succeeded", "provider", providerName, "models", len(ids))
	s.models.put(providerName, ids)
	return ids
}

func New(cfg *config.Config, providers map[string]provider.Provider, logger *slog.Logger, pricingSvc CostEstimator) *Server {
	if providers == nil {
		providers = map[string]provider.Provider{}
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{startedAt: time.Now()}
	s.state.Store(&state{cfg: cfg, providers: providers, pricing: pricingSvc, logger: logger})

	s.mux = http.NewServeMux()
	// OpenRouter clients often carry the https://openrouter.ai/api/v1 base URL,
	// so the same handlers serve /api/v1/* as aliases.
	for _, prefix := range []string{"/v1", "/api/v1"} {
		s.mux.HandleFunc(prefix+"/chat/completions", s.handleChatCompletions)
		s.mux.HandleFunc(prefix+"/responses", s.handleResponses)
		s.mux.HandleFunc(prefix+"/models", s.handleModels)
	}
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

// applyCORSHeaders exposes the proxy to browser-based OpenRouter clients.
func applyCORSHeaders(header http.Header) {
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Expose-Headers", "X-Request-ID, X-Model-Proxy-Alias, X-Model-Proxy-Upstream")
	header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Request-ID, HTTP-Referer, X-Title")
	header.Set("Access-Control-Max-Age", "600")
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
func (s *Server) Reload(cfg *config.Config, providers map[string]provider.Provider, logger *slog.Logger, pricingSvc CostEstimator) error {
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
	s.models.clear()
	s.state.Store(&state{cfg: cfg, providers: providers, pricing: pricingSvc, logger: logger})
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	lr := &requestLog{
		requestID:  requestIDFrom(r),
		method:     r.Method,
		path:       r.URL.Path,
		remoteAddr: r.RemoteAddr,
		userAgent:  r.UserAgent(),
	}

	w.Header().Set("X-Request-ID", lr.requestID)
	applyCORSHeaders(w.Header())
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

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
	if lr.userAgent != "" {
		attrs = append(attrs, "user_agent", lr.userAgent)
	}
	if lr.keyName != "" {
		attrs = append(attrs, "key_name", lr.keyName)
	}
	if lr.policyName != "" {
		attrs = append(attrs, "policy", lr.policyName)
	}
	if lr.policyMode != "" {
		attrs = append(attrs, "policy_mode", lr.policyMode)
	}
	if len(lr.policyModels) > 0 {
		attrs = append(attrs, "policy_models", lr.policyModels)
	}
	if lr.alias != "" {
		attrs = append(attrs, "alias", lr.alias)
	}
	if lr.provider != "" {
		attrs = append(attrs, "provider", lr.provider)
	}
	if lr.billingMode != "" {
		attrs = append(attrs, "billing_mode", lr.billingMode)
	}
	if lr.upstream != "" {
		attrs = append(attrs, "upstream_model", lr.upstream)
	}
	if lr.passthrough {
		attrs = append(attrs, "passthrough", true)
	}
	if len(lr.droppedFields) > 0 {
		attrs = append(attrs, "dropped_fields", lr.droppedFields)
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
		if lr.estimatedCost != nil {
			attrs = append(attrs, "estimated_api_cost_usd", *lr.estimatedCost)
		}
		if lr.pricingSource != "" {
			attrs = append(attrs, "pricing_source", lr.pricingSource)
		}
		if lr.usage.CacheDiscount != nil {
			attrs = append(attrs, "cache_discount", *lr.usage.CacheDiscount)
		}
		if details := lr.usage.PromptTokensDetails; details != nil {
			if details.CachedTokens != nil {
				attrs = append(attrs, "cached_tokens", *details.CachedTokens)
			}
			if details.CacheWriteTokens != nil {
				attrs = append(attrs, "cache_write_tokens", *details.CacheWriteTokens)
			}
		}
		if details := lr.usage.CompletionTokensDetails; details != nil {
			if details.ReasoningTokens != nil {
				attrs = append(attrs, "reasoning_tokens", *details.ReasoningTokens)
			}
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
