package config

import (
	"encoding/json"
	"log/slog"
	"sort"
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
	Pricing   PricingConfig
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

// PricingConfig configures optional list-price estimates from a model catalog.
// An empty Source disables pricing.
type PricingConfig struct {
	Source          string
	URL             string
	Cache           string
	RefreshInterval time.Duration
	Timeout         time.Duration
}

// Enabled reports whether a pricing catalog is configured.
func (p PricingConfig) Enabled() bool {
	return p.Source != ""
}

type VirtualKey struct {
	Name      string
	Key       string
	Policy    string
	UserAgent string
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
	Type                  string
	BaseURL               string
	APIKey                string
	AuthDir               string
	BillingMode           string
	PricingProvider       string
	PassthroughModels     bool
	ClientVersion         string
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	Referer               string
	Title                 string
	ExtraBody             map[string]json.RawMessage
}

// PassthroughProviderNames returns the sorted names of providers configured to
// accept model ids that the alias registry does not list.
func (c *Config) PassthroughProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for name, providerCfg := range c.Providers {
		if providerCfg.PassthroughModels {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

type ModelConfig struct {
	Alias     string
	Provider  string
	Upstream  string
	ExtraBody map[string]json.RawMessage
	Pricing   *ModelPricing

	// Passthrough marks a model that was forwarded without a configured alias.
	// Alias then holds the requested id, which is also the upstream id.
	Passthrough bool
}

// ModelPricing is an explicit list-price override in USD per million tokens.
// CachedInputPerMillion falls back to InputPerMillion when nil.
type ModelPricing struct {
	InputPerMillion       float64
	CachedInputPerMillion *float64
	OutputPerMillion      float64
}
