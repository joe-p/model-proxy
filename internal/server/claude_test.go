package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/claudesub"
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/server"
)

const claudeMessagesPath = "/claude_sub/v1/messages?beta=true"

func postClaude(t *testing.T, proxyURL, proxyKey, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if proxyKey != "" {
		req.Header.Set("X-Model-Proxy-Key", "Bearer "+proxyKey)
	}
	req.Header.Set("Authorization", "Bearer claude-oauth-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.0")
	req.Header.Set("X-Request-ID", "claude-request-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func newClaudeTestProxy(t *testing.T, upstreamURL string, tweak func(*config.Config)) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	return newClaudeTestProxyWithPricing(t, upstreamURL, tweak, nil)
}

func newClaudeTestProxyWithPricing(t *testing.T, upstreamURL string, tweak func(*config.Config), estimator server.CostEstimator) (*httptest.Server, *lockedBuffer) {
	t.Helper()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:            "127.0.0.1:0",
			MaxBodyBytes:      1 << 20,
			ReadHeaderTimeout: time.Second,
			ShutdownTimeout:   time.Second,
		},
		Logging:                  config.LoggingConfig{Format: "json", Level: slog.LevelDebug, LogUsage: true},
		Auth:                     config.AuthConfig{Enabled: true},
		Keys:                     []config.VirtualKey{{Name: "test", Key: "test-key"}},
		EnableClaudeSubscription: true,
	}
	if tweak != nil {
		tweak(cfg)
	}

	logBuf := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := server.New(cfg, nil, logger, estimator,
		server.WithClaudeSubscriptionTransport(claudesub.New(upstreamURL)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, logBuf
}

func TestClaudeSubForwardsTransparentlyAndLogsUsage(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	const responseBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":50,"output_tokens":20}}`

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("upstream method = %s", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "beta=true" {
			t.Errorf("upstream query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer claude-oauth-token" {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := r.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Errorf("upstream anthropic-version = %q", got)
		}
		if got := r.Header.Get("Anthropic-Beta"); got != "oauth-2025-04-20" {
			t.Errorf("upstream anthropic-beta = %q", got)
		}
		if got := r.Header.Get("X-Model-Proxy-Key"); got != "" {
			t.Errorf("proxy key leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-Request-ID"); got != "claude-request-id" {
			t.Errorf("upstream request id = %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		if string(body) != requestBody {
			t.Errorf("upstream body changed:\nwant %s\ngot  %s", requestBody, body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_upstream")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newClaudeTestProxyWithPricing(t, upstream.URL, nil, fakeEstimator{cost: 0.42})
	resp := postClaude(t, proxy.URL, "test-key", claudeMessagesPath, requestBody)
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, gotBody)
	}
	if string(gotBody) != responseBody {
		t.Fatalf("body changed:\nwant %s\ngot  %s", responseBody, gotBody)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d", upstreamCalls.Load())
	}
	if got := resp.Header.Get("X-Model-Proxy-Upstream"); got != "claude-sonnet-4-5-20250929" {
		t.Fatalf("upstream header = %q", got)
	}
	if got := resp.Header.Get("Request-Id"); got != "req_upstream" {
		t.Fatalf("upstream response header not copied: %q", got)
	}
	if got := resp.Header.Get("X-Request-ID"); got != "claude-request-id" {
		t.Fatalf("request id header = %q", got)
	}

	waitForLog(t, logs, `"key_name":"test"`)
	waitForLog(t, logs, `"provider":"anthropic"`)
	waitForLog(t, logs, `"billing_mode":"subscription"`)
	waitForLog(t, logs, `"upstream_model":"claude-sonnet-4-5-20250929"`)
	waitForLog(t, logs, `"passthrough":true`)
	waitForLog(t, logs, `"prompt_tokens":153`)
	waitForLog(t, logs, `"completion_tokens":20`)
	waitForLog(t, logs, `"total_tokens":173`)
	waitForLog(t, logs, `"cached_tokens":50`)
	waitForLog(t, logs, `"cache_write_tokens":100`)
	waitForLog(t, logs, `"estimated_api_cost_usd":0.42`)
	waitForLog(t, logs, `"pricing_source":"models.dev"`)
}

func TestClaudeSubscriptionModelsAreNotListed(t *testing.T) {
	proxy, _ := newClaudeTestProxy(t, "http://127.0.0.1:1", nil)

	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var payload struct {
		Data       []json.RawMessage `json:"data"`
		TotalCount int               `json:"total_count"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode models payload %s: %v", body, err)
	}
	if resp.StatusCode != http.StatusOK || len(payload.Data) != 0 || payload.TotalCount != 0 {
		t.Fatalf("unexpected models payload: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestClaudeSubIsDisabledByDefault(t *testing.T) {
	proxy, _ := newTestProxy(t, "http://127.0.0.1:1", nil)
	resp := postClaude(t, proxy.URL, "test-key", claudeMessagesPath, `{"model":"claude-sonnet-4-5","messages":[]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestClaudeSubRequiresProxyKeyButNotAuthorization(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	proxy, _ := newClaudeTestProxy(t, upstream.URL, nil)

	resp := postClaude(t, proxy.URL, "", claudeMessagesPath, `{"model":"claude-sonnet-4-5","messages":[]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if envelope.Type != "error" || envelope.Error.Type != "authentication_error" {
		t.Fatalf("unexpected error envelope: %s", body)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream should not have been called")
	}
}

func TestClaudeSubPassesThroughUpstreamError(t *testing.T) {
	const errorBody = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errorBody))
	}))
	defer upstream.Close()

	proxy, logs := newClaudeTestProxy(t, upstream.URL, nil)
	resp := postClaude(t, proxy.URL, "test-key", claudeMessagesPath, `{"model":"claude-sonnet-4-5","messages":[]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != errorBody {
		t.Fatalf("upstream error body changed:\nwant %s\ngot  %s", errorBody, body)
	}
	waitForLog(t, logs, `"error_code":"upstream_error"`)
	// The access log must name the reason, not just that upstream failed.
	waitForLog(t, logs, `"error":"rate_limit_error: slow down"`)
}

func TestClaudeSubStreamsTransparentlyAndLogsUsage(t *testing.T) {
	const streamBody = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":2,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range strings.SplitAfter(streamBody, "\n") {
			_, _ = w.Write([]byte(line))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	proxy, logs := newClaudeTestProxy(t, upstream.URL, nil)
	resp := postClaude(t, proxy.URL, "test-key", claudeMessagesPath, `{"model":"claude-sonnet-4-5","messages":[],"stream":true}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if string(body) != streamBody {
		t.Fatalf("stream changed:\nwant %q\ngot  %q", streamBody, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	waitForLog(t, logs, `"stream":true`)
	waitForLog(t, logs, `"prompt_tokens":7`)
	waitForLog(t, logs, `"completion_tokens":7`)
	waitForLog(t, logs, `"total_tokens":14`)
	waitForLog(t, logs, `"cached_tokens":2`)
}

func TestClaudeSubAppliesKeyPolicyToModel(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	proxy, _ := newClaudeTestProxy(t, upstream.URL, func(cfg *config.Config) {
		cfg.Keys = []config.VirtualKey{{Name: "fast-only", Key: "test-key", Policy: "basic"}}
		cfg.Policies = map[string]config.PolicyConfig{
			"basic": {
				Name:   "basic",
				Mode:   config.PolicyWhitelist,
				Models: map[string]struct{}{"fast": {}},
			},
		}
	})

	resp := postClaude(t, proxy.URL, "test-key", claudeMessagesPath, `{"model":"claude-sonnet-4-5","messages":[]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream should not have been called")
	}
	if !strings.Contains(string(body), `"permission_error"`) {
		t.Fatalf("unexpected error body: %s", body)
	}
}
