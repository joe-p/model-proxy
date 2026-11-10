package server

import (
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

	aliases := make([]string, 0, len(st.cfg.Models))
	for alias := range st.cfg.Models {
		if st.cfg.ModelAllowed(key, alias) {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelEntry, 0, len(aliases))
	for _, alias := range aliases {
		data = append(data, modelEntry{
			ID:      alias,
			Object:  "model",
			Created: s.startedAt.Unix(),
			OwnedBy: "model-proxy",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}{
		Object: "list",
		Data:   data,
	}); err != nil {
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
	var alias string
	if err := json.Unmarshal(rawModel, &alias); err != nil || strings.TrimSpace(alias) == "" {
		writeAPIError(w, lr, errInvalidRequest("Request field \"model\" must be a non-empty string"))
		return
	}

	for _, field := range []string{"models", "route", "plugins"} {
		if _, exists := fields[field]; exists {
			writeAPIError(w, lr, errInvalidRequest(fmt.Sprintf("Request field %q is not allowed", field)))
			return
		}
	}

	model, exists := cfg.Models[alias]
	if !exists {
		writeAPIError(w, lr, errModelNotFound())
		return
	}
	if !cfg.ModelAllowed(key, alias) {
		writeAPIError(w, lr, errModelNotAllowed())
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

	lr.alias = alias
	lr.provider = model.Provider
	lr.upstream = model.Upstream
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

	w.Header().Set("X-Model-Proxy-Alias", alias)
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

	w.WriteHeader(resp.StatusCode)

	var copyErr error
	if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		lr.usage, copyErr = copySSE(w, resp.Body, cfg.Logging.LogUsage)
	} else {
		lr.usage, copyErr = copyJSON(w, resp.Body, cfg.Logging.LogUsage)
	}
	if copyErr != nil && r.Context().Err() == nil {
		lr.errorDetail = copyErr.Error()
	}
	if r.Context().Err() != nil {
		lr.clientCanceled = true
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, logRecordFrom(r.Context()), errNotFound())
}

// logKey records the authenticated key and the policy that applies to it.
func logKey(lr *requestLog, cfg *config.Config, key *config.VirtualKey) {
	if key == nil {
		return
	}
	lr.keyName = key.Name
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

func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return io.ReadAll(r.Body)
}
