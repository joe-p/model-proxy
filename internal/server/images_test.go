package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/server"
)

func postImages(t *testing.T, proxyURL, key, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestImagesGenerationsRewritesModelAndStreamsResponse(t *testing.T) {
	const responseBody = `{"created":1748372400,"data":[{"b64_json":"aW1n","media_type":"image/png"}],"usage":{"prompt_tokens":0,"completion_tokens":10,"total_tokens":10,"cost":0.04}}`

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Method != http.MethodPost {
			t.Errorf("upstream method = %s", r.Method)
		}
		if r.URL.Path != "/images/generations" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "foo=bar" {
			t.Errorf("upstream query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Errorf("upstream authorization = %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		var model string
		_ = json.Unmarshal(fields["model"], &model)
		if model != "openai/gpt-image-1" {
			t.Errorf("upstream model = %q", model)
		}
		// Fields the OpenAI images client sends are forwarded untouched.
		for _, field := range []string{"prompt", "size", "n", "response_format"} {
			if _, ok := fields[field]; !ok {
				t.Errorf("field %q was not passed through", field)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		cfg.Models["image"] = config.ModelConfig{
			Alias:    "image",
			Provider: "phala",
			Upstream: "openai/gpt-image-1",
		}
	})
	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations?foo=bar",
		`{"model":"image","prompt":"a red panda","size":"1024x1024","n":1,"response_format":"b64_json"}`)
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, gotBody)
	}
	if string(gotBody) != responseBody {
		t.Fatalf("body changed:\nwant %s\ngot  %s", responseBody, gotBody)
	}
	if got := resp.Header.Get("X-Model-Proxy-Alias"); got != "image" {
		t.Fatalf("alias header = %q", got)
	}
	if got := resp.Header.Get("X-Model-Proxy-Upstream"); got != "openai/gpt-image-1" {
		t.Fatalf("upstream header = %q", got)
	}
	if got := resp.Header.Get("X-Upstream"); got != "yes" {
		t.Fatalf("upstream header was not copied: %q", got)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d", upstreamCalls)
	}

	waitForLog(t, logs, `"path":"/v1/images/generations"`)
	waitForLog(t, logs, `"alias":"image"`)
	waitForLog(t, logs, `"upstream_model":"openai/gpt-image-1"`)
	waitForLog(t, logs, `"prompt_tokens":0`)
	waitForLog(t, logs, `"completion_tokens":10`)
	waitForLog(t, logs, `"total_tokens":10`)
	waitForLog(t, logs, `"cost_usd":0.04`)
}

func TestImagesGenerationsHonorsProviderImagesPath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[]}`))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.ImagesPath = "/images"
		cfg.Providers["phala"] = providerCfg
		cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "img"}
	})

	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations", `{"model":"image","prompt":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotPath != "/images" {
		t.Fatalf("upstream path = %q", gotPath)
	}
}

func TestImagesGenerationsServesPathAliases(t *testing.T) {
	for _, path := range []string{"/v1/images", "/v1/images/generations", "/api/v1/images", "/api/v1/images/generations"} {
		t.Run(path, func(t *testing.T) {
			var gotPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1,"data":[]}`))
			}))
			defer upstream.Close()

			proxy, _ := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
				cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "img"}
			})

			resp := postImages(t, proxy.URL, "test-key", path, `{"model":"image","prompt":"x"}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if gotPath != "/images/generations" {
				t.Fatalf("upstream path = %q", gotPath)
			}
		})
	}
}

func TestImagesGenerationsRequiresModel(t *testing.T) {
	proxy, _ := newTestProxyWithProvider(t, "http://127.0.0.1:1", "openai-compatible", nil)
	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations", `{"prompt":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestImagesGenerationsRejectsUnsupportedProvider(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:            "127.0.0.1:0",
			MaxBodyBytes:      1 << 20,
			ReadHeaderTimeout: time.Second,
			ShutdownTimeout:   time.Second,
		},
		Logging: config.LoggingConfig{Format: "json", Level: slog.LevelDebug},
		Auth:    config.AuthConfig{Enabled: true},
		Keys:    []config.VirtualKey{{Name: "test", Key: "test-key"}},
		Providers: map[string]config.ProviderConfig{
			"chat-only": {Name: "chat-only", Type: "openai-compatible"},
		},
		Models: map[string]config.ModelConfig{
			"image": {Alias: "image", Provider: "chat-only", Upstream: "img"},
		},
	}
	srv := server.New(cfg, map[string]provider.Provider{"chat-only": chatOnlyProvider{}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations", `{"model":"image","prompt":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// chatOnlyProvider implements provider.Provider without provider.ImagesProvider.
type chatOnlyProvider struct{}

func (chatOnlyProvider) Name() string { return "chat-only" }

func (chatOnlyProvider) ChatCompletions(context.Context, provider.Request) (*http.Response, error) {
	return nil, &provider.Error{Kind: provider.Unavailable}
}

func (chatOnlyProvider) Responses(context.Context, provider.Request) (*http.Response, error) {
	return nil, &provider.Error{Kind: provider.Unavailable}
}

func TestImagesGenerationsLogsOpenAIUsageWithoutBufferingBody(t *testing.T) {
	// A large payload proves usage observation does not rewrite or truncate
	// the body.
	image := strings.Repeat("A", 256<<10)
	responseBody := fmt.Sprintf(
		`{"created":1,"data":[{"b64_json":%q}],"usage":{"input_tokens":50,"output_tokens":4160,"total_tokens":4210,"input_tokens_details":{"image_tokens":0,"text_tokens":50}}}`,
		image,
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "gpt-image-1"}
	})

	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations", `{"model":"image","prompt":"x"}`)
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(gotBody) != responseBody {
		t.Fatalf("body changed: got %d bytes, want %d", len(gotBody), len(responseBody))
	}

	waitForLog(t, logs, `"prompt_tokens":50`)
	waitForLog(t, logs, `"completion_tokens":4160`)
	waitForLog(t, logs, `"total_tokens":4210`)
}

func TestImagesGenerationsEstimatesCostWhenUpstreamOmitsIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithPricing(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.PricingProvider = "test"
		cfg.Providers["phala"] = providerCfg
		cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "img"}
	}, fakeEstimator{cost: 0.25})

	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations", `{"model":"image","prompt":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	waitForLog(t, logs, `"estimated_api_cost_usd":0.25`)
	waitForLog(t, logs, `"pricing_source":"models.dev"`)
}

func TestImagesGenerationsForwardsNonJSONBody(t *testing.T) {
	const responseBody = `data: {"type":"image_generation.partial_image"}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	proxy, _ := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "img"}
	})

	resp := postImages(t, proxy.URL, "test-key", "/v1/images/generations", `{"model":"image","prompt":"x"}`)
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(gotBody) != responseBody {
		t.Fatalf("body changed:\nwant %q\ngot  %q", responseBody, gotBody)
	}
}
