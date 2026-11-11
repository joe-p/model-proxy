package server

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/openroutercompat"
	"github.com/joe-p/model-proxy/internal/pricing"
)

// openRouterClientFields are OpenRouter request fields that would bypass the
// alias registry or add paid features. They are dropped and logged.
var openRouterClientFields = []string{"models", "route", "plugins", "transforms"}

// maxUpstreamErrorBytes bounds how much of an upstream error body is buffered
// before it is rewritten into the OpenRouter error envelope.
const maxUpstreamErrorBytes = 1 << 20

// tolerateOpenRouterFields drops OpenRouter-only request fields that would
// bypass the alias registry or add paid features, recording them for the access
// log.
func tolerateOpenRouterFields(lr *requestLog, fields map[string]json.RawMessage) {
	var dropped []string
	for _, field := range openRouterClientFields {
		if _, exists := fields[field]; !exists {
			continue
		}
		delete(fields, field)
		dropped = append(dropped, field)
	}
	if len(dropped) > 0 {
		lr.droppedFields = dropped
	}
}

// writeUpstreamError rewrites an upstream error response into OpenRouter's error
// envelope. The upstream status is preserved.
func writeUpstreamError(w http.ResponseWriter, resp *http.Response, providerName string) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes))
	if err != nil {
		return err
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(openroutercompat.NormalizeError(body, providerName, resp.StatusCode))
	return err
}

// modelsPayload renders the model list in OpenRouter's shape.
func (s *Server) modelsPayload(st *state, entries []modelEntry) any {
	return openRouterModelList(st, entries, s.startedAt.Unix())
}

// modelEntry is one model id the proxy exposes, with the metadata needed to
// describe it.
type modelEntry struct {
	ID          string
	Name        string
	Provider    string
	Upstream    string
	Passthrough bool
	Pricing     *config.ModelPricing
}

// modelEntries returns every model id an authenticated key may use: configured
// aliases plus ids discovered from passthrough providers, exposed as
// "<provider>/<id>". Aliases win on conflict.
func (s *Server) modelEntries(ctx context.Context, st *state, key *config.VirtualKey) []modelEntry {
	seen := make(map[string]struct{}, len(st.cfg.Models))
	entries := make([]modelEntry, 0, len(st.cfg.Models))

	for alias, model := range st.cfg.Models {
		if !st.cfg.ModelAllowed(key, alias) {
			continue
		}
		seen[alias] = struct{}{}
		entries = append(entries, modelEntry{
			ID:       alias,
			Name:     alias,
			Provider: model.Provider,
			Upstream: model.Upstream,
			Pricing:  model.Pricing,
		})
	}

	for _, providerName := range st.cfg.PassthroughProviderNames() {
		for _, id := range s.passthroughModels(ctx, st, providerName) {
			// Passthrough ids carry their provider as a prefix, matching the
			// shape of configured aliases and keeping ids from different
			// passthrough providers distinct.
			alias := providerName + "/" + id
			if _, exists := seen[alias]; exists {
				continue
			}
			// Policies may name the exposed id or its bare upstream id.
			if !st.cfg.ModelAllowed(key, alias) || !st.cfg.ModelAllowed(key, id) {
				continue
			}
			seen[alias] = struct{}{}
			entries = append(entries, modelEntry{
				ID:          alias,
				Name:        alias,
				Provider:    providerName,
				Upstream:    id,
				Passthrough: true,
			})
		}
	}

	sortModelEntries(entries)
	return entries
}

func sortModelEntries(entries []modelEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].ID < entries[j-1].ID; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

type openRouterModel struct {
	ID                  string             `json:"id"`
	CanonicalSlug       string             `json:"canonical_slug"`
	Name                string             `json:"name"`
	Created             int64              `json:"created"`
	Description         string             `json:"description"`
	ContextLength       int64              `json:"context_length"`
	Architecture        openRouterArch     `json:"architecture"`
	Pricing             *openRouterPricing `json:"pricing,omitempty"`
	TopProvider         openRouterTopProv  `json:"top_provider"`
	SupportedParameters []string           `json:"supported_parameters"`
	DefaultParameters   map[string]any     `json:"default_parameters"`
}

type openRouterArch struct {
	Modality        string   `json:"modality"`
	InputModalities []string `json:"input_modalities"`
	OutputModality  []string `json:"output_modalities"`
	Tokenizer       string   `json:"tokenizer"`
}

type openRouterPricing struct {
	Prompt         string                    `json:"prompt"`
	Completion     string                    `json:"completion"`
	Request        string                    `json:"request"`
	Image          string                    `json:"image"`
	InputCacheRead string                    `json:"input_cache_read,omitempty"`
	Overrides      []openRouterPriceOverride `json:"overrides,omitempty"`
}

type openRouterPriceOverride struct {
	MinPromptTokens int64  `json:"min_prompt_tokens"`
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read,omitempty"`
}

type openRouterTopProv struct {
	ContextLength       int64 `json:"context_length"`
	MaxCompletionTokens int64 `json:"max_completion_tokens"`
	IsModerated         bool  `json:"is_moderated"`
}

