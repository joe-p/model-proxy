package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/joe-p/model-proxy/internal/auth"
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, logRecordFrom(r.Context()), errMethodNotAllowed(http.MethodGet))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	if r.Method != http.MethodGet {
		writeAPIError(w, lr, errMethodNotAllowed(http.MethodGet))
		return
	}

	st := stateFrom(r.Context())
	key, ok := auth.Authenticate(r, st.cfg.Auth.Enabled, st.cfg.Keys)
	if !ok {
		writeAPIError(w, lr, errInvalidAPIKey())
		return
	}
	logKey(lr, st.cfg, key)

	entries := s.modelEntries(r.Context(), st, key)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.modelsPayload(r.Context(), st, entries)); err != nil {
		lr.errorDetail = err.Error()
	}
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	if r.Method != http.MethodPost {
		writeAPIError(w, lr, errMethodNotAllowed(http.MethodPost))
		return
	}

	st := stateFrom(r.Context())
	cfg := st.cfg

	key, ok := auth.Authenticate(r, cfg.Auth.Enabled, cfg.Keys)
	if !ok {
		writeAPIError(w, lr, errInvalidAPIKey())
		return
	}
	logKey(lr, cfg, key)

	body, err := readRequestBody(w, r, cfg.Server.MaxBodyBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, lr, errRequestBodyTooLarge())
			return
		}
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		writeAPIError(w, lr, errInvalidRequest("Unable to read request body"))
		return
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeAPIError(w, lr, errInvalidRequest("Request body must be a JSON object"))
		return
	}

	rawModel, exists := fields["model"]
	if !exists {
		writeAPIError(w, lr, errInvalidRequest("Request field \"model\" is required"))
		return
	}
	var requestedModel string
	if err := json.Unmarshal(rawModel, &requestedModel); err != nil || strings.TrimSpace(requestedModel) == "" {
		writeAPIError(w, lr, errInvalidRequest("Request field \"model\" must be a non-empty string"))
		return
	}

	tolerateOpenRouterFields(lr, fields)

	model, resolveErr := resolveModel(cfg, key, requestedModel)
	if resolveErr != nil {
		writeAPIError(w, lr, resolveErr)
		return
	}

	stream := false
	if rawStream, exists := fields["stream"]; exists {
		var streamValue *bool
		if err := json.Unmarshal(rawStream, &streamValue); err != nil || streamValue == nil {
			writeAPIError(w, lr, errInvalidRequest("Request field \"stream\" must be a boolean"))
			return
		}
		stream = *streamValue
	}

	if len(model.ExtraBody) > 0 {
		fields, err = config.MergeExtraBody(fields, model.ExtraBody)
		if err != nil {
			lr.errorDetail = err.Error()
			writeAPIError(w, lr, errInternal())
			return
		}
	}

	providerType := cfg.Providers[model.Provider].Type
	if providerType == "" {
		providerType = model.Provider
	}
	// OpenRouter always reports usage, including for streams. Ask
	// OpenAI-compatible providers for the same terminal usage chunk.
	if stream && providerType != "openrouter" {
		ensureStreamUsage(fields)
	}
	cost := chatCost(st, cfg, model)

	// The alias rewrite always wins, even for hand-built configs that bypass
	// load-time validation of reserved extra_body fields.
	upstreamModel, err := json.Marshal(model.Upstream)
	if err != nil {
		writeAPIError(w, lr, errInternal())
		return
	}
	fields["model"] = upstreamModel
	upstreamBody, err := json.Marshal(fields)
	if err != nil {
		writeAPIError(w, lr, errInternal())
		return
	}

	prov, exists := st.providers[model.Provider]
	if !exists {
		lr.errorDetail = fmt.Sprintf("provider %q is not available", model.Provider)
		writeAPIError(w, lr, errInternal())
		return
	}

	lr.alias = model.Alias
	lr.provider = model.Provider
	lr.billingMode = cfg.Providers[model.Provider].BillingMode
	lr.upstream = model.Upstream
	lr.passthrough = model.Passthrough
	lr.stream = stream

	resp, err := prov.ChatCompletions(r.Context(), provider.Request{
		UpstreamModel: model.Upstream,
		Body:          upstreamBody,
		Stream:        stream,
		Accept:        r.Header.Get("Accept"),
		RequestID:     lr.requestID,
		Query:         r.URL.RawQuery,
	})
	if err != nil {
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		lr.errorDetail = err.Error()
		var providerErr *provider.Error
		if errors.As(err, &providerErr) && providerErr.Kind == provider.Timeout {
			writeAPIError(w, lr, errUpstreamTimeout())
		} else {
			writeAPIError(w, lr, errUpstreamUnavailable())
		}
		return
	}
	defer resp.Body.Close()

	w.Header().Set("X-Model-Proxy-Alias", model.Alias)
	w.Header().Set("X-Model-Proxy-Upstream", model.Upstream)
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set(requestIDHeader, lr.requestID)
	if w.Header().Get("Content-Type") == "" {
		if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}

	if resp.StatusCode >= 400 {
		lr.errorCode = "upstream_error"
		if err := writeUpstreamError(w, resp, model.Provider); err != nil && r.Context().Err() == nil {
			lr.errorDetail = err.Error()
		}
		if r.Context().Err() != nil {
			lr.clientCanceled = true
		}
		return
	}

	w.WriteHeader(resp.StatusCode)

	var copyErr error
	if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		lr.usage, copyErr = copySSE(w, resp.Body, cfg.Logging.LogUsage, cost)
	} else {
		lr.usage, copyErr = copyJSON(w, resp.Body, cfg.Logging.LogUsage, cost)
	}
	if copyErr != nil && r.Context().Err() == nil {
		lr.errorDetail = copyErr.Error()
	}
	if r.Context().Err() != nil {
		lr.clientCanceled = true
	}
	applyEstimatedCost(lr, st, model)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, logRecordFrom(r.Context()), errNotFound())
}

// logKey records the authenticated key, its access-log user-agent override,
// and the policy that applies to it.
func logKey(lr *requestLog, cfg *config.Config, key *config.VirtualKey) {
	if key == nil {
		return
	}
	lr.keyName = key.Name
	if key.UserAgent != "" {
		lr.userAgent = key.UserAgent
	}
	if key.Policy == "" {
		return
	}
	lr.policyName = key.Policy
	policy, ok := cfg.Policies[key.Policy]
	if !ok {
		return
	}
	lr.policyMode = policy.Mode
	lr.policyModels = make([]string, 0, len(policy.Models))
	for alias := range policy.Models {
		lr.policyModels = append(lr.policyModels, alias)
	}
	sort.Strings(lr.policyModels)
}

// ensureStreamUsage asks an OpenAI-compatible upstream to include the
// terminal usage chunk. OpenRouter always includes it; matching that behavior
// keeps OpenRouter-compatible clients and access logs consistent.
func ensureStreamUsage(fields map[string]json.RawMessage) {
	raw, exists := fields["stream_options"]
	trimmed := bytes.TrimSpace(raw)
	if !exists || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		return
	}

	var options map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &options); err != nil || options == nil {
		return
	}
	options["include_usage"] = json.RawMessage(`true`)
	encoded, err := json.Marshal(options)
	if err == nil {
		fields["stream_options"] = encoded
	}
}

func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return io.ReadAll(r.Body)
}
