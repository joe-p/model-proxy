package server

import (
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/pricing"
)

// CostEstimator resolves list-price rates for an upstream model.
type CostEstimator interface {
	Estimate(providerID, modelID string, usage pricing.Usage) (float64, bool)
}

// applyEstimatedCost attaches an API-equivalent cost estimate when the
// upstream did not report an actual cost. An explicit model pricing block wins
// over the catalog.
func applyEstimatedCost(lr *requestLog, st *state, model config.ModelConfig) {
	if lr.usage == nil || lr.usage.Cost != nil {
		return
	}

	usage := pricing.Usage{
		PromptTokens:     lr.usage.PromptTokens,
		CompletionTokens: lr.usage.CompletionTokens,
	}
	if details := lr.usage.PromptTokensDetails; details != nil {
		usage.CachedTokens = details.CachedTokens
		usage.CacheWriteTokens = details.CacheWriteTokens
	}

	if model.Pricing != nil {
		rates := pricing.Rates{
			Input:      model.Pricing.InputPerMillion,
			Output:     model.Pricing.OutputPerMillion,
			CacheWrite: model.Pricing.InputPerMillion,
		}
		rates.CachedInput = model.Pricing.InputPerMillion
		if model.Pricing.CachedInputPerMillion != nil {
			rates.CachedInput = *model.Pricing.CachedInputPerMillion
		}
		cost := pricing.EstimateUsage(usage, rates)
		lr.estimatedCost = &cost
		lr.pricingSource = "config"
		return
	}

	if st.pricing == nil {
		return
	}
	providerCfg, ok := st.cfg.Providers[model.Provider]
	if !ok || providerCfg.PricingProvider == "" {
		return
	}
	cost, ok := st.pricing.Estimate(providerCfg.PricingProvider, model.Upstream, usage)
	if !ok {
		return
	}
	lr.estimatedCost = &cost
	lr.pricingSource = "models.dev"
}
