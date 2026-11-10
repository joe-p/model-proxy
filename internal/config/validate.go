package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"
)

func resolve(envPath string, raw rawConfig, dotenv map[string]string) (*Config, error) {
	cfg := &Config{
		EnvFile:   envPath,
		Policies:  make(map[string]PolicyConfig, len(raw.Policies)),
		Providers: make(map[string]ProviderConfig, len(raw.Providers)),
		Models:    make(map[string]ModelConfig, len(raw.Models)),
	}

	// Server defaults.
	cfg.Server = ServerConfig{
		Listen:            "127.0.0.1:8080",
		MaxBodyBytes:      1 << 20,
		ReadHeaderTimeout: 10 * time.Second,
		ShutdownTimeout:   15 * time.Second,
	}
	if raw.Server.Listen != nil {
		cfg.Server.Listen = strings.TrimSpace(*raw.Server.Listen)
	}
	if cfg.Server.Listen == "" {
		return nil, fmt.Errorf("server.listen must not be empty")
	}
	if raw.Server.MaxBodyBytes != nil {
		cfg.Server.MaxBodyBytes = *raw.Server.MaxBodyBytes
	}
	if cfg.Server.MaxBodyBytes <= 0 {
		return nil, fmt.Errorf("server.max_body_bytes must be positive")
	}
	var err error
	cfg.Server.ReadHeaderTimeout, err = parseDuration(raw.Server.ReadHeaderTimeout, 10*time.Second, "server.read_header_timeout")
	if err != nil {
		return nil, err
	}
	cfg.Server.ShutdownTimeout, err = parseDuration(raw.Server.ShutdownTimeout, 15*time.Second, "server.shutdown_timeout")
	if err != nil {
		return nil, err
	}

	// Logging defaults.
	cfg.Logging = LoggingConfig{Format: "json", Level: slog.LevelInfo, LogUsage: true}
	if raw.Logging.Format != nil {
		cfg.Logging.Format = strings.ToLower(strings.TrimSpace(*raw.Logging.Format))
	}
	if cfg.Logging.Format != "json" && cfg.Logging.Format != "text" {
		return nil, fmt.Errorf("logging.format must be \"json\" or \"text\"")
	}
	if raw.Logging.Level != nil {
		level := strings.ToLower(strings.TrimSpace(*raw.Logging.Level))
		if err := cfg.Logging.Level.UnmarshalText([]byte(level)); err != nil {
			return nil, fmt.Errorf("logging.level: %w", err)
		}
	}
	if raw.Logging.LogUsage != nil {
		cfg.Logging.LogUsage = *raw.Logging.LogUsage
	}

	// Auth defaults to enabled.
	cfg.Auth.Enabled = true
	if raw.Auth != nil && raw.Auth.Enabled != nil {
		cfg.Auth.Enabled = *raw.Auth.Enabled
	}

	keyNames := make(map[string]struct{}, len(raw.VirtualKeys))
	keyValues := make(map[string]string, len(raw.VirtualKeys))
	for i, rawKey := range raw.VirtualKeys {
		name := strings.TrimSpace(rawKey.Name)
		keyEnv := strings.TrimSpace(rawKey.KeyEnv)
		if name == "" {
			return nil, fmt.Errorf("virtual_keys[%d].name must not be empty", i)
		}
		if keyEnv == "" {
			return nil, fmt.Errorf("virtual_keys[%d] (%s): key_env must not be empty", i, name)
		}
		if _, exists := keyNames[name]; exists {
			return nil, fmt.Errorf("duplicate virtual key name %q", name)
		}
		key, ok := lookupEnv(dotenv, keyEnv)
		if !ok {
			return nil, fmt.Errorf("virtual key %q: environment variable %s is not set", name, keyEnv)
		}
		if key == "" {
			return nil, fmt.Errorf("virtual key %q: environment variable %s is empty", name, keyEnv)
		}
		if other, exists := keyValues[key]; exists {
			return nil, fmt.Errorf("virtual keys %q and %q resolve to the same value", other, name)
		}
		keyNames[name] = struct{}{}
		keyValues[key] = name
		cfg.Keys = append(cfg.Keys, VirtualKey{
			Name:   name,
			Key:    key,
			Policy: strings.TrimSpace(rawKey.Policy),
		})
	}
	if cfg.Auth.Enabled && len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("auth is enabled but no virtual keys are configured")
	}

	providerNames := sortedKeys(raw.Providers)
	for _, name := range providerNames {
		if name == "" || name != strings.TrimSpace(name) {
			return nil, fmt.Errorf("provider names must be non-empty and have no surrounding whitespace")
		}
		rawProvider := raw.Providers[name]

		baseURL, err := validateBaseURL(rawProvider.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		apiKeyEnv := strings.TrimSpace(rawProvider.APIKeyEnv)
		if apiKeyEnv == "" {
			return nil, fmt.Errorf("provider %q: api_key_env must not be empty", name)
		}
		apiKey, ok := lookupEnv(dotenv, apiKeyEnv)
		if !ok {
			return nil, fmt.Errorf("provider %q: environment variable %s is not set", name, apiKeyEnv)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("provider %q: environment variable %s is empty", name, apiKeyEnv)
		}
		dialTimeout, err := parseDuration(rawProvider.DialTimeout, 10*time.Second, fmt.Sprintf("provider %q dial_timeout", name))
		if err != nil {
			return nil, err
		}
		responseTimeout, err := parseDuration(rawProvider.ResponseHeaderTimeout, 120*time.Second, fmt.Sprintf("provider %q response_header_timeout", name))
		if err != nil {
			return nil, err
		}
		extraBody, err := normalizeExtraBody(rawProvider.ExtraBody, fmt.Sprintf("provider %q extra_body", name))
		if err != nil {
			return nil, err
		}
		cfg.Providers[name] = ProviderConfig{
			Name:                  name,
			BaseURL:               baseURL,
			APIKey:                apiKey,
			DialTimeout:           dialTimeout,
			ResponseHeaderTimeout: responseTimeout,
			Referer:               strings.TrimSpace(rawProvider.Referer),
			Title:                 strings.TrimSpace(rawProvider.Title),
			ExtraBody:             extraBody,
		}
	}

	modelAliases := sortedKeys(raw.Models)
	for _, alias := range modelAliases {
		if alias == "" || alias != strings.TrimSpace(alias) {
			return nil, fmt.Errorf("model aliases must be non-empty and have no surrounding whitespace")
		}
		rawModel := raw.Models[alias]
		providerName := strings.TrimSpace(rawModel.Provider)
		upstream := strings.TrimSpace(rawModel.Upstream)
		if providerName == "" {
			return nil, fmt.Errorf("model %q: provider must not be empty", alias)
		}
		if upstream == "" {
			return nil, fmt.Errorf("model %q: upstream must not be empty", alias)
		}
		providerCfg, ok := cfg.Providers[providerName]
		if !ok {
			return nil, fmt.Errorf("model %q references unknown provider %q", alias, providerName)
		}
		modelExtra, err := normalizeExtraBody(rawModel.ExtraBody, fmt.Sprintf("model %q extra_body", alias))
		if err != nil {
			return nil, err
		}
		// Provider-level fields are defaults; model-level fields override them.
		// Nested objects such as OpenRouter's "provider" are merged.
		effectiveExtra, err := MergeExtraBody(providerCfg.ExtraBody, modelExtra)
		if err != nil {
			return nil, fmt.Errorf("model %q extra_body: %w", alias, err)
		}
		cfg.Models[alias] = ModelConfig{
			Alias:     alias,
			Provider:  providerName,
			Upstream:  upstream,
			ExtraBody: effectiveExtra,
		}
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("at least one model must be configured")
	}

	for i, rawPolicy := range raw.Policies {
		name := strings.TrimSpace(rawPolicy.Name)
		if name == "" {
			return nil, fmt.Errorf("policies[%d].name must not be empty", i)
		}
		if _, exists := cfg.Policies[name]; exists {
			return nil, fmt.Errorf("duplicate policy name %q", name)
		}
		mode := strings.ToLower(strings.TrimSpace(rawPolicy.Mode))
		if mode != PolicyWhitelist && mode != PolicyBlacklist {
			return nil, fmt.Errorf("policy %q: mode must be %q or %q", name, PolicyWhitelist, PolicyBlacklist)
		}
		if len(rawPolicy.Models) == 0 {
			return nil, fmt.Errorf("policy %q: models must not be empty", name)
		}
		models := make(map[string]struct{}, len(rawPolicy.Models))
		for j, value := range rawPolicy.Models {
			alias := strings.TrimSpace(value)
			if alias == "" {
				return nil, fmt.Errorf("policy %q: models[%d] must not be empty", name, j)
			}
			if _, ok := cfg.Models[alias]; !ok {
				return nil, fmt.Errorf("policy %q references unknown model %q", name, alias)
			}
			models[alias] = struct{}{}
		}
		cfg.Policies[name] = PolicyConfig{Name: name, Mode: mode, Models: models}
	}

	for _, key := range cfg.Keys {
		if key.Policy == "" {
			continue
		}
		if _, ok := cfg.Policies[key.Policy]; !ok {
			return nil, fmt.Errorf("virtual key %q references unknown policy %q", key.Name, key.Policy)
		}
	}

	return cfg, nil
}

func parseDuration(value *string, fallback time.Duration, field string) (time.Duration, error) {
	if value == nil {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(*value))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return d, nil
}

func validateBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("base_url must not be empty")
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url must use http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("base_url must not include a query or fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
