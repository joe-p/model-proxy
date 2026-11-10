package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/pricing"
	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/openai"
	"github.com/joe-p/model-proxy/internal/provider/openrouter"
	"github.com/joe-p/model-proxy/internal/server"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type fakeEstimator struct {
	cost float64
}

func (f fakeEstimator) Estimate(string, string, pricing.Usage) (float64, bool) {
	return f.cost, true
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestProxy(t *testing.T, upstreamURL string, tweak func(*config.Config)) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	return newTestProxyWithProvider(t, upstreamURL, "openrouter", tweak)
}

func newTestProxyWithProvider(t *testing.T, upstreamURL, providerType string, tweak func(*config.Config)) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	return newTestProxyWithPricing(t, upstreamURL, providerType, tweak, nil)
}

func newTestProxyWithPricing(t *testing.T, upstreamURL, providerType string, tweak func(*config.Config), estimator server.CostEstimator) (*httptest.Server, *lockedBuffer) {
	t.Helper()

	providerName := "openrouter"
	if providerType != "openrouter" {
		providerName = "phala"
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:            "127.0.0.1:0",
			MaxBodyBytes:      1 << 20,
			ReadHeaderTimeout: time.Second,
			ShutdownTimeout:   time.Second,
		},
		Logging: config.LoggingConfig{Format: "json", Level: slog.LevelDebug, LogUsage: true},
		Auth:    config.AuthConfig{Enabled: true},
		Keys:    []config.VirtualKey{{Name: "test", Key: "test-key"}},
		Providers: map[string]config.ProviderConfig{
			providerName: {
				Name:                  providerName,
				Type:                  providerType,
				BaseURL:               upstreamURL,
				APIKey:                "upstream-key",
				DialTimeout:           time.Second,
				ResponseHeaderTimeout: time.Second,
				Referer:               "https://example.test",
				Title:                 "model-proxy-test",
			},
		},
		Models: map[string]config.ModelConfig{
			"fast":  {Alias: "fast", Provider: providerName, Upstream: "openai/gpt-4o-mini"},
			"smart": {Alias: "smart", Provider: providerName, Upstream: "anthropic/claude-sonnet-4"},
		},
	}
	if tweak != nil {
		tweak(cfg)
	}

	logBuf := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	providerCfg := cfg.Providers[providerName]
	var p provider.Provider
	var err error
	switch providerType {
	case "openrouter":
		p, err = openrouter.New(
			providerCfg.Name,
			providerCfg.BaseURL,
			providerCfg.APIKey,
			providerCfg.DialTimeout,
			providerCfg.ResponseHeaderTimeout,
			providerCfg.Referer,
			providerCfg.Title,
		)
	case "openai-compatible":
		p, err = openai.New(
			providerCfg.Name,
			providerCfg.BaseURL,
			providerCfg.APIKey,
			providerCfg.DialTimeout,
			providerCfg.ResponseHeaderTimeout,
		)
	default:
		t.Fatalf("unsupported test provider type %q", providerType)
	}
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}

	srv := server.New(cfg, map[string]provider.Provider{providerName: p}, logger, estimator)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, logBuf
}

func waitForLog(t *testing.T, buf *lockedBuffer, substring string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substring) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log did not contain %q; logs:\n%s", substring, buf.String())
}

