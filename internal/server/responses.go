package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/joe-p/model-proxy/internal/auth"
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
)

// handleResponses proxies the OpenAI Responses API. The response body is
// forwarded unchanged so Responses-native clients keep their own usage
// tracking; the proxy observes usage only for its access log.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
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
	lr.billingMode = cfg.Providers[model.Provider].BillingMode
	lr.upstream = model.Upstream
	lr.stream = stream

	resp, err := prov.Responses(r.Context(), provider.Request{
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
	sse := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if w.Header().Get("Content-Type") == "" {
		if (stream || sse) && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}

	w.WriteHeader(resp.StatusCode)

	var copyErr error
	if (stream || sse) && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		lr.usage, copyErr = copyResponsesSSE(w, resp.Body, cfg.Logging.LogUsage)
	} else {
		lr.usage, copyErr = copyResponsesJSON(w, resp.Body, cfg.Logging.LogUsage)
	}
	if copyErr != nil && r.Context().Err() == nil {
		lr.errorDetail = copyErr.Error()
	}
	if r.Context().Err() != nil {
		lr.clientCanceled = true
	}
	applyEstimatedCost(lr, st, model)
}

// copyResponsesSSE forwards Responses SSE bytes unchanged and observes usage
// from response.completed / response.incomplete events.
func copyResponsesSSE(dst http.ResponseWriter, src io.Reader, parseUsage bool) (*usage, error) {
	reader := bufio.NewReader(src)
	var lastUsage *usage

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if parseUsage {
				if observed := responsesUsageFromSSELine(line); observed != nil {
					lastUsage = observed
				}
			}
			if _, writeErr := dst.Write(line); writeErr != nil {
				return lastUsage, writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return lastUsage, nil
			}
			return lastUsage, err
		}
	}
}

func copyResponsesJSON(dst io.Writer, src io.Reader, parseUsage bool) (*usage, error) {
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
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil
	}
	return normalizeResponsesUsage(envelope.Usage), nil
}

func responsesUsageFromSSELine(line []byte) *usage {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return nil
	}

	var event struct {
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.Response == nil {
		return nil
	}
	return normalizeResponsesUsage(event.Response.Usage)
}

type responsesUsage struct {
	InputTokens        int64    `json:"input_tokens"`
	OutputTokens       int64    `json:"output_tokens"`
	TotalTokens        int64    `json:"total_tokens"`
	Cost               *float64 `json:"cost"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func normalizeResponsesUsage(in *responsesUsage) *usage {
	if in == nil {
		return nil
	}

	out := &usage{
		PromptTokens:     in.InputTokens,
		CompletionTokens: in.OutputTokens,
		TotalTokens:      in.TotalTokens,
		Cost:             in.Cost,
	}
	if out.TotalTokens == 0 && (out.PromptTokens != 0 || out.CompletionTokens != 0) {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if in.InputTokensDetails != nil {
		out.PromptTokensDetails = &promptTokensDetails{CachedTokens: in.InputTokensDetails.CachedTokens}
	}
	if in.OutputTokensDetails != nil {
		out.CompletionTokensDetails = &completionTokensDetails{ReasoningTokens: in.OutputTokensDetails.ReasoningTokens}
	}
	return out
}
