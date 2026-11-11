package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/server"
)

const reloadTestConfig = `{
  "server": {"listen": "127.0.0.1:0"},
  "logging": {"level": "error"},
  "key_file": "keys.json",
  "providers": {
    "openrouter": {"base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_KEY"}
  },
  "models": {
    %s
  }
}`

// writeReloadConfig rewrites the config file with the given models block.
func writeReloadConfig(t *testing.T, path, models string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "keys.json"), []byte(`[{"name": "laptop", "key_env": "PROXY_KEY"}]`), 0o600); err != nil {
		t.Fatalf("write keys: %v", err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(reloadTestConfig, models)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestReloaderAppliesChangedConfig(t *testing.T) {
	t.Setenv("PROXY_KEY", "test-key")
	t.Setenv("OPENROUTER_KEY", "upstream-key")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeReloadConfig(t, path, `"fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	logs, _, err := newLogRuntime(cfg, nil)
	if err != nil {
		t.Fatalf("newLogRuntime: %v", err)
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	srv := server.New(cfg, providers, logs.logger, nil, server.WithReloader(nil))
	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	rl := newReloader(cfg, logs, nil)
	rl.attach(srv)

	// An untouched file is a no-op, so GitOps loops stay cheap.
	res, err := rl.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("reload unchanged: %v", err)
	}
	if res.Changed {
		t.Fatal("unchanged config must not report a change")
	}
	if res.Models != 1 || res.Providers != 1 || res.Keys != 1 {
		t.Fatalf("no-op result should still describe the applied config: %+v", res)
	}

	writeReloadConfig(t, path, `"slow": {"provider": "openrouter", "upstream": "openai/gpt-4o"}`)
	res, err = rl.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("reload changed: %v", err)
	}
	if !res.Changed || res.Models != 1 || res.Providers != 1 || res.Keys != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// A second no-op must describe the newly applied config, not the startup one.
	again, err := rl.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("reload unchanged after change: %v", err)
	}
	if again.Changed || again.ConfigHash != res.ConfigHash {
		t.Fatalf("expected a no-op with the same hash: %+v", again)
	}
	if res.ConfigHash == "" {
		t.Fatal("expected a config hash")
	}
	if got := modelIDs(t, proxy.URL); len(got) != 1 || got[0] != "slow" {
		t.Fatalf("reload did not swap models: %v", got)
	}

	// A broken file must be rejected and leave the running config alone.
	if err := os.WriteFile(path, []byte(`{"server": {"nope": 1}}`), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	if _, err := rl.ReloadConfig(context.Background(), false); err == nil {
		t.Fatal("expected an invalid config to be rejected")
	}
	if got := modelIDs(t, proxy.URL); len(got) != 1 || got[0] != "slow" {
		t.Fatalf("a failed reload changed the running config: %v", got)
	}

	// Restoring the file and forcing a rebuild reports a change again.
	writeReloadConfig(t, path, `"slow": {"provider": "openrouter", "upstream": "openai/gpt-4o"}`)
	res, err = rl.ReloadConfig(context.Background(), true)
	if err != nil {
		t.Fatalf("forced reload: %v", err)
	}
	if !res.Changed {
		t.Fatal("a forced reload must report a change")
	}
}

func modelIDs(t *testing.T, baseURL string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read models: %v", err)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode models: %v (%s)", err, raw)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestReloaderDetectsKeyFileChange(t *testing.T) {
	t.Setenv("OPENROUTER_KEY", "upstream-key")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	keyPath := filepath.Join(dir, "keys.json")
	configData := `{
  "server": {"listen": "127.0.0.1:0"},
  "logging": {"level": "error"},
  "key_file": "keys.json",
  "providers": {
    "openrouter": {"base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_KEY"}
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	writeKeys := func(name string) {
		t.Helper()
		body := fmt.Sprintf(`[{"name": "%s", "key": "%s-key"}]`, name, name)
		if err := os.WriteFile(keyPath, []byte(body), 0o600); err != nil {
			t.Fatalf("write keys: %v", err)
		}
	}
	writeKeys("first")

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KeyFile != keyPath {
		t.Fatalf("key file = %q, want %q", cfg.KeyFile, keyPath)
	}
	logs, _, err := newLogRuntime(cfg, nil)
	if err != nil {
		t.Fatalf("newLogRuntime: %v", err)
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	srv := server.New(cfg, providers, logs.logger, nil, server.WithReloader(nil))
	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	rl := newReloader(cfg, logs, nil)
	rl.attach(srv)

	res, err := rl.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("reload unchanged: %v", err)
	}
	if res.Changed {
		t.Fatal("unchanged config and key file must not report a change")
	}

	writeKeys("second")
	res, err = rl.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("reload changed key file: %v", err)
	}
	if !res.Changed || res.Keys != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer second-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new key status = %d, want 200", resp.StatusCode)
	}
}