func postChat(t *testing.T, proxyURL, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions?foo=bar", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "model-proxy-test-client/1.0")
	req.Header.Set("X-Request-ID", "client-request-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func postResponses(t *testing.T, proxyURL, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestModelsRequiresAuthAndListsSortedAliases(t *testing.T) {
	proxy, _ := newTestProxy(t, "http://127.0.0.1:1", nil)

	resp, err := http.Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models with auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload struct {
		TotalCount int `json:"total_count"`
		Data       []struct {
			ID            string `json:"id"`
			CanonicalSlug string `json:"canonical_slug"`
			Name          string `json:"name"`
			Created       int64  `json:"created"`
			Architecture  struct {
				Modality        string   `json:"modality"`
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if payload.TotalCount != 2 || len(payload.Data) != 2 {
		t.Fatalf("unexpected models payload: %+v", payload)
	}
	if payload.Data[0].ID != "fast" || payload.Data[1].ID != "smart" {
		t.Fatalf("models not sorted: %+v", payload.Data)
	}
	if payload.Data[0].CanonicalSlug != "fast" || payload.Data[0].Created == 0 {
		t.Fatalf("unexpected model metadata: %+v", payload.Data[0])
	}
	if payload.Data[0].Architecture.Modality != "text->text" {
		t.Fatalf("unexpected architecture: %+v", payload.Data[0].Architecture)
	}
}

func TestChatRewritesModelAndPassesResponseThrough(t *testing.T) {
	const responseBody = `{"id":"chatcmpl-1","model":"openai/gpt-4o-mini","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.0001,"cache_discount":0.00002,"prompt_tokens_details":{"cached_tokens":8,"cache_write_tokens":2}},"choices":[]}`
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("upstream method = %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "foo=bar" {
			t.Errorf("upstream query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := r.Header.Get("X-Request-ID"); got != "client-request-id" {
			t.Errorf("upstream request id = %q", got)
		}
		if got := r.Header.Get("HTTP-Referer"); got != "https://example.test" {
			t.Errorf("upstream referer = %q", got)
		}
		if got := r.Header.Get("X-Title"); got != "model-proxy-test" {
			t.Errorf("upstream title = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("upstream accept = %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		var model string
		_ = json.Unmarshal(fields["model"], &model)
		if model != "openai/gpt-4o-mini" {
			t.Errorf("upstream model = %q", model)
		}
		if _, ok := fields["provider"]; !ok {
			t.Errorf("provider field was not passed through")
		}
		if _, ok := fields["messages"]; !ok {
			t.Errorf("messages field was not passed through")
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxy(t, upstream.URL, nil)
	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":false,"provider":{"order":["x"]}}`)
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
	if got := resp.Header.Get("X-Model-Proxy-Alias"); got != "fast" {
		t.Fatalf("alias header = %q", got)
	}
	if got := resp.Header.Get("X-Model-Proxy-Upstream"); got != "openai/gpt-4o-mini" {
		t.Fatalf("upstream header = %q", got)
	}
	if got := resp.Header.Get("X-Upstream"); got != "yes" {
		t.Fatalf("upstream response header not copied: %q", got)
	}
	if got := resp.Header.Get("X-Request-ID"); got != "client-request-id" {
		t.Fatalf("request id header = %q", got)
	}

	waitForLog(t, logs, `"cost_usd":0.0001`)
	waitForLog(t, logs, `"total_tokens":15`)
	waitForLog(t, logs, `"cached_tokens":8`)
	waitForLog(t, logs, `"cache_write_tokens":2`)
	waitForLog(t, logs, `"cache_discount":0.00002`)
	waitForLog(t, logs, `"alias":"fast"`)
	waitForLog(t, logs, `"user_agent":"model-proxy-test-client/1.0"`)
}

func TestChatUsesVirtualKeyUserAgentInAccessLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		cfg.Keys = []config.VirtualKey{
			{Name: "open-webui", Key: "custom-key", UserAgent: "Open WebUI"},
		}
	})

	resp := postChat(t, proxy.URL, "custom-key", `{"model":"fast","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	waitForLog(t, logs, `"key_name":"open-webui"`)
	waitForLog(t, logs, `"user_agent":"Open WebUI"`)
	if strings.Contains(logs.String(), `"user_agent":"model-proxy-test-client/1.0"`) {
		t.Fatalf("client user agent should have been overridden in logs:\n%s", logs.String())
	}
}

func TestChatPassesThroughUpstreamError(t *testing.T) {
	const errorBody = `{"error":{"message":"rate limited","type":"rate_limit"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errorBody))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, nil)
	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Message  string         `json:"message"`
			Code     int            `json:"code"`
			Metadata map[string]any `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if envelope.Error.Message != "rate limited" || envelope.Error.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected error envelope: %s", body)
	}
	if envelope.Error.Metadata["provider_name"] != "openrouter" {
		t.Fatalf("missing provider metadata: %s", body)
	}
}

func TestChatDropsOpenRouterOnlyFields(t *testing.T) {
	var calls atomic.Int32
	gotBody := make(chan map[string]json.RawMessage, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		gotBody <- fields
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy, logs := newTestProxy(t, upstream.URL, nil)
	resp := postChat(t, proxy.URL, "test-key",
		`{"model":"fast","messages":[],"models":["anthropic/claude-opus"],"route":"fallback","plugins":[{"id":"x"}],"transforms":["middle-out"]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	select {
	case fields := <-gotBody:
		for _, field := range []string{"models", "route", "plugins", "transforms"} {
			if _, exists := fields[field]; exists {
				t.Fatalf("%q was forwarded upstream", field)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not called")
	}
	waitForLog(t, logs, `"dropped_fields":["models","route","plugins","transforms"]`)
}

func TestChatRejectsUnknownModelAndInvalidKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not have been called")
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, nil)

	resp := postChat(t, proxy.URL, "test-key", `{"model":"unknown","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d", resp.StatusCode)
	}

	resp2 := postChat(t, proxy.URL, "wrong-key", `{"model":"fast","messages":[]}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key status = %d", resp2.StatusCode)
	}
}

func TestChatRejectsOversizedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not have been called")
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		cfg.Server.MaxBodyBytes = 16
	})

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[{"role":"user","content":"this is too long"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestChatStreamingIsByteTransparentAndLogsUsage(t *testing.T) {
	const streamBody = "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5,\"cost\":0.00002,\"prompt_tokens_details\":{\"cached_tokens\":2,\"cache_write_tokens\":0}}}\n\n" +
		"data: [DONE]\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if _, ok := fields["stream_options"]; ok {
			t.Errorf("OpenRouter should not receive injected stream_options")
		}

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

	proxy, logs := newTestProxy(t, upstream.URL, nil)
	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[],"stream":true}`)
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
	waitForLog(t, logs, `"cost_usd":0.00002`)
	waitForLog(t, logs, `"cached_tokens":2`)
	waitForLog(t, logs, `"cache_write_tokens":0`)
	waitForLog(t, logs, `"stream":true`)
}

func TestPrivateAIGatewayNormalizesUsageToOpenRouter(t *testing.T) {
	const responseBody = `{"id":"chatcmpl-1","object":"chat.completion","usage":{"prompt_tokens":10,"completion_tokens":5,"cache_read_input_tokens":4,"cache_creation_input_tokens":2,"reasoning_tokens":3},"choices":[]}`
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if _, ok := fields["stream_options"]; ok {
			t.Errorf("stream_options should not be added to a non-streaming request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", nil)
	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[],"stream":false}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d", upstreamCalls.Load())
	}

	var envelope struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var total int64
	if err := json.Unmarshal(envelope.Usage["total_tokens"], &total); err != nil || total != 15 {
		t.Fatalf("total_tokens = %d, err = %v", total, err)
	}
	var details struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	}
	if err := json.Unmarshal(envelope.Usage["prompt_tokens_details"], &details); err != nil {
		t.Fatalf("decode prompt_tokens_details: %v", err)
	}
	if details.CachedTokens == nil || *details.CachedTokens != 4 {
		t.Fatalf("cached_tokens = %v", details.CachedTokens)
	}
	if details.CacheWriteTokens == nil || *details.CacheWriteTokens != 2 {
		t.Fatalf("cache_write_tokens = %v", details.CacheWriteTokens)
	}
	var completionDetails struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	}
	if err := json.Unmarshal(envelope.Usage["completion_tokens_details"], &completionDetails); err != nil {
		t.Fatalf("decode completion_tokens_details: %v", err)
	}
	if completionDetails.ReasoningTokens == nil || *completionDetails.ReasoningTokens != 3 {
		t.Fatalf("reasoning_tokens = %v", completionDetails.ReasoningTokens)
	}

	waitForLog(t, logs, `"total_tokens":15`)
	waitForLog(t, logs, `"cached_tokens":4`)
	waitForLog(t, logs, `"cache_write_tokens":2`)
	waitForLog(t, logs, `"reasoning_tokens":3`)
}

func TestPrivateAIGatewayRequestsAndNormalizesStreamingUsage(t *testing.T) {
	const streamBody = "data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"cache_read_input_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields struct {
			StreamOptions map[string]json.RawMessage `json:"stream_options"`
		}
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		var includeUsage bool
		if err := json.Unmarshal(fields.StreamOptions["include_usage"], &includeUsage); err != nil || !includeUsage {
			t.Errorf("include_usage = %v, err = %v", includeUsage, err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", nil)
	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[],"stream":true,"stream_options":{"include_usage":false}}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"cached_tokens":2`) {
		t.Fatalf("stream was not normalized: %s", body)
	}
	if !strings.Contains(string(body), `"total_tokens":3`) {
		t.Fatalf("stream usage total was not normalized: %s", body)
	}
	waitForLog(t, logs, `"cached_tokens":2`)
	waitForLog(t, logs, `"total_tokens":3`)
}

func TestUpstreamTimeoutReturns504(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		providerCfg := cfg.Providers["openrouter"]
		providerCfg.ResponseHeaderTimeout = 20 * time.Millisecond
		cfg.Providers["openrouter"] = providerCfg
	})

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code     int `json:"code"`
			Metadata struct {
				ProxyCode string `json:"proxy_code"`
			} `json:"metadata"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != http.StatusGatewayTimeout || envelope.Error.Metadata.ProxyCode != "upstream_timeout" {
		t.Fatalf("error envelope = %+v", envelope.Error)
	}
}

func TestResponsesStreamingIsByteTransparentAndLogsUsage(t *testing.T) {
	const streamBody = "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":15}}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Errorf("authorization = %q", got)
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		var model string
		_ = json.Unmarshal(fields["model"], &model)
		if model != "openai/gpt-4o-mini" {
			t.Errorf("model = %q", model)
		}
		if _, exists := fields["stream_options"]; exists {
			t.Error("stream_options must not be injected into Responses requests")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.BillingMode = "subscription"
		cfg.Providers["phala"] = providerCfg
	})

	resp := postResponses(t, proxy.URL, "test-key", `{"model":"fast","input":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != streamBody {
		t.Fatalf("body was modified:\n%s", body)
	}

	waitForLog(t, logs, `"billing_mode":"subscription"`)
	waitForLog(t, logs, `"prompt_tokens":10`)
	waitForLog(t, logs, `"completion_tokens":5`)
	waitForLog(t, logs, `"total_tokens":15`)
	waitForLog(t, logs, `"cached_tokens":4`)
	waitForLog(t, logs, `"reasoning_tokens":2`)
}

func TestResponsesNonStreamingLogsUsage(t *testing.T) {
	const responseBody = `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":3},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":19},"output":[]}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.BillingMode = "subscription"
		cfg.Providers["phala"] = providerCfg
	})

	resp := postResponses(t, proxy.URL, "test-key", `{"model":"fast","input":[{"role":"user","content":"hi"}],"stream":false}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != responseBody {
		t.Fatalf("body was modified:\n%s", body)
	}

	waitForLog(t, logs, `"prompt_tokens":12`)
	waitForLog(t, logs, `"completion_tokens":7`)
	waitForLog(t, logs, `"total_tokens":19`)
	waitForLog(t, logs, `"cached_tokens":3`)
	waitForLog(t, logs, `"reasoning_tokens":4`)
	waitForLog(t, logs, `"billing_mode":"subscription"`)
}

func TestResponsesLogsEstimatedCost(t *testing.T) {
	const responseBody = `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":400},"output_tokens":300,"total_tokens":1300},"output":[]}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithPricing(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.BillingMode = "subscription"
		providerCfg.PricingProvider = "openai"
		cfg.Providers["phala"] = providerCfg
	}, fakeEstimator{cost: 0.00424})

	resp := postResponses(t, proxy.URL, "test-key", `{"model":"fast","input":[{"role":"user","content":"hi"}],"stream":false}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	waitForLog(t, logs, `"estimated_api_cost_usd":0.00424`)
	waitForLog(t, logs, `"pricing_source":"models.dev"`)
	waitForLog(t, logs, `"billing_mode":"subscription"`)
}

func TestChatLogsConfiguredPriceOverride(t *testing.T) {
	const responseBody = `{"id":"chatcmpl-1","object":"chat.completion","usage":{"prompt_tokens":1000,"completion_tokens":300,"total_tokens":1300,"prompt_tokens_details":{"cached_tokens":400}},"choices":[]}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	cachedRate := 0.1
	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		modelCfg := cfg.Models["fast"]
		modelCfg.Pricing = &config.ModelPricing{
			InputPerMillion:       2,
			CachedInputPerMillion: &cachedRate,
			OutputPerMillion:      10,
		}
		cfg.Models["fast"] = modelCfg
	})

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	waitForLog(t, logs, `"estimated_api_cost_usd":0.00424`)
	waitForLog(t, logs, `"pricing_source":"config"`)
}

func TestReloadSwapsConfigAndRejectsListenerChange(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:            "127.0.0.1:0",
			MaxBodyBytes:      1 << 20,
			ReadHeaderTimeout: time.Second,
			ShutdownTimeout:   time.Second,
		},
		Logging: config.LoggingConfig{Format: "text", Level: slog.LevelDebug, LogUsage: true},
		Auth:    config.AuthConfig{Enabled: false},
		Models: map[string]config.ModelConfig{
			"fast": {Alias: "fast", Provider: "openrouter", Upstream: "openai/gpt-4o-mini"},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(cfg, nil, logger, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	reloaded := *cfg
	reloaded.Models = map[string]config.ModelConfig{
		"new": {Alias: "new", Provider: "openrouter", Upstream: "openai/gpt-4o"},
	}
	if err := srv.Reload(&reloaded, nil, logger, nil); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if len(payload.Data) != 1 || payload.Data[0].ID != "new" {
		t.Fatalf("reload did not swap models: %+v", payload.Data)
	}

	bad := reloaded
	bad.Server.Listen = "127.0.0.1:1"
	if err := srv.Reload(&bad, nil, logger, nil); err == nil {
		t.Fatal("expected listener change to be rejected")
	}
}

func TestPoliciesRestrictChatAndModelListing(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		cfg.Keys = []config.VirtualKey{
			{Name: "allow-fast", Key: "allow-fast-key", Policy: "only-fast"},
			{Name: "deny-fast", Key: "deny-fast-key", Policy: "no-fast"},
		}
		cfg.Policies = map[string]config.PolicyConfig{
			"only-fast": {Name: "only-fast", Mode: config.PolicyWhitelist, Models: map[string]struct{}{"fast": {}}},
			"no-fast":   {Name: "no-fast", Mode: config.PolicyBlacklist, Models: map[string]struct{}{"fast": {}}},
		}
	})

	// Whitelisted alias succeeds.
	resp := postChat(t, proxy.URL, "allow-fast-key", `{"model":"fast","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whitelisted status = %d, body = %s", resp.StatusCode, body)
	}
	waitForLog(t, logs, `"policy":"only-fast"`)
	waitForLog(t, logs, `"policy_mode":"whitelist"`)
	waitForLog(t, logs, `"policy_models":["fast"]`)

	// Alias outside the whitelist is rejected before reaching upstream.
	resp = postChat(t, proxy.URL, "allow-fast-key", `{"model":"smart","messages":[]}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-whitelisted status = %d, body = %s", resp.StatusCode, body)
	}
	var envelope struct {
		Error struct {
			Code     int `json:"code"`
			Metadata struct {
				ProxyCode string `json:"proxy_code"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if envelope.Error.Code != http.StatusForbidden || envelope.Error.Metadata.ProxyCode != "model_not_allowed" {
		t.Fatalf("error envelope = %+v", envelope.Error)
	}

	// Blacklisted alias is rejected, other aliases are allowed.
	resp = postChat(t, proxy.URL, "deny-fast-key", `{"model":"fast","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blacklisted status = %d", resp.StatusCode)
	}
	resp = postChat(t, proxy.URL, "deny-fast-key", `{"model":"smart","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-blacklisted status = %d", resp.StatusCode)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}

	// /v1/models lists only the aliases each key may use.
	for key, want := range map[string][]string{
		"allow-fast-key": {"fast"},
		"deny-fast-key":  {"smart"},
	} {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET models: %v", err)
		}
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode models: %v", err)
		}
		resp.Body.Close()
		if len(payload.Data) != len(want) {
			t.Fatalf("%s models = %+v, want %v", key, payload.Data, want)
		}
		for i, id := range want {
			if payload.Data[i].ID != id {
				t.Fatalf("%s models = %+v, want %v", key, payload.Data, want)
			}
		}
	}
}

