package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseConfig = `{
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`

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
	path := writeFile(t, dir, "config.json", `{
  "env_file": ".env",
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)

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

func TestLoadResolvesProviderTypes(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    },
    "phala": {
      "type": "openai-compatible",
      "base_url": "https://inference.phala.com/v1",
      "api_key_env": "PHALA_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"},
    "private": {"provider": "phala", "upstream": "z-ai/glm-5.3"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")
	t.Setenv("PHALA_KEY", "phala-upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["openrouter"].Type; got != "openrouter" {
		t.Fatalf("default provider type = %q, want name", got)
	}
	if got := cfg.Providers["phala"].Type; got != "openai-compatible" {
		t.Fatalf("explicit provider type = %q", got)
	}
}

func TestLoadAcceptsCommentsAndTrailingCommas(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  // Line comments are ignored.
  "auth": {"enabled": true}, /* Block comments are ignored too. */
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"},
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1", // "//" inside strings is preserved
      "api_key_env": "OPENROUTER_KEY",
    },
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"},
  },
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["openrouter"].BaseURL; got != "https://openrouter.ai/api/v1" {
		t.Fatalf("unexpected base URL: %q", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "server": {"unknown_setting": true},
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", baseConfig+"\n{}")
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected trailing data error")
	}
}

func TestLoadRejectsPlaintextKeyField(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "laptop", "key": "plaintext"}
  ]
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error for key, got %v", err)
	}
}

func TestLoadRejectsDuplicateResolvedKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "one", "key_env": "KEY_ONE"},
    {"name": "two", "key_env": "KEY_TWO"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
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
	path := writeFile(t, dir, "config.json", baseConfig)
	unsetEnv(t, "PROXY_KEY")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "PROXY_KEY") {
		t.Fatalf("expected missing env error, got %v", err)
	}
}

func TestLoadRequiresKeysWhenAuthEnabled(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no virtual keys") {
		t.Fatalf("expected no virtual keys error, got %v", err)
	}
}

func TestLoadAllowsNoKeysWhenAuthDisabled(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
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
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "missing", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "server": {"read_header_timeout": "soon"},
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "read_header_timeout") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

func TestLoadRejectsMissingEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "env_file": ".env",
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "env file") {
		t.Fatalf("expected env file error, got %v", err)
	}
}

func TestLoadResolvesPoliciesAndKeyReferences(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY", "policy": "basic"},
    {"name": "unrestricted", "key_env": "PROXY_KEY_2"}
  ],
  "policies": [
    {"name": "basic", "mode": "whitelist", "models": ["fast"]}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"},
    "smart": {"provider": "openrouter", "upstream": "anthropic/claude-sonnet-4"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("PROXY_KEY_2", "secret-2")
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	policy := cfg.Policies["basic"]
	if policy.Name != "basic" || policy.Mode != PolicyWhitelist {
		t.Fatalf("unexpected policy: %+v", policy)
	}
	if _, ok := policy.Models["fast"]; !ok {
		t.Fatalf("policy models missing fast: %+v", policy.Models)
	}
	if cfg.Keys[0].Policy != "basic" || cfg.Keys[1].Policy != "" {
		t.Fatalf("unexpected key policies: %+v", cfg.Keys)
	}
	if !cfg.ModelAllowed(&cfg.Keys[0], "fast") {
		t.Fatal("whitelisted model should be allowed")
	}
	if cfg.ModelAllowed(&cfg.Keys[0], "smart") {
		t.Fatal("non-whitelisted model should be denied")
	}
	if !cfg.ModelAllowed(&cfg.Keys[1], "smart") {
		t.Fatal("key without policy should allow every model")
	}
}

func TestLoadResolvesVirtualKeyUserAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "custom", "key_env": "PROXY_KEY", "user_agent": "  my-client/2.0  "},
    {"name": "default", "key_env": "PROXY_KEY_2"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("PROXY_KEY_2", "secret-2")
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Keys[0].UserAgent; got != "my-client/2.0" {
		t.Fatalf("user agent = %q, want trimmed override", got)
	}
	if got := cfg.Keys[1].UserAgent; got != "" {
		t.Fatalf("user agent = %q, want empty default", got)
	}
}

func TestLoadRejectsInvalidVirtualKeyUserAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "bad", "key_env": "PROXY_KEY", "user_agent": "client\r\nX-Injected: yes"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "user_agent") {
		t.Fatalf("expected user_agent error, got %v", err)
	}
}

func TestLoadRejectsUnknownPolicyReference(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY", "policy": "missing"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Fatalf("expected unknown policy error, got %v", err)
	}
}

func TestLoadRejectsUnknownModelInPolicy(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "policies": [
    {"name": "basic", "mode": "blacklist", "models": ["missing"]}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected unknown model error, got %v", err)
	}
}

func TestLoadRejectsInvalidPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   string
	}{
		{name: "mode", policy: `{"name": "p", "mode": "allow", "models": ["fast"]}`, want: "mode"},
		{name: "empty models", policy: `{"name": "p", "mode": "whitelist", "models": []}`, want: "models"},
		{name: "duplicate name", policy: `{"name": "p", "mode": "whitelist", "models": ["fast"]}, {"name": "p", "mode": "blacklist", "models": ["fast"]}`, want: "duplicate policy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "policies": [`+tc.policy+`],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
			t.Setenv("OPENROUTER_KEY", "upstream")
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadMergesExtraBody(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY",
      "extra_body": {
        "provider": {"zdr": true, "data_collection": "deny"}
      }
    }
  },
  "models": {
    "fast": {
      "provider": "openrouter",
      "upstream": "openai/gpt-4o-mini",
      "extra_body": {
        "provider": {"order": ["openai"]},
        "transforms": ["middle-out"]
      }
    },
    "smart": {
      "provider": "openrouter",
      "upstream": "anthropic/claude-sonnet-4"
    }
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	fast := decodeExtraBody(t, cfg.Models["fast"].ExtraBody)
	provider := fast["provider"].(map[string]any)
	if provider["zdr"] != true || provider["data_collection"] != "deny" {
		t.Fatalf("provider defaults missing from fast: %+v", provider)
	}
	if got := provider["order"].([]any); len(got) != 1 || got[0] != "openai" {
		t.Fatalf("model provider override missing: %+v", provider)
	}
	if got := fast["transforms"].([]any); len(got) != 1 || got[0] != "middle-out" {
		t.Fatalf("model transforms missing: %+v", fast)
	}

	smart := decodeExtraBody(t, cfg.Models["smart"].ExtraBody)
	smartProvider := smart["provider"].(map[string]any)
	if smartProvider["zdr"] != true || smartProvider["data_collection"] != "deny" {
		t.Fatalf("provider defaults missing from smart: %+v", smartProvider)
	}
	if _, ok := smartProvider["order"]; ok {
		t.Fatalf("model override leaked into sibling model: %+v", smartProvider)
	}
}

func TestLoadRejectsReservedExtraBodyFields(t *testing.T) {
	for _, field := range []string{"model", "messages", "stream", "models", "route", "plugins"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {
      "provider": "openrouter",
      "upstream": "openai/gpt-4o-mini",
      "extra_body": {"`+field+`": true}
    }
  }
}`)
			t.Setenv("OPENROUTER_KEY", "upstream")

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "may not be set") {
				t.Fatalf("expected reserved field error, got %v", err)
			}
		})
	}
}

