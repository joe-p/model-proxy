package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/pelletier/go-toml/v2"
)

type rawConfig struct {
	EnvFile     string                 `toml:"env_file"`
	Server      rawServer              `toml:"server"`
	Logging     rawLogging             `toml:"logging"`
	Auth        *rawAuth               `toml:"auth"`
	VirtualKeys []rawVirtualKey        `toml:"virtual_keys"`
	Policies    []rawPolicy            `toml:"policies"`
	Providers   map[string]rawProvider `toml:"providers"`
	Models      map[string]rawModel    `toml:"models"`
}

type rawServer struct {
	Listen            *string `toml:"listen"`
	MaxBodyBytes      *int64  `toml:"max_body_bytes"`
	ReadHeaderTimeout *string `toml:"read_header_timeout"`
	ShutdownTimeout   *string `toml:"shutdown_timeout"`
}

type rawLogging struct {
	Format   *string `toml:"format"`
	Level    *string `toml:"level"`
	LogUsage *bool   `toml:"log_usage"`
}

type rawAuth struct {
	Enabled *bool `toml:"enabled"`
}

type rawVirtualKey struct {
	Name   string `toml:"name"`
	KeyEnv string `toml:"key_env"`
	Policy string `toml:"policy"`
}

type rawPolicy struct {
	Name   string   `toml:"name"`
	Mode   string   `toml:"mode"`
	Models []string `toml:"models"`
}

type rawProvider struct {
	BaseURL               string         `toml:"base_url"`
	APIKeyEnv             string         `toml:"api_key_env"`
	DialTimeout           *string        `toml:"dial_timeout"`
	ResponseHeaderTimeout *string        `toml:"response_header_timeout"`
	Referer               string         `toml:"referer"`
	Title                 string         `toml:"title"`
	ExtraBody             map[string]any `toml:"extra_body"`
}

type rawModel struct {
	Provider  string         `toml:"provider"`
	Upstream  string         `toml:"upstream"`
	ExtraBody map[string]any `toml:"extra_body"`
}

// Load reads, resolves, and validates a TOML configuration file. An optional
// env_file, relative to the config file, is loaded before key_env references
// are resolved. Real process environment variables take precedence over values
// from the env file.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", abs, err)
	}

	var raw rawConfig
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
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
