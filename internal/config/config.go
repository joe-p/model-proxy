package config

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration. Secrets are already
// loaded into memory and must never be logged.
type Config struct {
	SourcePath string
	EnvFile    string
	// KeyFile is the resolved path of the optional separate virtual-key file.
	KeyFile string

	Server    ServerConfig
	Logging   LoggingConfig
	Auth      AuthConfig
	Pricing   PricingConfig
	Keys      []VirtualKey
	Policies  map[string]PolicyConfig
	Providers map[string]ProviderConfig
	Models    map[string]ModelConfig

	// EnableClaudeSubscription exposes /claude_sub/, a transparent proxy for
	// Claude Code subscription traffic.
	EnableClaudeSubscription bool
}

type ServerConfig struct {
	Listen            string
	MaxBodyBytes      int64
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration

	// AdminListen enables the administrative API on a separate listener. An
	// empty value disables it and with it the reload endpoint.
	AdminListen string
	// AdminToken is the resolved admin credential. It is never logged and never
	// accepted from a reloaded configuration: a reload cannot change who is
	// allowed to reload.
	AdminToken string
}

// AdminEnabled reports whether the administrative API is configured.
func (s ServerConfig) AdminEnabled() bool {
	return s.AdminListen != "" && s.AdminToken != ""
}

type LoggingConfig struct {
	Format     string
	Level      slog.Level
	LogUsage   bool
	AccessSink AccessSinkConfig
}

// AccessSinkConfig configures optional push of access-log records to an HTTP
// collector. An empty URL disables the sink.
type AccessSinkConfig struct {
	URL           string
	BatchSize     int
	FlushInterval time.Duration
	Timeout       time.Duration
	QueueSize     int
}

// Enabled reports whether an HTTP access-log sink is configured.
func (s AccessSinkConfig) Enabled() bool {
	return s.URL != ""
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

// SplitProviderModel splits a model id of the form "<provider>/<upstream>"
// when the provider name matches a configured provider. The longest matching
// provider name wins, so upstream ids may contain slashes of their own. The
// returned upstream is never empty.
func (c *Config) SplitProviderModel(id string) (providerName, upstream string, ok bool) {
	for name := range c.Providers {
		if len(id) <= len(name)+1 || !strings.HasPrefix(id, name+"/") {
			continue
		}
		if len(name) <= len(providerName) {
			continue
		}
		providerName, upstream, ok = name, id[len(name)+1:], true
	}
	return providerName, upstream, ok
}

type ModelConfig struct {
	Alias     string
	Provider  string
	Upstream  string
	ExtraBody map[string]json.RawMessage
	Pricing   *ModelPricing

	// Passthrough marks a model that was forwarded without a configured alias.
	// Alias holds the requested id; Upstream is that id without a
	// "<provider>/" prefix when the client used one.
	Passthrough bool
}

// ModelPricing is an explicit list-price override in USD per million tokens.
// CachedInputPerMillion falls back to InputPerMillion when nil.
type ModelPricing struct {
	InputPerMillion       float64
	CachedInputPerMillion *float64
	OutputPerMillion      float64
}