func TestLoadResolvesCodexAuthDir(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "codex": {
      "type": "codex",
      "base_url": "https://chatgpt.com/backend-api/codex",
      "auth_dir": "credentials"
    }
  },
  "models": {
    "gpt": {"provider": "codex", "upstream": "gpt-5"}
  }
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	provider := cfg.Providers["codex"]
	if provider.Type != "codex" {
		t.Fatalf("type = %q", provider.Type)
	}
	if provider.APIKey != "" {
		t.Fatalf("api key = %q, want empty", provider.APIKey)
	}
	if want := filepath.Join(dir, "credentials"); provider.AuthDir != want {
		t.Fatalf("auth dir = %q, want %q", provider.AuthDir, want)
	}
}

func TestLoadCodexAuthDirDefaultsToCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "codex": {
      "type": "codex",
      "base_url": "https://chatgpt.com/backend-api/codex"
    }
  },
  "models": {
    "gpt": {"provider": "codex", "upstream": "gpt-5"}
  }
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["codex"].AuthDir; got != home {
		t.Fatalf("auth dir = %q, want %q", got, home)
	}
}

func TestLoadResolvesPricing(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "pricing": {
    "source": "models.dev",
    "url": "https://example.test/catalog.json",
    "cache": "cache/models.dev.json",
    "refresh_interval": "1h",
    "timeout": "5s"
  },
  "providers": {
    "codex": {
      "type": "codex",
      "base_url": "https://chatgpt.com/backend-api/codex"
    }
  },
  "models": {
    "gpt": {
      "provider": "codex",
      "upstream": "gpt-6.1-sol",
      "pricing": {
        "input_per_million": 2,
        "cached_input_per_million": 0.1,
        "output_per_million": 10
      }
    }
  }
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pricing.Enabled() {
		t.Fatal("pricing should be enabled")
	}
	if cfg.Pricing.URL != "https://example.test/catalog.json" {
		t.Fatalf("url = %q", cfg.Pricing.URL)
	}
	if want := filepath.Join(dir, "cache", "models.dev.json"); cfg.Pricing.Cache != want {
		t.Fatalf("cache = %q, want %q", cfg.Pricing.Cache, want)
	}
	if cfg.Pricing.RefreshInterval != time.Hour {
		t.Fatalf("refresh interval = %v", cfg.Pricing.RefreshInterval)
	}
	if cfg.Pricing.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v", cfg.Pricing.Timeout)
	}
	if got := cfg.Providers["codex"].PricingProvider; got != "openai" {
		t.Fatalf("pricing provider = %q, want openai", got)
	}
	pricing := cfg.Models["gpt"].Pricing
	if pricing == nil || pricing.InputPerMillion != 2 || pricing.OutputPerMillion != 10 {
		t.Fatalf("model pricing = %+v", pricing)
	}
	if pricing.CachedInputPerMillion == nil || *pricing.CachedInputPerMillion != 0.1 {
		t.Fatalf("cached pricing = %v", pricing.CachedInputPerMillion)
	}
}

func TestLoadPricingDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", baseConfig)
	t.Setenv("PROXY_KEY", "secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pricing.Enabled() {
		t.Fatalf("pricing should be disabled: %+v", cfg.Pricing)
	}
}

func TestLoadRejectsUnknownPricingSource(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "pricing": {"source": "elsewhere"},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "pricing.source") {
		t.Fatalf("expected pricing source error, got %v", err)
	}
}

func TestLoadRejectsIncompleteModelPricing(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {
      "provider": "openrouter",
      "upstream": "openai/gpt-4o-mini",
      "pricing": {"input_per_million": 2}
    }
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "input_per_million") {
		t.Fatalf("expected pricing error, got %v", err)
	}
}

func TestLoadCodexBillingModeDefaultsToSubscription(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "codex": {
      "type": "codex",
      "base_url": "https://chatgpt.com/backend-api/codex"
    },
    "other": {
      "type": "openai-compatible",
      "base_url": "https://example.test/v1",
      "api_key_env": "OTHER_KEY"
    }
  },
  "models": {
    "gpt": {"provider": "codex", "upstream": "gpt-5"}
  }
}`)
	t.Setenv("OTHER_KEY", "key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["codex"].BillingMode; got != "subscription" {
		t.Fatalf("codex billing mode = %q, want subscription", got)
	}
	if got := cfg.Providers["other"].BillingMode; got != "" {
		t.Fatalf("other billing mode = %q, want empty", got)
	}
}

func TestLoadEnablesClaudeSubscriptionWithoutModels(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "enable_claude_subscription": true
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EnableClaudeSubscription {
		t.Fatal("claude subscription was not enabled")
	}
	if len(cfg.Models) != 0 {
		t.Fatalf("models = %d, want 0", len(cfg.Models))
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("providers = %d, want 0", len(cfg.Providers))
	}
}

func TestLoadClaudeSubscriptionDefaultsToDisabled(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EnableClaudeSubscription {
		t.Fatal("claude subscription should default to disabled")
	}
}

func TestLoadRejectsAuthDirForNonCodexProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "custom": {
      "type": "openai-compatible",
      "base_url": "https://example.test/v1",
      "api_key_env": "CUSTOM_KEY",
      "auth_dir": "credentials"
    }
  },
  "models": {
    "m": {"provider": "custom", "upstream": "m"}
  }
}`)
	t.Setenv("CUSTOM_KEY", "key")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "auth_dir") {
		t.Fatalf("expected auth_dir error, got %v", err)
	}
}

