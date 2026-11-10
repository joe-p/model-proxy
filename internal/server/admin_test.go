package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/server"
)

// fakeReloader records calls and returns canned results, so the handlers can be
// tested without touching the filesystem.
type fakeReloader struct {
	calls  atomic.Int32
	forced atomic.Bool
	result server.ReloadResult
	err    error
}

func (f *fakeReloader) ReloadConfig(_ context.Context, force bool) (server.ReloadResult, error) {
	f.calls.Add(1)
	f.forced.Store(force)
	return f.result, f.err
}

func adminServer(t *testing.T, tweak func(*config.Config), rl server.Reloader) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:            "127.0.0.1:0",
			AdminListen:       "127.0.0.1:0",
			AdminToken:        "admin-secret",
			MaxBodyBytes:      1 << 20,
			ReadHeaderTimeout: 1e9,
			ShutdownTimeout:   1e9,
		},
		Logging: config.LoggingConfig{Format: "text", Level: slog.LevelDebug, LogUsage: true},
		Auth:    config.AuthConfig{Enabled: true},
		Keys:    []config.VirtualKey{{Name: "laptop", Key: "test-key"}},
		Policies: map[string]config.PolicyConfig{
			"only-fast": {Name: "only-fast", Mode: config.PolicyWhitelist, Models: map[string]struct{}{"fast": {}}},
		},
		Providers: map[string]config.ProviderConfig{
			"openrouter": {Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "upstream-secret"},
		},
		Models: map[string]config.ModelConfig{
			"fast": {Alias: "fast", Provider: "openrouter", Upstream: "openai/gpt-4o-mini"},
		},
	}
	if tweak != nil {
		tweak(cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(cfg, nil, logger, nil, server.WithReloader(rl))
	ts := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(ts.Close)
	return ts
}

func adminRequest(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestAdminReloadRequiresToken(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	for _, token := range []string{"", "wrong"} {
		resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", token, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, resp.StatusCode)
		}
	}
	if got := rl.calls.Load(); got != 0 {
		t.Fatalf("reloader called %d times for unauthorized requests", got)
	}
}

func TestAdminReloadAcceptsHeaderCredential(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/reload", nil)
	req.Header.Set("X-Model-Proxy-Admin", "admin-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rl.calls.Load() != 1 {
		t.Fatalf("reloader calls = %d, want 1", rl.calls.Load())
	}
}

func TestAdminReloadNotFoundWithoutTokenConfigured(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, func(cfg *config.Config) {
		cfg.Server.AdminToken = ""
		cfg.Server.AdminListen = ""
	}, rl)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if rl.calls.Load() != 0 {
		t.Fatal("unconfigured admin api must not reload")
	}
}

func TestAdminReloadNotFoundWithoutReloader(t *testing.T) {
	ts := adminServer(t, nil, nil)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminReloadSuccessReportsGeneration(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{
		Changed:    true,
		ConfigHash: "abc123",
		Models:     2,
		Providers:  1,
		Keys:       1,
		Policies:   1,
	}}
	ts := adminServer(t, nil, rl)

	body := decodeJSON[struct {
		Status     string `json:"status"`
		Changed    bool   `json:"changed"`
		ConfigHash string `json:"config_hash"`
		Models     int    `json:"models"`
		Generation uint64 `json:"generation"`
	}](t, adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil))

	if body.Status != "ok" || !body.Changed || body.ConfigHash != "abc123" || body.Models != 2 {
		t.Fatalf("unexpected payload: %+v", body)
	}
	if body.Generation != 1 {
		t.Fatalf("generation = %d, want 1", body.Generation)
	}
}

func TestAdminReloadUnchangedKeepsGeneration(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: false, ConfigHash: "abc123"}}
	ts := adminServer(t, nil, rl)

	for i := 0; i < 2; i++ {
		body := decodeJSON[struct {
			Changed    bool   `json:"changed"`
			Generation uint64 `json:"generation"`
		}](t, adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil))
		if body.Changed {
			t.Fatalf("attempt %d: expected changed=false", i)
		}
		if body.Generation != 0 {
			t.Fatalf("attempt %d: generation = %d, want 0 for a no-op", i, body.Generation)
		}
	}
}

func TestAdminReloadForceFlag(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload?force=1", "admin-secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !rl.forced.Load() {
		t.Fatal("force query parameter was not forwarded")
	}
}