func TestChatAppliesConfiguredExtraBody(t *testing.T) {
	gotBody := make(chan map[string]json.RawMessage, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		gotBody <- fields
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		model := cfg.Models["fast"]
		model.ExtraBody = rawFields(t, `{
			"provider": {"zdr": true, "data_collection": "deny", "order": ["openai"]},
			"transforms": ["middle-out"]
		}`)
		cfg.Models["fast"] = model
	})

	resp := postChat(t, proxy.URL, "test-key", `{
		"model": "fast",
		"messages": [{"role": "user", "content": "hi"}],
		"provider": {"zdr": false, "allow_fallbacks": false}
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	fields := <-gotBody
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != "openai/gpt-4o-mini" {
		t.Fatalf("upstream model = %q, err = %v", model, err)
	}

	var provider map[string]any
	if err := json.Unmarshal(fields["provider"], &provider); err != nil {
		t.Fatalf("decode provider: %v", err)
	}
	if provider["zdr"] != true || provider["data_collection"] != "deny" {
		t.Fatalf("configured provider fields not enforced: %+v", provider)
	}
	if got := provider["order"].([]any); len(got) != 1 || got[0] != "openai" {
		t.Fatalf("configured provider order missing: %+v", provider)
	}
	if provider["allow_fallbacks"] != false {
		t.Fatalf("client provider fields should be preserved: %+v", provider)
	}

	var transforms []string
	if err := json.Unmarshal(fields["transforms"], &transforms); err != nil || len(transforms) != 1 || transforms[0] != "middle-out" {
		t.Fatalf("configured transforms missing: %v, err = %v", transforms, err)
	}
}

func rawFields(t *testing.T, value string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &fields); err != nil {
		t.Fatalf("decode raw fields: %v", err)
	}
	return fields
}

// passthroughProxy builds a proxy whose provider accepts unlisted model ids and
// advertises "/models" on its upstream.
func passthroughProxy(t *testing.T, upstream *httptest.Server, tweak func(*config.Config)) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	return newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		for name, providerCfg := range cfg.Providers {
			providerCfg.PassthroughModels = true
			cfg.Providers[name] = providerCfg
		}
		if tweak != nil {
			tweak(cfg)
		}
	})
}

func TestChatPassesThroughUnlistedModel(t *testing.T) {
	var gotModel string
	var gotAliasHeader, gotUpstreamHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &fields)
		_ = json.Unmarshal(fields["model"], &gotModel)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy, logs := passthroughProxy(t, upstream, nil)

	resp := postChat(t, proxy.URL, "test-key", `{"model":"anthropic/claude-sonnet-5","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if gotModel != "anthropic/claude-sonnet-5" {
		t.Fatalf("upstream model = %q, want unchanged id", gotModel)
	}
	gotAliasHeader = resp.Header.Get("X-Model-Proxy-Alias")
	gotUpstreamHeader = resp.Header.Get("X-Model-Proxy-Upstream")
	if gotAliasHeader != "anthropic/claude-sonnet-5" || gotUpstreamHeader != "anthropic/claude-sonnet-5" {
		t.Fatalf("alias/upstream headers = %q/%q", gotAliasHeader, gotUpstreamHeader)
	}
	waitForLog(t, logs, `"passthrough":true`)
	waitForLog(t, logs, `"upstream_model":"anthropic/claude-sonnet-5"`)
}

