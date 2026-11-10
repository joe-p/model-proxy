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
	Policies  map[string]PolicyConfig
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
	Name   string
	Key    string
	Policy string
}

const (
	PolicyWhitelist = "whitelist"
	PolicyBlacklist = "blacklist"
)

// PolicyConfig restricts which model aliases a virtual key may use. Models is
// the set of aliases named by the policy; Mode determines whether that set is
// allowed (whitelist) or denied (blacklist).
type PolicyConfig struct {
	Name   string
	Mode   string
	Models map[string]struct{}
}

// ModelAllowed reports whether a virtual key may use the given model alias. A
// nil key or a key without a policy allows every configured alias.
func (c *Config) ModelAllowed(key *VirtualKey, alias string) bool {
	if key == nil || key.Policy == "" {
		return true
	}
	policy, ok := c.Policies[key.Policy]
	if !ok {
		return false
	}
	_, listed := policy.Models[alias]
	if policy.Mode == PolicyWhitelist {
		return listed
	}
	return !listed
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
