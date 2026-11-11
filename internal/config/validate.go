package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func resolve(envPath, configDir string, raw rawConfig, dotenv map[string]string) (*Config, error) {
	cfg := &Config{
		EnvFile:                  envPath,
		Policies:                 make(map[string]PolicyConfig, len(raw.Policies)),
		Providers:                make(map[string]ProviderConfig, len(raw.Providers)),
		Models:                   make(map[string]ModelConfig, len(raw.Models)),
		EnableClaudeSubscription: raw.EnableClaudeSubscription,
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
	if err := resolveAdmin(&cfg.Server, raw.Server, dotenv); err != nil {
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
	cfg.Logging.AccessSink, err = resolveAccessSink(raw.Logging.AccessSink)
	if err != nil {
		return nil, err
	}

	// Auth defaults to enabled.
	cfg.Auth.Enabled = true
	if raw.Auth != nil && raw.Auth.Enabled != nil {
		cfg.Auth.Enabled = *raw.Auth.Enabled
	}

	cfg.Pricing, err = resolvePricing(raw.Pricing, configDir)
	if err != nil {
		return nil, err
	}

	cfg.Keys, cfg.KeyFile, err = resolveVirtualKeys(raw.KeyFile, configDir, dotenv)
	if err != nil {
		return nil, err
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

		providerType := strings.TrimSpace(rawProvider.Type)
		if providerType == "" {
			// Backwards compatibility: a provider without an explicit type is
			// identified by its configured name.
			providerType = name
		}
		codexProvider := isCodexProvider(providerType)

		if rawProvider.AuthDir != "" && !codexProvider {
			return nil, fmt.Errorf("provider %q: auth_dir is only supported for codex providers", name)
		}

		baseURL, err := validateBaseURL(rawProvider.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}

		apiKey := ""
		apiKeyEnv := strings.TrimSpace(rawProvider.APIKeyEnv)
		if apiKeyEnv != "" {
			var ok bool
			apiKey, ok = lookupEnv(dotenv, apiKeyEnv)
			if !ok {
				return nil, fmt.Errorf("provider %q: environment variable %s is not set", name, apiKeyEnv)
			}
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q: environment variable %s is empty", name, apiKeyEnv)
			}
		} else if !codexProvider {
			return nil, fmt.Errorf("provider %q: api_key_env must not be empty", name)
		}

		authDir := ""
		if codexProvider && apiKey == "" {
			authDir, err = resolveAuthDir(rawProvider.AuthDir, configDir)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
		}

		billingMode := strings.TrimSpace(rawProvider.BillingMode)
		if billingMode == "" && codexProvider {
			billingMode = "subscription"
		}
		if err := validateLogValue(billingMode, fmt.Sprintf("provider %q billing_mode", name)); err != nil {
			return nil, err
		}

		pricingProvider := strings.TrimSpace(rawProvider.PricingProvider)
		if pricingProvider == "" {
			switch {
			case codexProvider:
				pricingProvider = "openai"
			case providerType == "openrouter":
				// models.dev indexes OpenRouter's own catalog under "openrouter".
				pricingProvider = "openrouter"
			}
		}
		if err := validateLogValue(pricingProvider, fmt.Sprintf("provider %q pricing_provider", name)); err != nil {
			return nil, err
		}

		imagesPath := strings.TrimSpace(rawProvider.ImagesPath)
		if imagesPath != "" {
			if codexProvider {
				return nil, fmt.Errorf("provider %q: images_path is not supported for codex providers", name)
			}
			if !strings.HasPrefix(imagesPath, "/") {
				return nil, fmt.Errorf("provider %q: images_path must start with \"/\"", name)
			}
			if strings.ContainsAny(imagesPath, " \t\r\n") {
				return nil, fmt.Errorf("provider %q: images_path must not contain whitespace", name)
			}
			if err := validateLogValue(imagesPath, fmt.Sprintf("provider %q images_path", name)); err != nil {
				return nil, err
			}
		}

		clientVersion := strings.TrimSpace(rawProvider.ClientVersion)
		if clientVersion != "" && !codexProvider {
			return nil, fmt.Errorf("provider %q: client_version is only supported for codex providers", name)
		}
		if err := validateLogValue(clientVersion, fmt.Sprintf("provider %q client_version", name)); err != nil {
			return nil, err
		}
		if clientVersion == "" && codexProvider && authDir != "" {
			// The Codex model listing requires a client version. Fall back to the one
			// recorded by the Codex CLI so passthrough listing works without extra
			// configuration.
			clientVersion = codexClientVersion(authDir)
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
			Type:                  providerType,
			BaseURL:               baseURL,
			ImagesPath:            imagesPath,
			APIKey:                apiKey,
			AuthDir:               authDir,
			BillingMode:           billingMode,
			PricingProvider:       pricingProvider,
			PassthroughModels:     rawProvider.PassthroughModels,
			ClientVersion:         clientVersion,
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
		modelPricing, err := normalizeModelPricing(rawModel.Pricing, alias)
		if err != nil {
			return nil, err
		}
		cfg.Models[alias] = ModelConfig{
			Alias:     alias,
			Provider:  providerName,
			Upstream:  upstream,
			ExtraBody: effectiveExtra,
			Pricing:   modelPricing,
		}
	}
	if len(cfg.Models) == 0 && !cfg.EnableClaudeSubscription {
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

// validateLogValue rejects control characters that could corrupt logs. Empty
// values are allowed and mean "use the client-supplied value".
func validateLogValue(value, field string) error {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == '\t' || (b >= 0x20 && b != 0x7f) {
			continue
		}
		return fmt.Errorf("%s contains an invalid control character", field)
	}
	return nil
}

// resolveAdmin resolves the optional administrative API settings. The admin
// credential is resolved here, once, so that a reload can only ever be
// authorized by the token the process started with: a configuration file must
// never be able to grant itself permission to reload.
func resolveAdmin(cfg *ServerConfig, raw rawServer, dotenv map[string]string) error {
	cfg.AdminListen = ""
	if raw.AdminListen != nil {
		cfg.AdminListen = strings.TrimSpace(*raw.AdminListen)
	}
	tokenEnv := ""
	if raw.AdminTokenEnv != nil {
		tokenEnv = strings.TrimSpace(*raw.AdminTokenEnv)
	}

	if cfg.AdminListen == "" {
		// Without a listener there is nowhere to serve the admin API. A token
		// configured on its own is harmless; the caller logs that reload is
		// unavailable.
		return nil
	}
	if cfg.AdminListen == cfg.Listen {
		return fmt.Errorf("server.admin_listen must differ from server.listen")
	}
	if tokenEnv == "" {
		return fmt.Errorf("server.admin_listen requires server.admin_token_env")
	}
	token, ok := lookupEnv(dotenv, tokenEnv)
	if !ok {
		return fmt.Errorf("server.admin_token_env: environment variable %s is not set", tokenEnv)
	}
	if token == "" {
		return fmt.Errorf("server.admin_token_env: environment variable %s is empty", tokenEnv)
	}
	cfg.AdminToken = token
	return nil
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

const (
	defaultSinkBatchSize     = 250
	defaultSinkFlushInterval = time.Second
	defaultSinkTimeout       = 5 * time.Second
	defaultSinkQueueSize     = 4096
)

func resolveAccessSink(raw *rawAccessSink) (AccessSinkConfig, error) {
	if raw == nil {
		return AccessSinkConfig{}, nil
	}
	var cfg AccessSinkConfig
	if raw.URL == nil || strings.TrimSpace(*raw.URL) == "" {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.url must not be empty")
	}
	endpoint := strings.TrimSpace(*raw.URL)
	u, err := url.Parse(endpoint)
	if err != nil {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.url must use http or https")
	}
	if u.Host == "" {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.url must include a host")
	}
	if u.Fragment != "" {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.url must not include a fragment")
	}
	cfg.URL = endpoint

	cfg.BatchSize = defaultSinkBatchSize
	if raw.BatchSize != nil {
		cfg.BatchSize = *raw.BatchSize
	}
	if cfg.BatchSize <= 0 {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.batch_size must be positive")
	}
	cfg.QueueSize = defaultSinkQueueSize
	if raw.QueueSize != nil {
		cfg.QueueSize = *raw.QueueSize
	}
	if cfg.QueueSize <= 0 {
		return AccessSinkConfig{}, fmt.Errorf("logging.access_sink.queue_size must be positive")
	}
	cfg.FlushInterval, err = parseDuration(raw.FlushInterval, defaultSinkFlushInterval, "logging.access_sink.flush_interval")
	if err != nil {
		return AccessSinkConfig{}, err
	}
	cfg.Timeout, err = parseDuration(raw.Timeout, defaultSinkTimeout, "logging.access_sink.timeout")
	if err != nil {
		return AccessSinkConfig{}, err
	}
	return cfg, nil
}

func isCodexProvider(providerType string) bool {
	return providerType == "codex" || providerType == "chatgpt"
}

const (
	defaultPricingURL     = "https://models.dev/api.json"
	defaultPricingRefresh = 24 * time.Hour
	defaultPricingTimeout = 30 * time.Second
)

func resolvePricing(raw *rawPricing, configDir string) (PricingConfig, error) {
	if raw == nil {
		return PricingConfig{}, nil
	}

	source := "models.dev"
	if raw.Source != nil {
		source = strings.ToLower(strings.TrimSpace(*raw.Source))
		if source == "" {
			return PricingConfig{}, fmt.Errorf("pricing.source must not be empty")
		}
	}
	if source != "models.dev" {
		return PricingConfig{}, fmt.Errorf("pricing.source must be \"models.dev\"")
	}

	rawURL := defaultPricingURL
	if raw.URL != nil && strings.TrimSpace(*raw.URL) != "" {
		rawURL = strings.TrimSpace(*raw.URL)
	}
	catalogURL, err := validateBaseURL(rawURL)
	if err != nil {
		return PricingConfig{}, fmt.Errorf("pricing.url: %w", err)
	}

	cache := ""
	if raw.Cache != nil {
		cache = strings.TrimSpace(*raw.Cache)
	}
	if cache == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(dir, "model-proxy", "models.dev.json")
		}
	} else {
		cache = expandHomePath(cache)
		if !filepath.IsAbs(cache) {
			cache = filepath.Join(configDir, cache)
		}
		cache = filepath.Clean(cache)
	}

	refreshInterval, err := parseDuration(raw.RefreshInterval, defaultPricingRefresh, "pricing.refresh_interval")
	if err != nil {
		return PricingConfig{}, err
	}
	timeout, err := parseDuration(raw.Timeout, defaultPricingTimeout, "pricing.timeout")
	if err != nil {
		return PricingConfig{}, err
	}

	return PricingConfig{
		Source:          source,
		URL:             catalogURL,
		Cache:           cache,
		RefreshInterval: refreshInterval,
		Timeout:         timeout,
	}, nil
}

func normalizeModelPricing(raw *rawModelPricing, alias string) (*ModelPricing, error) {
	if raw == nil {
		return nil, nil
	}
	if raw.InputPerMillion == nil || raw.OutputPerMillion == nil {
		return nil, fmt.Errorf("model %q pricing: input_per_million and output_per_million are required", alias)
	}
	if *raw.InputPerMillion < 0 || *raw.OutputPerMillion < 0 {
		return nil, fmt.Errorf("model %q pricing: rates must not be negative", alias)
	}
	if raw.CachedInputPerMillion != nil && *raw.CachedInputPerMillion < 0 {
		return nil, fmt.Errorf("model %q pricing: cached_input_per_million must not be negative", alias)
	}
	return &ModelPricing{
		InputPerMillion:       *raw.InputPerMillion,
		CachedInputPerMillion: raw.CachedInputPerMillion,
		OutputPerMillion:      *raw.OutputPerMillion,
	}, nil
}

func expandHomePath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// codexClientVersion reads the version recorded by the Codex CLI in
// <auth_dir>/version.json. It returns an empty string when the file is missing or
// unreadable, which disables model listing but not passthrough routing.
func codexClientVersion(authDir string) string {
	data, err := os.ReadFile(filepath.Join(authDir, "version.json"))
	if err != nil {
		return ""
	}
	var version struct {
		LatestVersion string `json:"latest_version"`
		Version       string `json:"version"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return ""
	}
	if value := strings.TrimSpace(version.Version); value != "" {
		return value
	}
	return strings.TrimSpace(version.LatestVersion)
}

// resolveAuthDir resolves a Codex credential directory. An empty value uses
// $CODEX_HOME, then ~/.codex. Relative paths are resolved from the config file.
func resolveAuthDir(value, configDir string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("CODEX_HOME"))
	}
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve default Codex auth_dir: %w", err)
		}
		value = filepath.Join(home, ".codex")
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve Codex auth_dir: %w", err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, value[2:])
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(configDir, value)
	}
	return filepath.Clean(value), nil
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
