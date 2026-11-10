package config

import (
	"log/slog"
	"time"
)

// Config is the fully resolved runtime configuration. Secrets are already
// loaded into memory and must never be logged.
type Config struct {
	SourcePath string
	EnvFile    string

	Server    ServerConfig
	Logging   LoggingConfig
	Auth      AuthConfig
	Keys      []VirtualKey
	Providers map[string]ProviderConfig
	Models    map[string]ModelConfig
}

type ServerConfig struct {
	Listen            string
	MaxBodyBytes      int64
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
}

type LoggingConfig struct {
	Format   string
	Level    slog.Level
	LogUsage bool
}

type AuthConfig struct {
	Enabled bool
}

type VirtualKey struct {
	Name string
	Key  string
}

type ProviderConfig struct {
	Name                  string
	BaseURL               string
	APIKey                string
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	Referer               string
	Title                 string
}

type ModelConfig struct {
	Alias    string
	Provider string
	Upstream string
}