func TestResponsesPassesThroughUnlistedModel(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &fields)
		_ = json.Unmarshal(fields["model"], &gotModel)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()

	proxy, logs := passthroughProxy(t, upstream, nil)

	resp := postResponses(t, proxy.URL, "test-key", `{"model":"gpt-5.6-sol","input":[],"stream":false}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if gotModel != "gpt-5.6-sol" {
		t.Fatalf("upstream model = %q, want unchanged id", gotModel)
	}
	waitForLog(t, logs, `"passthrough":true`)
	waitForLog(t, logs, `"prompt_tokens":3`)
}

func TestPassthroughRespectsPolicies(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"fast"},{"id":"denied-model"},{"id":"allowed-model"}]}`))
			return
		}
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy, _ := passthroughProxy(t, upstream, func(cfg *config.Config) {
		cfg.Keys = []config.VirtualKey{
			{Name: "allow-fast", Key: "allow-fast-key", Policy: "only-fast"},
			{Name: "deny-fast", Key: "deny-fast-key", Policy: "no-denied"},
		}
		cfg.Policies = map[string]config.PolicyConfig{
			"only-fast": {Name: "only-fast", Mode: config.PolicyWhitelist, Models: map[string]struct{}{"fast": {}}},
			"no-denied": {Name: "no-denied", Mode: config.PolicyBlacklist, Models: map[string]struct{}{"denied-model": {}}},
		}
	})

	// A whitelist key may not use passthrough ids, even allowed ones.
	resp := postChat(t, proxy.URL, "allow-fast-key", `{"model":"allowed-model","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("whitelist passthrough status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "model_not_allowed") {
		t.Fatalf("unexpected body: %s", body)
	}

	// A blacklist key may use passthrough ids it does not list.
	resp = postChat(t, proxy.URL, "deny-fast-key", `{"model":"allowed-model","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("blacklist passthrough status = %d", resp.StatusCode)
	}

	// A blacklist key may not use a passthrough id it lists.
	resp = postChat(t, proxy.URL, "deny-fast-key", `{"model":"denied-model","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blacklisted passthrough status = %d", resp.StatusCode)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}

	// /v1/models still filters passthrough ids by policy.
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer deny-fast-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	got := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		got = append(got, entry.ID)
	}
	if want := []string{"allowed-model", "fast", "smart"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

func TestPassthroughAmbiguousProvidersRejected(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		// A second passthrough provider makes the upstream for an unlisted id
		// ambiguous.
		cfg.Providers["second"] = config.ProviderConfig{
			Name:                  "second",
			Type:                  "openai-compatible",
			BaseURL:               upstream.URL,
			APIKey:                "key",
			DialTimeout:           time.Second,
			ResponseHeaderTimeout: time.Second,
			PassthroughModels:     true,
		}
		for name, providerCfg := range cfg.Providers {
			if name == "openrouter" {
				providerCfg.PassthroughModels = true
				cfg.Providers[name] = providerCfg
			}
		}
	})

	resp := postChat(t, proxy.URL, "test-key", `{"model":"mystery-model","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "openrouter") || !strings.Contains(string(body), "second") {
		t.Fatalf("error should name both providers: %s", body)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}

	// Configured aliases still work.
	resp = postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alias status = %d", resp.StatusCode)
	}
}