func TestLoadStillRequiresAPIKeyForNonCodexProviders(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "custom": {
      "type": "openai-compatible",
      "base_url": "https://example.test/v1"
    }
  },
  "models": {
    "m": {"provider": "custom", "upstream": "m"}
  }
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("expected api_key_env error, got %v", err)
	}
}

func decodeExtraBody(t *testing.T, value map[string]json.RawMessage) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal extra body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode extra body: %v", err)
	}
	return decoded
}

func TestLoadResolvesPassthroughModels(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "codex": {
      "type": "codex",
      "base_url": "https://chatgpt.com/backend-api/codex",
      "passthrough_models": true,
      "client_version": "0.167.0"
    },
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("OPENROUTER_KEY", "key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Providers["codex"].PassthroughModels {
		t.Fatal("codex passthrough not enabled")
	}
	if cfg.Providers["openrouter"].PassthroughModels {
		t.Fatal("openrouter passthrough should default to false")
	}
	if got := cfg.Providers["codex"].ClientVersion; got != "0.167.0" {
		t.Fatalf("client version = %q", got)
	}
	if names := cfg.PassthroughProviderNames(); len(names) != 1 || names[0] != "codex" {
		t.Fatalf("passthrough providers = %v, want [codex]", names)
	}
}

func TestLoadRejectsClientVersionForNonCodexProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "custom": {
      "type": "openai-compatible",
      "base_url": "https://example.test/v1",
      "api_key_env": "CUSTOM_KEY",
      "client_version": "1.2.3"
    }
  },
  "models": {
    "m": {"provider": "custom", "upstream": "m"}
  }
}`)
	t.Setenv("CUSTOM_KEY", "key")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "client_version") {
		t.Fatalf("expected client_version error, got %v", err)
	}
}

func TestLoadCodexClientVersionDefaultsToCLIVersionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "version.json"), []byte(`{"latest_version":"0.171.0","dismissed_version":null}`), 0o600); err != nil {
		t.Fatalf("write version.json: %v", err)
	}

	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "codex": {
      "type": "codex",
      "base_url": "https://chatgpt.com/backend-api/codex",
      "passthrough_models": true
    }
  },
  "models": {
    "gpt": {"provider": "codex", "upstream": "gpt-5"}
  }
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["codex"].ClientVersion; got != "0.171.0" {
		t.Fatalf("client version = %q, want 0.171.0", got)
	}
}

func TestLoadPassthroughProvidersAreSorted(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "auth": {"enabled": false},
  "providers": {
    "zeta": {
      "type": "openai-compatible",
      "base_url": "https://zeta.test/v1",
      "api_key_env": "ZETA_KEY",
      "passthrough_models": true
    },
    "alpha": {
      "type": "openai-compatible",
      "base_url": "https://alpha.test/v1",
      "api_key_env": "ALPHA_KEY",
      "passthrough_models": true
    }
  },
  "models": {
    "m": {"provider": "zeta", "upstream": "m"}
  }
}`)
	t.Setenv("ZETA_KEY", "zeta")
	t.Setenv("ALPHA_KEY", "alpha")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names := cfg.PassthroughProviderNames()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Fatalf("passthrough providers = %v, want [alpha zeta]", names)
	}
}

func TestLoadRejectsRetiredCompatibilityBlock(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "compatibility": {"openrouter": false},
  "virtual_keys": [
    {"name": "laptop", "key_env": "PROXY_KEY"}
  ],
  "providers": {
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_KEY"
    }
  },
  "models": {
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "key")
	t.Setenv("OPENROUTER_KEY", "upstream")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "compatibility") {
		t.Fatalf("expected unknown-field error for compatibility, got %v", err)
	}
}
