package server

import (
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/openroutercompat"
	"github.com/joe-p/model-proxy/internal/pricing"
)

// modelCatalog is the pricing service capability needed to describe models to
// clients. It is optional so tests can run without a catalog.
type modelCatalog interface {
	LookupModel(providerID, modelID string) (pricing.ModelInfo, bool)
}

// chatCost returns the OpenRouter cost injector for a chat request, or nil when
// costs must not be injected.
//
// Costs are injected only for API-billed providers: a subscription provider
// (billing_mode "subscription") has no per-token price to report. A
// provider-reported cost always wins because the injector never runs for it.
func chatCost(st *state, cfg *config.Config, model config.ModelConfig) openroutercompat.CostFunc {
	providerCfg := cfg.Providers[model.Provider]
	if providerCfg.BillingMode == "subscription" {
		return nil
	}

	if model.Pricing != nil {
		rates := ratesFromModelPricing(model.Pricing)
		return func(tokens openroutercompat.Tokens) *openroutercompat.Cost {
			return injectableCost(tokens, rates)
		}
	}

	catalog, ok := st.pricing.(modelCatalog)
	if !ok || providerCfg.PricingProvider == "" {
		return nil
	}
	info, ok := catalog.LookupModel(providerCfg.PricingProvider, model.Upstream)
	if !ok {
		return nil
	}
	return func(tokens openroutercompat.Tokens) *openroutercompat.Cost {
		return injectableCost(tokens, info.RatesFor(tokens.PromptTokens))
	}
}

// injectableCost splits a derived cost into OpenRouter's prompt/completion
// breakdown. It returns nil when there is nothing to charge for.
func injectableCost(tokens openroutercompat.Tokens, rates pricing.Rates) *openroutercompat.Cost {
	if tokens.PromptTokens == 0 && tokens.CompletionTokens == 0 {
		return nil
	}
	promptUsage := pricing.Usage{
		PromptTokens:     tokens.PromptTokens,
		CachedTokens:     tokens.CachedTokens,
		CacheWriteTokens: tokens.CacheWriteTokens,
	}
	completionUsage := pricing.Usage{CompletionTokens: tokens.CompletionTokens}

	promptCost := pricing.EstimateUsage(promptUsage, rates)
	completionCost := pricing.EstimateUsage(completionUsage, rates)
	return &openroutercompat.Cost{
		Total:      promptCost + completionCost,
		Prompt:     promptCost,
		Completion: completionCost,
	}
}

func ratesFromModelPricing(modelPricing *config.ModelPricing) pricing.Rates {
	rates := pricing.Rates{
		Input:       modelPricing.InputPerMillion,
		CachedInput: modelPricing.InputPerMillion,
		Output:      modelPricing.OutputPerMillion,
		CacheWrite:  modelPricing.InputPerMillion,
	}
	if modelPricing.CachedInputPerMillion != nil {
		rates.CachedInput = *modelPricing.CachedInputPerMillion
	}
	return rates
}
