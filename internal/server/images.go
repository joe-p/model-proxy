package server

import (
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

// handleImagesGenerations proxies OpenAI-compatible image generation requests
// to providers that implement provider.ImagesProvider. The request body is
// rewritten only to resolve the model alias; the response is streamed through
// unchanged because image payloads are large and opaque to the proxy.
func (s *Server) handleImagesGenerations(w http.ResponseWriter, r *http.Request) {
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
	images, ok := prov.(provider.ImagesProvider)
	if !ok {
		writeAPIError(w, lr, errInvalidRequest(fmt.Sprintf(
			"Provider %q does not support image generation", model.Provider,
		)))
		return
	}

	lr.alias = model.Alias
	lr.provider = model.Provider
	lr.billingMode = cfg.Providers[model.Provider].BillingMode
	lr.upstream = model.Upstream
	lr.passthrough = model.Passthrough
	lr.stream = stream

	resp, err := images.Images(r.Context(), provider.Request{
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
		w.Header().Set("Content-Type", "application/json")
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
	lr.usage, copyErr = copyImageJSON(w, resp.Body, cfg.Logging.LogUsage)
	if copyErr != nil && r.Context().Err() == nil {
		lr.errorDetail = copyErr.Error()
	}
	if r.Context().Err() != nil {
		lr.clientCanceled = true
	}
	applyEstimatedCost(lr, st, model)
}

// copyImageJSON streams an image response to dst and observes the top-level
// usage object without buffering the image payload. The body is never
// rewritten: only the access log needs the usage numbers, and image responses
// can be several megabytes of base64. JSON parse failures are not request
// failures, because the same path also carries non-JSON (for example SSE)
// responses; the remaining bytes are forwarded regardless.
func copyImageJSON(dst io.Writer, src io.Reader, parseUsage bool) (*usage, error) {
	if !parseUsage {
		_, err := io.Copy(dst, src)
		return nil, err
	}

	// TeeReader forwards every byte the decoder reads, so the client receives
	// the original body even when parsing stops early.
	dec := json.NewDecoder(io.TeeReader(src, dst))

	tok, err := dec.Token()
	if err != nil {
		return nil, forwardRemaining(dst, src)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, forwardRemaining(dst, src)
	}

	var observed *usage
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return observed, forwardRemaining(dst, src)
		}
		key, _ := keyToken.(string)
		if key == "usage" {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return observed, forwardRemaining(dst, src)
			}
			observed = imageUsageFrom(raw)
			continue
		}
		if err := skipJSONValue(dec); err != nil {
			return observed, forwardRemaining(dst, src)
		}
	}
	if _, err := dec.Token(); err != nil {
		return observed, forwardRemaining(dst, src)
	}
	return observed, forwardRemaining(dst, src)
}

// forwardRemaining copies whatever the decoder did not consume. Bytes the
// decoder read ahead were already written by the TeeReader, so this cannot
// duplicate or drop output.
func forwardRemaining(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}

// skipJSONValue consumes one JSON value without retaining it. json.RawMessage
// would buffer the value, which is exactly what must be avoided for base64
// image data.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = dec.Token()
	return err
}

// imageUsage is the union of the token shapes image APIs return. OpenAI uses
// input_tokens/output_tokens; OpenRouter uses prompt_tokens/completion_tokens
// and reports cost.
type imageUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	InputTokens             int64                    `json:"input_tokens"`
	OutputTokens            int64                    `json:"output_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	Cost                    *float64                 `json:"cost"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details"`
}

func imageUsageFrom(raw json.RawMessage) *usage {
	var in imageUsage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}

	prompt := in.PromptTokens
	if prompt == 0 {
		prompt = in.InputTokens
	}
	completion := in.CompletionTokens
	if completion == 0 {
		completion = in.OutputTokens
	}
	total := in.TotalTokens
	if total == 0 && (prompt != 0 || completion != 0) {
		total = prompt + completion
	}
	if prompt == 0 && completion == 0 && total == 0 && in.Cost == nil {
		return nil
	}

	return &usage{
		PromptTokens:            prompt,
		CompletionTokens:        completion,
		TotalTokens:             total,
		Cost:                    in.Cost,
		PromptTokensDetails:     in.PromptTokensDetails,
		CompletionTokensDetails: in.CompletionTokensDetails,
	}
}