func TestUnknownModelWithoutPassthroughIs404(t *testing.T) {
	proxy, _ := newTestProxy(t, "http://127.0.0.1:1", nil)
	resp := postChat(t, proxy.URL, "test-key", `{"model":"mystery-model","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "model_not_found") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestModelsListsDiscoveredPassthroughModels(t *testing.T) {
	var discoveryCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		discoveryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fast"},{"id":"zeta-model"},{"id":"alpha-model"}]}`))
	}))
	defer upstream.Close()

	proxy, _ := passthroughProxy(t, upstream, nil)

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET models: %v", err)
		}
		var payload struct {
			Data []struct {
				ID            string `json:"id"`
				CanonicalSlug string `json:"canonical_slug"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode models: %v", err)
		}
		resp.Body.Close()

		got := make([]string, 0, len(payload.Data))
		for _, entry := range payload.Data {
			got = append(got, entry.ID)
			if entry.CanonicalSlug != entry.ID {
				t.Fatalf("unexpected metadata for %q: %+v", entry.ID, entry)
			}
		}
		// Aliases and discovered ids are merged and de-duplicated.
		if want := []string{"alpha-model", "fast", "smart", "zeta-model"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}

	if got := discoveryCalls.Load(); got != 1 {
		t.Fatalf("discovery calls = %d, want 1 (cached)", got)
	}
}

func TestModelsSurvivesDiscoveryFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	proxy, logs := passthroughProxy(t, upstream, nil)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(payload.Data) != 2 {
		t.Fatalf("models = %+v, want the two aliases", payload.Data)
	}
	waitForLog(t, logs, `"msg":"model discovery failed"`)
}

func TestPassthroughAppliesProviderExtraBody(t *testing.T) {
	gotBody := make(chan map[string]json.RawMessage, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		gotBody <- fields
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, func(cfg *config.Config) {
		for name, providerCfg := range cfg.Providers {
			providerCfg.PassthroughModels = true
			providerCfg.ExtraBody = map[string]json.RawMessage{
				"provider": json.RawMessage(`{"zdr":true}`),
			}
			cfg.Providers[name] = providerCfg
		}
	})

	resp := postChat(t, proxy.URL, "test-key", `{"model":"zeta-model","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case fields := <-gotBody:
		if string(fields["provider"]) != `{"zdr":true}` {
			t.Fatalf("provider field = %s", fields["provider"])
		}
		if string(fields["model"]) != `"zeta-model"` {
			t.Fatalf("model field = %s", fields["model"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not called")
	}
}

// fakeCatalog is a pricing service that also describes models.
type fakeCatalog struct {
	info  pricing.ModelInfo
	known bool
	cost  float64
}

func (f fakeCatalog) Estimate(string, string, pricing.Usage) (float64, bool) {
	return f.cost, true
}

func (f fakeCatalog) LookupModel(string, string) (pricing.ModelInfo, bool) {
	return f.info, f.known
}

func solCatalog() fakeCatalog {
	return fakeCatalog{
		known: true,
		cost:  7,
		info: pricing.ModelInfo{
			ContextLength:    1050000,
			MaxOutputTokens:  128000,
			InputModalities:  []string{"text", "image", "pdf"},
			OutputModalities: []string{"text"},
			Reasoning:        true,
			ToolCall:         true,
			Attachment:       true,
			Rates:            pricing.Rates{Input: 4, CachedInput: 0.4, Output: 20, CacheWrite: 5},
			RateTable: []pricing.TierRate{
				{MinPromptTokens: 0, Rates: pricing.Rates{Input: 4, CachedInput: 0.4, Output: 20, CacheWrite: 5}},
				{MinPromptTokens: 272000, Rates: pricing.Rates{Input: 8, CachedInput: 0.8, Output: 30, CacheWrite: 10}},
			},
		},
	}
}

func TestModelsOpenRouterShapeUsesCatalogMetadata(t *testing.T) {
	proxy, _ := newTestProxyWithPricing(t, "http://127.0.0.1:1", "openrouter", func(cfg *config.Config) {
		providerCfg := cfg.Providers["openrouter"]
		providerCfg.PricingProvider = "openai"
		cfg.Providers["openrouter"] = providerCfg
	}, solCatalog())

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()

	var payload struct {
		TotalCount int `json:"total_count"`
		Data       []struct {
			ID            string `json:"id"`
			CanonicalSlug string `json:"canonical_slug"`
			Name          string `json:"name"`
			ContextLength int64  `json:"context_length"`
			Architecture  struct {
				Modality         string   `json:"modality"`
				InputModalities  []string `json:"input_modalities"`
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			Pricing struct {
				Prompt         string `json:"prompt"`
				Completion     string `json:"completion"`
				InputCacheRead string `json:"input_cache_read"`
				Overrides      []struct {
					MinPromptTokens int64  `json:"min_prompt_tokens"`
					Prompt          string `json:"prompt"`
				} `json:"overrides"`
			} `json:"pricing"`
			TopProvider struct {
				ContextLength       int64 `json:"context_length"`
				MaxCompletionTokens int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if payload.TotalCount != 2 || len(payload.Data) != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}

	model := payload.Data[0]
	if model.ID != "fast" || model.CanonicalSlug != "fast" || model.Name != "fast" {
		t.Fatalf("unexpected identity: %+v", model)
	}
	if model.ContextLength != 1050000 || model.TopProvider.MaxCompletionTokens != 128000 {
		t.Fatalf("unexpected limits: %+v", model)
	}
	if model.Architecture.Modality != "text+image+file->text" {
		t.Fatalf("unexpected modality: %q", model.Architecture.Modality)
	}
	if model.Pricing.Prompt != "0.000004" || model.Pricing.Completion != "0.00002" || model.Pricing.InputCacheRead != "0.0000004" {
		t.Fatalf("unexpected pricing: %+v", model.Pricing)
	}
	if len(model.Pricing.Overrides) != 1 || model.Pricing.Overrides[0].MinPromptTokens != 272000 || model.Pricing.Overrides[0].Prompt != "0.000008" {
		t.Fatalf("unexpected pricing overrides: %+v", model.Pricing.Overrides)
	}

	params := strings.Join(model.SupportedParameters, ",")
	for _, want := range []string{"tools", "reasoning", "reasoning_effort", "temperature", "max_completion_tokens"} {
		if !strings.Contains(params, want) {
			t.Fatalf("supported_parameters missing %q: %s", want, params)
		}
	}
}

func flatCatalog() fakeCatalog {
	return fakeCatalog{
		known: true,
		cost:  1.4,
		info: pricing.ModelInfo{
			ContextLength:    128000,
			MaxOutputTokens:  16384,
			InputModalities:  []string{"text"},
			OutputModalities: []string{"text"},
			Rates:            pricing.Rates{Input: 4, CachedInput: 4, Output: 20, CacheWrite: 4},
			RateTable: []pricing.TierRate{
				{MinPromptTokens: 0, Rates: pricing.Rates{Input: 4, CachedInput: 4, Output: 20, CacheWrite: 4}},
			},
		},
	}
}

func TestChatInjectsEstimatedCostForAPIBilledProviders(t *testing.T) {
	const upstreamBody = `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100000,"completion_tokens":50000,"total_tokens":150000}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithPricing(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.PricingProvider = "openai"
		cfg.Providers["phala"] = providerCfg
	}, flatCatalog())

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var payload struct {
		Usage struct {
			Cost        *float64 `json:"cost"`
			IsByok      *bool    `json:"is_byok"`
			CostDetails struct {
				Total      float64 `json:"upstream_inference_cost"`
				Prompt     float64 `json:"upstream_inference_prompt_cost"`
				Completion float64 `json:"upstream_inference_completions_cost"`
			} `json:"cost_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body %s: %v", body, err)
	}
	if payload.Usage.Cost == nil || *payload.Usage.Cost != 1.4 {
		t.Fatalf("usage.cost = %v, want 1.4", payload.Usage.Cost)
	}
	if payload.Usage.IsByok == nil || *payload.Usage.IsByok {
		t.Fatalf("usage.is_byok = %v, want false", payload.Usage.IsByok)
	}
	// 100k prompt tokens at $4/M plus 50k completion tokens at $20/M.
	if payload.Usage.CostDetails.Prompt != 0.4 || payload.Usage.CostDetails.Completion != 1 || payload.Usage.CostDetails.Total != 1.4 {
		t.Fatalf("unexpected cost details: %+v", payload.Usage.CostDetails)
	}

	// The access log keeps the estimate labelled as an estimate.
	waitForLog(t, logs, `"estimated_api_cost_usd":1.4`)
	waitForLog(t, logs, `"pricing_source":"models.dev"`)
}

func TestChatStreamingInjectsCostIntoUsageChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			"data: " + `{"choices":[],"usage":{"prompt_tokens":100000,"completion_tokens":50000,"total_tokens":150000}}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxyWithPricing(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.PricingProvider = "openai"
		cfg.Providers["phala"] = providerCfg
	}, flatCatalog())

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[],"stream":true}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	stream := string(body)
	if !strings.Contains(stream, `"cost":1.4`) {
		t.Fatalf("stream missing injected cost: %s", stream)
	}
	if !strings.Contains(stream, `"is_byok":false`) {
		t.Fatalf("stream missing is_byok: %s", stream)
	}
	if strings.Count(stream, `"cost":1.4`) != 1 {
		t.Fatalf("cost injected more than once: %s", stream)
	}
}

func TestChatSkipsCostInjectionForSubscriptionBilling(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":500000,"total_tokens":1500000}}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxyWithPricing(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.PricingProvider = "openai"
		providerCfg.BillingMode = "subscription"
		cfg.Providers["phala"] = providerCfg
	}, solCatalog())

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `"cost"`) {
		t.Fatalf("subscription provider must not report a cost: %s", body)
	}
}

func TestChatKeepsProviderReportedCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.5}}`))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithPricing(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.PricingProvider = "openai"
		cfg.Providers["phala"] = providerCfg
	}, solCatalog())

	resp := postChat(t, proxy.URL, "test-key", `{"model":"fast","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var payload struct {
		Usage struct {
			Cost *float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body %s: %v", body, err)
	}
	if payload.Usage.Cost == nil || *payload.Usage.Cost != 0.5 {
		t.Fatalf("usage.cost = %v, want the reported 0.5", payload.Usage.Cost)
	}
	// Reported costs suppress the estimate in the access log.
	waitForLog(t, logs, `"cost_usd":0.5`)
	if strings.Contains(logs.String(), "estimated_api_cost_usd") {
		t.Fatalf("log should not estimate a reported cost")
	}
}

func TestCORSPreflightAndHeaders(t *testing.T) {
	proxy, _ := newTestProxy(t, "http://127.0.0.1:1", nil)

	req, _ := http.NewRequest(http.MethodOptions, proxy.URL+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Fatalf("allow-headers = %q", got)
	}

	// Preflight must not require a virtual key, and normal responses expose the
	// proxy headers.
	req, _ = http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "X-Model-Proxy-Alias") {
		t.Fatalf("expose-headers = %q", got)
	}
}

func TestAPIV1PathsAreAliased(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxy(t, upstream.URL, nil)

	resp, err := http.Post(proxy.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"fast","messages":[]}`))
	if err != nil {
		t.Fatalf("POST /api/v1/chat/completions: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without key = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/api/v1/chat/completions", strings.NewReader(`{"model":"fast","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST with key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, proxy.URL+"/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/models: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d", resp.StatusCode)
	}
}
