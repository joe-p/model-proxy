package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/joe-p/model-proxy/internal/auth"
	"github.com/joe-p/model-proxy/internal/claudesub"
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
)

const (
	// claudeProxyKeyHeader is the virtual-key header used by /claude_sub/.
	// Authorization is reserved for the client's Claude subscription token.
	claudeProxyKeyHeader = "X-Model-Proxy-Key"
	// claudeSubPath is the path prefix stripped before forwarding upstream.
	claudeSubPath = "/claude_sub"
)

// handleClaudeSub transparently proxies Anthropic's Messages API using the
// client's own Claude Code subscription credential. Request and response bodies
// are forwarded unchanged; the proxy only observes usage for its access log and
// API-equivalent cost estimate.
func (s *Server) handleClaudeSub(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	st := stateFrom(r.Context())
	cfg := st.cfg

	key, ok := auth.AuthenticateHeader(r, claudeProxyKeyHeader, cfg.Auth.Enabled, cfg.Keys)
	if !ok {
		writeClaudeError(w, lr, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key")
		return
	}
	logKey(lr, cfg, key)

	if st.claudeSub == nil {
		writeClaudeError(w, lr, http.StatusNotFound, "not_found_error", "not_found", "Claude subscription proxying is not enabled")
		return
	}

	body, err := readRequestBody(w, r, cfg.Server.MaxBodyBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeClaudeError(w, lr, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "Request body is too large")
			return
		}
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		writeClaudeError(w, lr, http.StatusBadRequest, "invalid_request_error", "invalid_request", "Unable to read request body")
		return
	}

	model, stream := claudeRequestFields(body)
	if model != "" && !cfg.ModelAllowed(key, model) {
		writeClaudeError(w, lr, http.StatusForbidden, "permission_error", "model_not_allowed", "Model is not allowed for this API key")
		return
	}

	path := strings.TrimPrefix(r.URL.EscapedPath(), claudeSubPath)
	if path == "" {
		path = "/"
	}

	// Claude subscription traffic is tagged with the anthropic provider so
	// access logs, dashboards, and pricing all share one namespace.
	lr.provider = "anthropic"
	lr.billingMode = "subscription"
	lr.upstream = model
	lr.passthrough = model != ""
	lr.stream = stream

	headers := claudeForwardHeaders(r)
	if headers.Get(requestIDHeader) == "" {
		headers.Set(requestIDHeader, lr.requestID)
	}

	resp, err := st.claudeSub.Proxy(r.Context(), provider.ProxyRequest{
		Method: r.Method,
		Path:   path,
		Query:  r.URL.RawQuery,
		Header: headers,
		Body:   body,
	})
	if err != nil {
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		lr.errorDetail = err.Error()
		var providerErr *provider.Error
		if errors.As(err, &providerErr) && providerErr.Kind == provider.Timeout {
			writeClaudeError(w, lr, http.StatusGatewayTimeout, "api_error", "upstream_timeout", "Upstream provider timed out")
		} else {
			writeClaudeError(w, lr, http.StatusBadGateway, "api_error", "upstream_unavailable", "Upstream provider is unavailable")
		}
		return
	}
	defer resp.Body.Close()

	if model != "" {
		w.Header().Set("X-Model-Proxy-Upstream", model)
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set(requestIDHeader, lr.requestID)
	sse := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	streaming := (stream || sse) && resp.StatusCode >= 200 && resp.StatusCode < 300
	lr.stream = lr.stream || sse
	if w.Header().Get("Content-Type") == "" {
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}

	if resp.StatusCode >= 400 {
		lr.errorCode = "upstream_error"
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
			lr.errorDetail = err.Error()
		}
		if r.Context().Err() != nil {
			lr.clientCanceled = true
		}
		return
	}

	w.WriteHeader(resp.StatusCode)

	var copyErr error
	if streaming {
		lr.usage, copyErr = copyClaudeSSE(w, resp.Body, cfg.Logging.LogUsage)
	} else {
		lr.usage, copyErr = copyClaudeJSON(w, resp.Body, cfg.Logging.LogUsage)
	}
	if copyErr != nil && r.Context().Err() == nil {
		lr.errorDetail = copyErr.Error()
	}
	if r.Context().Err() != nil {
		lr.clientCanceled = true
	}
	applyClaudeEstimatedCost(lr, st, model)
}

// claudeRequestFields reads the model and stream flag from a Messages request.
// Parsing is best-effort: the body is forwarded unchanged even when it is not
// the JSON shape this function expects.
func claudeRequestFields(body []byte) (model string, stream bool) {
	if len(body) == 0 {
		return "", false
	}
	var fields struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false
	}
	return strings.TrimSpace(fields.Model), fields.Stream
}

// claudeForwardHeaders copies the client's end-to-end headers for the upstream
// call. Authorization and x-api-key are intentionally preserved: they carry the
// client's subscription or API credential.
func claudeForwardHeaders(r *http.Request) http.Header {
	dst := make(http.Header, len(r.Header))

	connectionTokens := make(map[string]struct{})
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			connectionTokens[http.CanonicalHeaderKey(strings.TrimSpace(token))] = struct{}{}
		}
	}

	for key, values := range r.Header {
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := hopByHopHeaders[canonical]; ok {
			continue
		}
		if _, ok := connectionTokens[canonical]; ok {
			continue
		}
		switch canonical {
		case "Content-Length", "Accept-Encoding", claudeProxyKeyHeader:
			continue
		}
		if strings.HasPrefix(canonical, "X-Model-Proxy-") {
			continue
		}
		dst[canonical] = append([]string(nil), values...)
	}
	return dst
}

