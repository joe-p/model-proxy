package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLoadsKeyFileWithDirectKeys(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keys.json", `[
  // The key file is JWCC too.
  {"name": "direct", "key": "  direct-secret  ", "policy": "basic"},
  {"name": "from-env", "key_env": "PROXY_KEY"},
]`)
	path := writeFile(t, dir, "config.json", `{
  "key_file": "keys.json",
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
    "fast": {"provider": "openrouter", "upstream": "openai/gpt-4o-mini"}
  }
}`)
	t.Setenv("PROXY_KEY", "env-secret")
	t.Setenv("OPENROUTER_KEY", "upstream")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "keys.json"); cfg.KeyFile != want {
		t.Fatalf("key file = %q, want %q", cfg.KeyFile, want)
	}
	if len(cfg.Keys) != 2 {
		t.Fatalf("keys = %+v", cfg.Keys)
	}
	if got := cfg.Keys[0].Key; got != "direct-secret" {
		t.Fatalf("direct key = %q, want trimmed value", got)
	}
	if got := cfg.Keys[0].Policy; got != "basic" {
		t.Fatalf("direct key policy = %q", got)
	}
	if got := cfg.Keys[1].Key; got != "env-secret" {
		t.Fatalf("env key = %q", got)
	}
}

func TestLoadLoadsAbsoluteKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeFile(t, dir, "keys.json", `[{"name": "laptop", "key": "secret"}]`)
	path := writeFile(t, dir, "config.json", `{
  "key_file": "`+keyFile+`",
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
	if len(cfg.Keys) != 1 || cfg.Keys[0].Name != "laptop" || cfg.Keys[0].Key != "secret" {
		t.Fatalf("unexpected keys: %+v", cfg.Keys)
	}
}

func TestLoadRejectsInlineVirtualKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "virtual_keys": [
    {"name": "inline", "key_env": "PROXY_KEY"}
  ]
}`)
	t.Setenv("PROXY_KEY", "inline-secret")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error for virtual_keys, got %v", err)
	}
}

func TestLoadRejectsKeyAndKeyEnvTogether(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keys.json", `[
  {"name": "both", "key": "direct-secret", "key_env": "PROXY_KEY"}
]`)
	path := writeFile(t, dir, "config.json", `{
  "key_file": "keys.json"
}`)
	t.Setenv("PROXY_KEY", "env-secret")

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

func TestLoadRejectsUnknownFieldInKeyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keys.json", `[
  {"name": "laptop", "key": "secret", "unknown_setting": true}
]`)
	path := writeFile(t, dir, "config.json", `{
  "key_file": "keys.json"
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadRejectsObjectKeyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keys.json", `{
  "virtual_keys": [{"name": "laptop", "key": "secret"}]
}`)
	path := writeFile(t, dir, "config.json", `{
  "key_file": "keys.json"
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "parse key file") {
		t.Fatalf("expected parse key file error, got %v", err)
	}
}

func TestLoadRejectsMissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
  "key_file": "missing.json"
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "key file") {
		t.Fatalf("expected key file error, got %v", err)
	}
}
