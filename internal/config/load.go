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
	EnvFile                  string                 `json:"env_file"`
	KeyFile                  string                 `json:"key_file"`
	Server                   rawServer              `json:"server"`
	Logging                  rawLogging             `json:"logging"`
	Auth                     *rawAuth               `json:"auth"`
	Pricing                  *rawPricing            `json:"pricing"`
	Policies                 []rawPolicy            `json:"policies"`
	Providers                map[string]rawProvider `json:"providers"`
	Models                   map[string]rawModel    `json:"models"`
	EnableClaudeSubscription bool                   `json:"enable_claude_subscription"`
}

type rawServer struct {
	Listen            *string `json:"listen"`
	AdminListen       *string `json:"admin_listen"`
	AdminTokenEnv     *string `json:"admin_token_env"`
	MaxBodyBytes      *int64  `json:"max_body_bytes"`
	ReadHeaderTimeout *string `json:"read_header_timeout"`
	ShutdownTimeout   *string `json:"shutdown_timeout"`
}

type rawLogging struct {
	Format     *string        `json:"format"`
	Level      *string        `json:"level"`
	LogUsage   *bool          `json:"log_usage"`
	AccessSink *rawAccessSink `json:"access_sink"`
}

type rawAccessSink struct {
	URL           *string `json:"url"`
	BatchSize     *int    `json:"batch_size"`
	FlushInterval *string `json:"flush_interval"`
	Timeout       *string `json:"timeout"`
	QueueSize     *int    `json:"queue_size"`
}

type rawAuth struct {
	Enabled *bool `json:"enabled"`
}

type rawPricing struct {
	Source          *string `json:"source"`
	URL             *string `json:"url"`
	Cache           *string `json:"cache"`
	RefreshInterval *string `json:"refresh_interval"`
	Timeout         *string `json:"timeout"`
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
	AuthDir               string         `json:"auth_dir"`
	BillingMode           string         `json:"billing_mode"`
	PricingProvider       string         `json:"pricing_provider"`
	PassthroughModels     bool           `json:"passthrough_models"`
	ClientVersion         string         `json:"client_version"`
	DialTimeout           *string        `json:"dial_timeout"`
	ResponseHeaderTimeout *string        `json:"response_header_timeout"`
	Referer               string         `json:"referer"`
	Title                 string         `json:"title"`
	ExtraBody             map[string]any `json:"extra_body"`
}

type rawModel struct {
	Provider  string           `json:"provider"`
	Upstream  string           `json:"upstream"`
	ExtraBody map[string]any   `json:"extra_body"`
	Pricing   *rawModelPricing `json:"pricing"`
}

type rawModelPricing struct {
	InputPerMillion       *float64 `json:"input_per_million"`
	CachedInputPerMillion *float64 `json:"cached_input_per_million"`
	OutputPerMillion      *float64 `json:"output_per_million"`
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

	cfg, err := resolve(envPath, filepath.Dir(abs), raw, dotenv)
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