func TestAdminReloadRejectsRequestBody(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret",
		strings.NewReader(`{"models":{}}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if rl.calls.Load() != 0 {
		t.Fatal("a request body must not trigger a reload")
	}
}

func TestAdminReloadRequiresRestartIsConflict(t *testing.T) {
	rl := &fakeReloader{err: fmt.Errorf("server.listen changes require a restart: %w", server.ErrReloadRequiresRestart)}
	ts := adminServer(t, nil, rl)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil)
	body := decodeJSON[struct {
		Error struct {
			Message string         `json:"message"`
			Code    int            `json:"code"`
			Meta    map[string]any `json:"metadata"`
		} `json:"error"`
	}](t, resp)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(body.Error.Message, "restart") {
		t.Fatalf("unexpected message: %q", body.Error.Message)
	}
	if body.Error.Meta["proxy_code"] != "reload_requires_restart" {
		t.Fatalf("unexpected code: %v", body.Error.Meta["proxy_code"])
	}
}

func TestAdminReloadInvalidConfigIsUnprocessable(t *testing.T) {
	rl := &fakeReloader{err: errors.New(`parse config: unknown field "nope"`)}
	ts := adminServer(t, nil, rl)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil)
	body := decodeJSON[struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}](t, resp)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body.Error.Message, "unknown field") {
		t.Fatalf("unexpected message: %q", body.Error.Message)
	}
}

func TestAdminReloadRejectsWrongMethod(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	resp := adminRequest(t, http.MethodGet, ts.URL+"/admin/reload", "admin-secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// The reload endpoint must never be reachable on the public mux.
func TestReloadNotExposedOnPublicMux(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	// Rebuild the same config with the public handler instead of the admin one.
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: "127.0.0.1:0",
			// Even a fully configured admin API must not leak onto the public mux.
			AdminListen: "127.0.0.1:1", AdminToken: "admin-secret",
			MaxBodyBytes: 1 << 20, ReadHeaderTimeout: 1e9, ShutdownTimeout: 1e9,
		},
		Logging: config.LoggingConfig{Format: "text", Level: slog.LevelDebug},
		Auth:    config.AuthConfig{Enabled: false},
		Models:  map[string]config.ModelConfig{"fast": {Alias: "fast", Provider: "openrouter", Upstream: "openai/gpt-4o-mini"}},
	}
	public := httptest.NewServer(server.New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, server.WithReloader(rl)))
	defer public.Close()
	_ = ts

	resp := adminRequest(t, http.MethodPost, public.URL+"/admin/reload", "admin-secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on the public listener", resp.StatusCode)
	}
	if rl.calls.Load() != 0 {
		t.Fatal("public listener triggered a reload")
	}
}

func TestAdminStatusOmitsSecrets(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true, ConfigHash: "abc123"}}
	ts := adminServer(t, nil, rl)

	// Apply one reload so generation and hash are populated.
	adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil).Body.Close()

	resp := adminRequest(t, http.MethodGet, ts.URL+"/admin/status", "admin-secret", nil)
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	text := string(raw)
	for _, secret := range []string{"admin-secret", "test-key", "upstream-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("status response leaked %q: %s", secret, text)
		}
	}

	var status struct {
		Status          string `json:"status"`
		ReloadAvailable bool   `json:"reload_available"`
		Generation      uint64 `json:"generation"`
		ConfigHash      string `json:"config_hash"`
		Models          []struct {
			Alias string `json:"alias"`
		} `json:"models"`
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
		Keys []struct {
			Name string `json:"name"`
		} `json:"virtual_keys"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Status != "ok" || !status.ReloadAvailable || status.Generation != 1 || status.ConfigHash != "abc123" {
		t.Fatalf("unexpected status payload: %s", text)
	}
	if len(status.Models) != 1 || status.Models[0].Alias != "fast" {
		t.Fatalf("unexpected models: %+v", status.Models)
	}
	if len(status.Providers) != 1 || status.Providers[0].Name != "openrouter" {
		t.Fatalf("unexpected providers: %+v", status.Providers)
	}
	if len(status.Keys) != 1 || status.Keys[0].Name != "laptop" {
		t.Fatalf("unexpected keys: %+v", status.Keys)
	}
}

func TestAdminStatusReportsLastError(t *testing.T) {
	rl := &fakeReloader{err: errors.New("read config: no such file")}
	ts := adminServer(t, nil, rl)

	adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil).Body.Close()

	status := decodeJSON[struct {
		LastError string `json:"last_reload_error"`
	}](t, adminRequest(t, http.MethodGet, ts.URL+"/admin/status", "admin-secret", nil))
	if !strings.Contains(status.LastError, "no such file") {
		t.Fatalf("last_reload_error = %q", status.LastError)
	}
}

func TestAdminApiDoesNotEmitCORSHeaders(t *testing.T) {
	rl := &fakeReloader{result: server.ReloadResult{Changed: true}}
	ts := adminServer(t, nil, rl)

	resp := adminRequest(t, http.MethodPost, ts.URL+"/admin/reload", "admin-secret", nil)
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("admin response advertised CORS origin %q", got)
	}

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/admin/reload", nil)
	preflight, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	preflight.Body.Close()
	if got := preflight.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("admin preflight advertised CORS origin %q", got)
	}
}

func TestReloadRejectsAdminListenerAndCredentialChanges(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:            "127.0.0.1:0",
			AdminListen:       "127.0.0.1:0",
			AdminToken:        "admin-secret",
			MaxBodyBytes:      1 << 20,
			ReadHeaderTimeout: 1e9,
			ShutdownTimeout:   1e9,
		},
		Logging: config.LoggingConfig{Format: "text", Level: slog.LevelDebug},
		Auth:    config.AuthConfig{Enabled: false},
		Models:  map[string]config.ModelConfig{"fast": {Alias: "fast", Provider: "openrouter", Upstream: "openai/gpt-4o-mini"}},
	}
	srv := server.New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	cases := map[string]func(*config.Config){
		"admin_listen": func(c *config.Config) { c.Server.AdminListen = "0.0.0.0:8081" },
		"admin_token":  func(c *config.Config) { c.Server.AdminToken = "rotated" },
	}
	for name, mutate := range cases {
		changed := *cfg
		mutate(&changed)
		err := srv.Reload(&changed, nil, nil, nil)
		if err == nil {
			t.Fatalf("%s: expected the change to be rejected", name)
		}
		if !errors.Is(err, server.ErrReloadRequiresRestart) {
			t.Fatalf("%s: error should wrap ErrReloadRequiresRestart: %v", name, err)
		}
	}
}