// openRouterModelList renders the entries as an OpenRouter /models payload.
// Metadata comes from the pricing catalog, with explicit config pricing winning.
func openRouterModelList(st *state, entries []modelEntry, created int64) map[string]any {
	catalog, _ := st.pricing.(modelCatalog)

	data := make([]openRouterModel, 0, len(entries))
	for _, entry := range entries {
		providerCfg := st.cfg.Providers[entry.Provider]

		var info pricing.ModelInfo
		var known bool
		if catalog != nil && providerCfg.PricingProvider != "" {
			info, known = catalog.LookupModel(providerCfg.PricingProvider, entry.Upstream)
		}

		inputModalities := normalizeModalities(info.InputModalities, known, info.Attachment)
		outputModalities := normalizeOutputModalities(info.OutputModalities)

		model := openRouterModel{
			ID:            entry.ID,
			CanonicalSlug: entry.ID,
			Name:          entry.Name,
			Created:       created,
			Description:   "",
			ContextLength: info.ContextLength,
			Architecture: openRouterArch{
				Modality:        modalityString(inputModalities, outputModalities),
				InputModalities: inputModalities,
				OutputModality:  outputModalities,
				Tokenizer:       "Other",
			},
			TopProvider: openRouterTopProv{
				ContextLength:       info.ContextLength,
				MaxCompletionTokens: info.MaxOutputTokens,
				IsModerated:         false,
			},
			SupportedParameters: supportedParameters(providerCfg.Type, info, known),
			DefaultParameters:   map[string]any{},
		}

		// A subscription provider bills per plan, not per token, so its models
		// have no list price to advertise. Catalog rates are still used for the
		// access log's API-equivalent estimate.
		if providerCfg.BillingMode != "subscription" {
			if pricingBody := openRouterPricingFor(entry.Pricing, info, known); pricingBody != nil {
				model.Pricing = pricingBody
			}
		}
		data = append(data, model)
	}

	return map[string]any{"data": data, "total_count": len(data)}
}

// openRouterPricingFor converts USD per million-token rates into OpenRouter's
// per-token strings. Config pricing wins over the catalog.
func openRouterPricingFor(modelPricing *config.ModelPricing, info pricing.ModelInfo, known bool) *openRouterPricing {
	if modelPricing != nil {
		prompt := modelPricing.InputPerMillion
		cached := prompt
		if modelPricing.CachedInputPerMillion != nil {
			cached = *modelPricing.CachedInputPerMillion
		}
		return &openRouterPricing{
			Prompt:         perToken(prompt),
			Completion:     perToken(modelPricing.OutputPerMillion),
			Request:        "0",
			Image:          "0",
			InputCacheRead: perToken(cached),
		}
	}
	if !known || len(info.RateTable) == 0 {
		return nil
	}

	base := info.RateTable[0].Rates
	pricingBody := &openRouterPricing{
		Prompt:         perToken(base.Input),
		Completion:     perToken(base.Output),
		Request:        "0",
		Image:          "0",
		InputCacheRead: perToken(base.CachedInput),
	}
	for _, tier := range info.RateTable[1:] {
		pricingBody.Overrides = append(pricingBody.Overrides, openRouterPriceOverride{
			MinPromptTokens: tier.MinPromptTokens,
			Prompt:          perToken(tier.Rates.Input),
			Completion:      perToken(tier.Rates.Output),
			InputCacheRead:  perToken(tier.Rates.CachedInput),
		})
	}
	return pricingBody
}

func perToken(perMillion float64) string {
	if perMillion <= 0 {
		return "0"
	}
	// Round to 12 decimal places so binary float noise (0.4/1e6) does not leak
	// into the per-token string OpenRouter clients parse.
	value := math.Round(perMillion/1_000_000*1e12) / 1e12
	if math.Abs(value) < 1e-15 {
		return "0"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func normalizeModalities(modalities []string, known, attachment bool) []string {
	if len(modalities) == 0 {
		if !known {
			return []string{"text"}
		}
		if attachment {
			return []string{"text", "image", "file"}
		}
		return []string{"text"}
	}
	normalized := make([]string, 0, len(modalities))
	seen := make(map[string]struct{}, len(modalities))
	for _, modality := range modalities {
		switch modality {
		case "pdf":
			modality = "file"
		case "text", "image", "audio", "video", "file":
		default:
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		normalized = append(normalized, modality)
	}
	if len(normalized) == 0 {
		return []string{"text"}
	}
	return normalized
}

func normalizeOutputModalities(modalities []string) []string {
	if len(modalities) == 0 {
		return []string{"text"}
	}
	return append([]string(nil), modalities...)
}

func modalityString(input, output []string) string {
	return strings.Join(input, "+") + "->" + strings.Join(output, "+")
}

// supportedParameters reports the request parameters clients may send. Values
// are conservative: a parameter is claimed only when the catalog confirms it, or
// when it is part of the OpenAI-compatible surface every provider accepts.
func supportedParameters(providerType string, info pricing.ModelInfo, known bool) []string {
	parameters := []string{"max_tokens", "max_completion_tokens", "stream", "stop"}
	if !known || info.ToolCall {
		parameters = append(parameters, "tools", "tool_choice", "response_format", "structured_outputs")
	}
	if known && info.Reasoning {
		parameters = append(parameters, "reasoning", "include_reasoning", "reasoning_effort")
	}
	// Codex-style backends reject sampling parameters.
	if providerType != "codex" && providerType != "chatgpt" {
		parameters = append(parameters, "temperature", "top_p", "seed")
	}
	return parameters
}
