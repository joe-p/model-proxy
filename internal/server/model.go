package server

import (
	"fmt"
	"strings"

	"github.com/joe-p/model-proxy/internal/config"
)

// resolveModel maps the model name from a request onto a configured alias or
// onto a provider that allows passthrough models. A "<provider>/<upstream>" id
// selects the provider explicitly and the prefix is stripped before the
// upstream call. An unlisted id without such a prefix goes to the single
// provider that allows passthrough models, with the id used unchanged as the
// upstream model id.
//
// Policies apply to passthrough ids the same way they apply to aliases: a
// whitelist key may only use the names it lists, and a blacklist key may use
// anything it does not list. Both the requested id and its bare upstream form
// are checked, so a blacklist entry cannot be bypassed with the other form.
func resolveModel(cfg *config.Config, key *config.VirtualKey, name string) (config.ModelConfig, *apiError) {
	if model, ok := cfg.Models[name]; ok {
		if !cfg.ModelAllowed(key, name) {
			return config.ModelConfig{}, errModelNotAllowed()
		}
		return model, nil
	}

	if providerName, upstream, ok := cfg.SplitProviderModel(name); ok {
		if !cfg.Providers[providerName].PassthroughModels {
			return config.ModelConfig{}, errInvalidRequest(fmt.Sprintf(
				"Model %q names provider %q, which does not allow passthrough models; add it to models",
				name, providerName,
			))
		}
		if !cfg.ModelAllowed(key, name) || !cfg.ModelAllowed(key, upstream) {
			return config.ModelConfig{}, errModelNotAllowed()
		}
		return passthroughModel(cfg, providerName, name, upstream), nil
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
	providerName := candidates[0]
	if !cfg.ModelAllowed(key, name) || !cfg.ModelAllowed(key, providerName+"/"+name) {
		return config.ModelConfig{}, errModelNotAllowed()
	}
	return passthroughModel(cfg, providerName, name, name), nil
}

// passthroughModel builds the route for a passed-through id. alias is the id
// the client asked for and upstream is what the provider receives.
func passthroughModel(cfg *config.Config, providerName, alias, upstream string) config.ModelConfig {
	return config.ModelConfig{
		Alias:       alias,
		Provider:    providerName,
		Upstream:    upstream,
		ExtraBody:   cfg.Providers[providerName].ExtraBody,
		Passthrough: true,
	}
}
