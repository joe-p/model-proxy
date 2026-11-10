package server

import (
	"fmt"
	"strings"

	"github.com/joe-p/model-proxy/internal/config"
)

// resolveModel maps the model name from a request onto a configured alias or,
// when exactly one provider allows passthrough models, onto that provider with
// the requested name used unchanged as the upstream model id.
//
// Policies apply to passthrough ids the same way they apply to aliases: a
// whitelist key may only use the names it lists, and a blacklist key may use
// anything it does not list.
func resolveModel(cfg *config.Config, key *config.VirtualKey, name string) (config.ModelConfig, *apiError) {
	if model, ok := cfg.Models[name]; ok {
		if !cfg.ModelAllowed(key, name) {
			return config.ModelConfig{}, errModelNotAllowed()
		}
		return model, nil
	}

	candidates := cfg.PassthroughProviderNames()
	if len(candidates) == 0 {
		return config.ModelConfig{}, errModelNotFound()
	}
	if len(candidates) > 1 {
		return config.ModelConfig{}, errInvalidRequest(fmt.Sprintf(
			"Model %q is not configured and more than one provider allows passthrough models (%s); add it to models to choose a provider",
			name, strings.Join(candidates, ", "),
		))
	}
	if !cfg.ModelAllowed(key, name) {
		return config.ModelConfig{}, errModelNotAllowed()
	}

	providerName := candidates[0]
	return config.ModelConfig{
		Alias:       name,
		Provider:    providerName,
		Upstream:    name,
		ExtraBody:   cfg.Providers[providerName].ExtraBody,
		Passthrough: true,
	}, nil
}