// copyClaudeJSON forwards a non-streaming Messages response unchanged and
// observes its usage object.
func copyClaudeJSON(dst io.Writer, src io.Reader, parseUsage bool) (*usage, error) {
	if !parseUsage {
		_, err := io.Copy(dst, src)
		return nil, err
	}

	body, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}

	var envelope struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Usage == nil {
		return nil, nil
	}
	return envelope.Usage.toUsage(), nil
}

// copyClaudeSSE forwards Anthropic SSE bytes unchanged. Usage arrives in
// message_start (input and cache counters) and message_delta (final output
// count), so the counters are merged across events.
func copyClaudeSSE(dst http.ResponseWriter, src io.Reader, parseUsage bool) (*usage, error) {
	reader := bufio.NewReader(src)
	var acc anthropicUsage
	seen := false

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if parseUsage {
				if observed := claudeUsageFromSSELine(line); observed != nil {
					acc.merge(observed)
					seen = true
				}
			}
			if _, writeErr := dst.Write(line); writeErr != nil {
				return claudeUsageResult(acc, seen), writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return claudeUsageResult(acc, seen), nil
			}
			return claudeUsageResult(acc, seen), err
		}
	}
}

func claudeUsageResult(acc anthropicUsage, seen bool) *usage {
	if !seen {
		return nil
	}
	return acc.toUsage()
}

func claudeUsageFromSSELine(line []byte) *anthropicUsage {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"usage"`)) {
		return nil
	}

	var event struct {
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil
	}
	if event.Usage != nil {
		return event.Usage
	}
	if event.Message != nil {
		return event.Message.Usage
	}
	return nil
}

// anthropicUsage is Anthropic's usage object. Anthropic reports uncached input
// tokens in input_tokens and cache counters separately.
type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

func (u *anthropicUsage) merge(other *anthropicUsage) {
	if other == nil {
		return
	}
	if other.InputTokens != nil {
		u.InputTokens = other.InputTokens
	}
	if other.OutputTokens != nil {
		u.OutputTokens = other.OutputTokens
	}
	if other.CacheCreationInputTokens != nil {
		u.CacheCreationInputTokens = other.CacheCreationInputTokens
	}
	if other.CacheReadInputTokens != nil {
		u.CacheReadInputTokens = other.CacheReadInputTokens
	}
}

// toUsage converts Anthropic counters to the proxy's access-log shape.
// PromptTokens includes cached input so the estimate and the logged total match
// other providers, while the cache counters remain separately available.
func (u *anthropicUsage) toUsage() *usage {
	input := int64OrZero(u.InputTokens)
	output := int64OrZero(u.OutputTokens)
	cacheWrite := int64OrZero(u.CacheCreationInputTokens)
	cached := int64OrZero(u.CacheReadInputTokens)

	out := &usage{
		PromptTokens:     input + cacheWrite + cached,
		CompletionTokens: output,
		TotalTokens:      input + cacheWrite + cached + output,
	}
	if u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil {
		out.PromptTokensDetails = &promptTokensDetails{}
		if u.CacheReadInputTokens != nil {
			out.PromptTokensDetails.CachedTokens = u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens != nil {
			out.PromptTokensDetails.CacheWriteTokens = u.CacheCreationInputTokens
		}
	}
	return out
}

func int64OrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// defaultClaudeSubscription returns the built-in transport for /claude_sub/
// when the feature is enabled.
func defaultClaudeSubscription(cfg *config.Config) provider.TransparentProxy {
	if !cfg.EnableClaudeSubscription {
		return nil
	}
	return claudesub.New(claudesub.DefaultBaseURL)
}

// applyClaudeEstimatedCost logs an API-equivalent list-price estimate. Claude
// subscription responses do not report a dollar cost, so the estimate is only
// written to the access log and never injected into the response.
func applyClaudeEstimatedCost(lr *requestLog, st *state, model string) {
	if lr.usage == nil || lr.usage.Cost != nil || model == "" || st.pricing == nil {
		return
	}

	cost, ok := st.pricing.Estimate("anthropic", model, usageForPricing(lr.usage))
	if !ok {
		// Claude Code may send a dated model id while the catalog indexes the
		// same model under its undated alias. Try that before giving up.
		pricingModel := claudePricingModelID(model)
		if pricingModel == model {
			return
		}
		cost, ok = st.pricing.Estimate("anthropic", pricingModel, usageForPricing(lr.usage))
		if !ok {
			return
		}
	}
	lr.estimatedCost = &cost
	lr.pricingSource = "models.dev"
}

// claudePricingModelID strips a 1M-context suffix and an ISO date suffix from a
// Claude model id so catalog aliases can be found.
func claudePricingModelID(model string) string {
	if i := strings.IndexByte(model, '['); i > 0 {
		model = model[:i]
	}
	const dateLen = len("20060102")
	if len(model) > dateLen+1 && model[len(model)-dateLen-1] == '-' && allDigits(model[len(model)-dateLen:]) {
		return model[:len(model)-dateLen-1]
	}
	return model
}

func allDigits(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return value != ""
}

// writeClaudeError writes Anthropic's error envelope for proxy-generated
// failures on /claude_sub/. Upstream errors are passed through untouched.
func writeClaudeError(w http.ResponseWriter, lr *requestLog, status int, errorType, code, message string) {
	if lr != nil {
		lr.errorCode = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errorType,
			"message": message,
		},
	})
}
