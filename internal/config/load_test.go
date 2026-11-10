package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseConfig = `
[[virtual_keys]]
name = "laptop"
key_env = "PROXY_KEY"

[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_KEY"

[models.fast]
provider = "openrouter"
upstream = "openai/gpt-4o-mini"
`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	old, ok := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, old)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func TestLoadDefaultsAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "PROXY_KEY=from-file\nOPENROUTER_KEY=upstream-from-file\n")
	path := writeFile(t, dir, "config.toml", "env_file = \".env\"\n"+baseConfig)

	t.Setenv("PROXY_KEY", "from-process")
	unsetEnv(t, "OPENROUTER_KEY")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Auth.Enabled {
		t.Fatal("auth should default to enabled")
	}
	if cfg.Logging.Format != "json" || cfg.Logging.Level != slog.LevelInfo || !cfg.Logging.LogUsage {
		t.Fatalf("unexpected logging defaults: %+v", cfg.Logging)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" || cfg.Server.MaxBodyBytes != 1<<20 {
		t.Fatalf("unexpected server defaults: %+v", cfg.Server)
	}
	if cfg.Server.ReadHeaderTimeout != 10*time.Second || cfg.Server.ShutdownTimeout != 15*time.Second {
		t.Fatalf("unexpected timeout defaults: %+v", cfg.Server)
	}
	if got := cfg.Keys[0].Key; got != "from-process" {
		t.Fatalf("process env should win, got %q", got)
	}
	if got := cfg.Providers["openrouter"].APIKey; got != "upstream-from-file" {
		t.Fatalf("env file API key not loaded, got %q", got)
	}
	if cfg.EnvFile != filepath.Join(dir, ".env") {
		t.Fatalf("unexpected env file path: %q", cfg.EnvFile)
	}
	if got := cfg.Models["fast"].Upstream; got != "openai/gpt-4o-mini" {
		t.Fatalf("unexpected upstream: %q", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", baseConfig+"\n[server]\nunknown_setting = true\n")
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "strict mode") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadRejectsPlaintextKeyField(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", `
[[virtual_keys]]
name = "laptop"
key = "plaintext"
`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "strict mode") {
		t.Fatalf("expected unknown field error for key, got %v", err)
	}
}

func TestLoadRejectsDuplicateResolvedKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", `
[[virtual_keys]]
name = "one"
key_env = "KEY_ONE"
[[virtual_keys]]
name = "two"
key_env = "KEY_TWO"

[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_KEY"

[models.fast]
provider = "openrouter"
upstream = "openai/gpt-4o-mini"
`)
	t.Setenv("KEY_ONE", "same")
	t.Setenv("KEY_TWO", "same")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "same value") {
		t.Fatalf("expected duplicate key error, got %v", err)
	}
}

func TestLoadRejectsMissingEnv(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", baseConfig)
	unsetEnv(t, "PROXY_KEY")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "PROXY_KEY") {
		t.Fatalf("expected missing env error, got %v", err)
	}
}

func TestLoadRequiresKeysWhenAuthEnabled(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", `
[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_KEY"

[models.fast]
provider = "openrouter"
upstream = "openai/gpt-4o-mini"
`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no virtual keys") {
		t.Fatalf("expected no virtual keys error, got %v", err)
	}
}

func TestLoadAllowsNoKeysWhenAuthDisabled(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", `
[auth]
enabled = false

[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_KEY"

[models.fast]
provider = "openrouter"
upstream = "openai/gpt-4o-mini"
`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Enabled || len(cfg.Keys) != 0 {
		t.Fatalf("unexpected auth config: %+v", cfg.Auth)
	}
}

func TestLoadRejectsUnknownProviderReference(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", `
[auth]
enabled = false

[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_KEY"

[models.fast]
provider = "missing"
upstream = "openai/gpt-4o-mini"
`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", baseConfig+"\n[server]\nread_header_timeout = \"soon\"\n")
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "read_header_timeout") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

func TestLoadRejectsMissingEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", "env_file = \".env\"\n"+baseConfig)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "env file") {
		t.Fatalf("expected env file error, got %v", err)
	}
}
