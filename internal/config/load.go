package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/tailscale/hujson"
)

type rawConfig struct {
	EnvFile     string                 `json:"env_file"`
	Server      rawServer              `json:"server"`
	Logging     rawLogging             `json:"logging"`
	Auth        *rawAuth               `json:"auth"`
	VirtualKeys []rawVirtualKey        `json:"virtual_keys"`
	Policies    []rawPolicy            `json:"policies"`
	Providers   map[string]rawProvider `json:"providers"`
	Models      map[string]rawModel    `json:"models"`
}

type rawServer struct {
	Listen            *string `json:"listen"`
	MaxBodyBytes      *int64  `json:"max_body_bytes"`
	ReadHeaderTimeout *string `json:"read_header_timeout"`
	ShutdownTimeout   *string `json:"shutdown_timeout"`
}

type rawLogging struct {
	Format   *string `json:"format"`
	Level    *string `json:"level"`
	LogUsage *bool   `json:"log_usage"`
}

type rawAuth struct {
	Enabled *bool `json:"enabled"`
}

type rawVirtualKey struct {
	Name      string `json:"name"`
	KeyEnv    string `json:"key_env"`
	Policy    string `json:"policy"`
	UserAgent string `json:"user_agent"`
}

type rawPolicy struct {
	Name   string   `json:"name"`
	Mode   string   `json:"mode"`
	Models []string `json:"models"`
}

type rawProvider struct {
	Type                  string         `json:"type"`
	BaseURL               string         `json:"base_url"`
	APIKeyEnv             string         `json:"api_key_env"`
	DialTimeout           *string        `json:"dial_timeout"`
	ResponseHeaderTimeout *string        `json:"response_header_timeout"`
	Referer               string         `json:"referer"`
	Title                 string         `json:"title"`
	ExtraBody             map[string]any `json:"extra_body"`
}

type rawModel struct {
	Provider  string         `json:"provider"`
	Upstream  string         `json:"upstream"`
	ExtraBody map[string]any `json:"extra_body"`
}

// Load reads, resolves, and validates a JWCC (JSON with comments and trailing
// commas) configuration file. An optional env_file, relative to the config
// file, is loaded before key_env references are resolved. Real process
// environment variables take precedence over values from the env file.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", abs, err)
	}

	standard, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", abs, err)
	}

	var raw rawConfig
	dec := json.NewDecoder(bytes.NewReader(standard))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", abs, err)
	}

	envPath := ""
	var dotenv map[string]string
	if raw.EnvFile != "" {
		envPath = raw.EnvFile
		if !filepath.IsAbs(envPath) {
			envPath = filepath.Join(filepath.Dir(abs), envPath)
		}
		dotenv, err = godotenv.Read(envPath)
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", envPath, err)
		}
	}

	cfg, err := resolve(envPath, raw, dotenv)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", abs, err)
	}
	cfg.SourcePath = abs
	return cfg, nil
}

func lookupEnv(dotenv map[string]string, name string) (string, bool) {
	if v, ok := os.LookupEnv(name); ok {
		return v, true
	}
	v, ok := dotenv[name]
	return v, ok
}
