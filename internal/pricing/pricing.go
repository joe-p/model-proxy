// Package pricing loads model list prices from models.dev and estimates
// API-equivalent costs for observed token usage.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

const (
	// DefaultURL is the models.dev catalog endpoint.
	DefaultURL = "https://models.dev/api.json"
	// DefaultRefreshInterval is how often the catalog is refreshed.
	DefaultRefreshInterval = 24 * time.Hour
	// DefaultTimeout bounds a catalog fetch.
	DefaultTimeout = 30 * time.Second

	maxCatalogBytes = 32 << 20
	maxRetryDelay   = 5 * time.Minute
)

// Usage is the token usage needed for a cost estimate.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     *int64
	CacheWriteTokens *int64
}

// Rates are USD prices per million tokens.
type Rates struct {
	Input       float64
	CachedInput float64
	Output      float64
	CacheWrite  float64
}

// ModelInfo is catalog metadata for one provider model.
type ModelInfo struct {
	ContextLength    int64
	MaxOutputTokens  int64
	InputModalities  []string
	OutputModalities []string
	Reasoning        bool
	ToolCall         bool
	Attachment       bool
	Rates            Rates
	RateTable        []TierRate
}

// TierRate is a context-tier price that applies at or above MinPromptTokens.
type TierRate struct {
	MinPromptTokens int64
	Rates           Rates
}

// EstimateUsage computes an API-equivalent cost from token counts and rates.
// Reasoning tokens are already included in CompletionTokens and must not be
// added separately.
func EstimateUsage(usage Usage, rates Rates) float64 {
	cached := int64(0)
	if usage.CachedTokens != nil {
		cached = *usage.CachedTokens
	}
	cacheWrite := int64(0)
	if usage.CacheWriteTokens != nil {
		cacheWrite = *usage.CacheWriteTokens
	}

	uncached := usage.PromptTokens - cached - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	if cached < 0 {
		cached = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}

	cost := float64(uncached)*rates.Input +
		float64(cached)*rates.CachedInput +
		float64(cacheWrite)*rates.CacheWrite +
		float64(usage.CompletionTokens)*rates.Output
	return cost / 1_000_000
}

// Options configures a pricing Service.
type Options struct {
	URL             string
	CachePath       string
	RefreshInterval time.Duration
	Timeout         time.Duration
	Client          *http.Client
	Logger          *slog.Logger
	Now             func() time.Time
}

// Service keeps a models.dev catalog in memory and refreshes it on a timer.
type Service struct {
	opts        Options
	client      *http.Client
	logger      *slog.Logger
	now         func() time.Time
	catalog     atomic.Pointer[Catalog]
	lastAttempt atomic.Int64
	lastSuccess atomic.Int64
}

// New creates a pricing service and loads any cached catalog synchronously.
// Network refreshes happen in the background after Start.
func New(opts Options) *Service {
	if opts.URL == "" {
		opts.URL = DefaultURL
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
	}

	s := &Service{
		opts:   opts,
		client: client,
		logger: opts.Logger,
		now:    opts.Now,
	}
	s.loadCache()
	return s
}

// Start begins the refresh loop. It returns immediately.
func (s *Service) Start(ctx context.Context) {
	go s.run(ctx)
}

func (s *Service) run(ctx context.Context) {
	for {
		delay := s.nextDelay()
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.Refresh(ctx)
	}
}

func (s *Service) nextDelay() time.Duration {
	now := s.now()
	interval := s.opts.RefreshInterval
	lastAttempt := s.lastAttempt.Load()
	if lastAttempt == 0 {
		return 0
	}

	lastSuccess := s.lastSuccess.Load()
	if lastSuccess == 0 || lastAttempt > lastSuccess {
		retry := interval
		if retry > maxRetryDelay {
			retry = maxRetryDelay
		}
		delay := retry - now.Sub(time.Unix(0, lastAttempt))
		if delay < 0 {
			return 0
		}
		return delay
	}

	delay := interval - now.Sub(time.Unix(0, lastSuccess))
	if delay < 0 {
		return 0
	}
	return delay
}

// Refresh fetches the catalog and updates the cache. Failures leave the
// previous catalog in place.
func (s *Service) Refresh(ctx context.Context) bool {
	s.lastAttempt.Store(s.now().UnixNano())

	reqCtx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.opts.URL, nil)
	if err != nil {
		s.logRefreshError(ctx, err)
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "model-proxy")

	resp, err := s.client.Do(req)
	if err != nil {
		s.logRefreshError(ctx, err)
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes))
	if err != nil {
		s.logRefreshError(ctx, err)
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.logRefreshError(ctx, fmt.Errorf("models.dev returned status %d", resp.StatusCode))
		return false
	}

	catalog, err := parseCatalog(body)
	if err != nil {
		s.logRefreshError(ctx, err)
		return false
	}

	now := s.now()
	s.catalog.Store(&catalog)
	s.lastSuccess.Store(now.UnixNano())

	if s.opts.CachePath != "" {
		if err := writeFileAtomic(s.opts.CachePath, body, 0o600); err != nil {
			s.logger.Warn("pricing catalog cache write failed", "path", s.opts.CachePath, "error", err)
		}
	}
	s.logger.Info("pricing catalog updated",
		"source", "models.dev",
		"url", s.opts.URL,
		"providers", len(catalog),
		"models", catalogModelCount(catalog),
		"cache", s.opts.CachePath,
	)
	return true
}

// Estimate returns an API-equivalent cost for a model, if the catalog has it.
func (s *Service) Estimate(providerID, modelID string, usage Usage) (float64, bool) {
	rates, ok := s.LookupRates(providerID, modelID, usage.PromptTokens)
	if !ok {
		return 0, false
	}
	return EstimateUsage(usage, rates), true
}

// LookupRates returns list rates for a model, applying the context tier that
// matches promptTokens.
func (s *Service) LookupRates(providerID, modelID string, promptTokens int64) (Rates, bool) {
	catalog := s.catalog.Load()
	if catalog == nil {
		return Rates{}, false
	}
	provider, ok := (*catalog)[providerID]
	if !ok {
		return Rates{}, false
	}
	model, ok := provider.Models[modelID]
	if !ok || model.Cost == nil {
		return Rates{}, false
	}
	return model.Cost.rates(promptTokens), true
}

func (s *Service) loadCache() {
	if s.opts.CachePath == "" {
		return
	}
	info, err := os.Stat(s.opts.CachePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("pricing catalog cache stat failed", "path", s.opts.CachePath, "error", err)
		}
		return
	}
	data, err := os.ReadFile(s.opts.CachePath)
	if err != nil {
		s.logger.Warn("pricing catalog cache read failed", "path", s.opts.CachePath, "error", err)
		return
	}
	catalog, err := parseCatalog(data)
	if err != nil {
		s.logger.Warn("pricing catalog cache is invalid", "path", s.opts.CachePath, "error", err)
		return
	}

	stamp := info.ModTime()
	if stamp.IsZero() {
		stamp = s.now()
	}
	s.catalog.Store(&catalog)
	s.lastAttempt.Store(stamp.UnixNano())
	s.lastSuccess.Store(stamp.UnixNano())
	s.logger.Info("pricing catalog loaded from cache",
		"source", "models.dev",
		"path", s.opts.CachePath,
		"providers", len(catalog),
		"models", catalogModelCount(catalog),
		"as_of", stamp.UTC().Format(time.RFC3339),
	)
}

func (s *Service) logRefreshError(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	s.logger.Warn("pricing catalog update failed", "source", "models.dev", "url", s.opts.URL, "error", err)
}

type Catalog map[string]catalogProvider

type catalogProvider struct {
	ID     string                  `json:"id"`
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	ID         string       `json:"id"`
	Cost       *catalogCost `json:"cost"`
	Limit      catalogLimit `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Reasoning  bool `json:"reasoning"`
	ToolCall   bool `json:"tool_call"`
	Attachment bool `json:"attachment"`
}

type catalogLimit struct {
	Context int64 `json:"context"`
	Output  int64 `json:"output"`
}

type catalogCost struct {
	Input           float64       `json:"input"`
	Output          float64       `json:"output"`
	CacheRead       *float64      `json:"cache_read"`
	CacheWrite      *float64      `json:"cache_write"`
	Tiers           []catalogTier `json:"tiers"`
	ContextOver200k *catalogRate  `json:"context_over_200k"`
}

type catalogTier struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	Tier       struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

type catalogRate struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

func (c *catalogCost) rates(promptTokens int64) Rates {
	var selected *catalogTier
	for i := range c.Tiers {
		tier := &c.Tiers[i]
		if tier.Tier.Type != "context" || promptTokens <= tier.Tier.Size {
			continue
		}
		if selected == nil || tier.Tier.Size >= selected.Tier.Size {
			selected = tier
		}
	}
	if selected != nil {
		return buildRates(selected.Input, selected.Output, selected.CacheRead, selected.CacheWrite)
	}
	if c.ContextOver200k != nil && promptTokens > 200_000 {
		return buildRates(c.ContextOver200k.Input, c.ContextOver200k.Output, c.ContextOver200k.CacheRead, c.ContextOver200k.CacheWrite)
	}
	return buildRates(c.Input, c.Output, c.CacheRead, c.CacheWrite)
}

// ModelInfo is catalog metadata used to describe a model to clients.
// RateTable is ordered by MinPromptTokens ascending; the last entry at or below
// the observed prompt size applies.
func (i ModelInfo) RatesFor(promptTokens int64) Rates {
	selected := i.Rates
	for _, tier := range i.RateTable {
		if promptTokens >= tier.MinPromptTokens {
			selected = tier.Rates
		}
	}
	return selected
}

// LookupModel returns catalog metadata for a provider model.
func (s *Service) LookupModel(providerID, modelID string) (ModelInfo, bool) {
	catalog := s.catalog.Load()
	if catalog == nil {
		return ModelInfo{}, false
	}
	provider, ok := (*catalog)[providerID]
	if !ok {
		return ModelInfo{}, false
	}
	model, ok := provider.Models[modelID]
	if !ok {
		return ModelInfo{}, false
	}

	info := ModelInfo{
		ContextLength:    model.Limit.Context,
		MaxOutputTokens:  model.Limit.Output,
		InputModalities:  model.Modalities.Input,
		OutputModalities: model.Modalities.Output,
		Reasoning:        model.Reasoning,
		ToolCall:         model.ToolCall,
		Attachment:       model.Attachment,
	}
	if model.Cost != nil {
		info.Rates = buildRates(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite)
		info.RateTable = model.Cost.rateTable()
	}
	return info, true
}

func (c *catalogCost) rateTable() []TierRate {
	table := []TierRate{{MinPromptTokens: 0, Rates: buildRates(c.Input, c.Output, c.CacheRead, c.CacheWrite)}}
	for _, tier := range c.Tiers {
		if tier.Tier.Type != "context" {
			continue
		}
		table = append(table, TierRate{
			MinPromptTokens: tier.Tier.Size,
			Rates:           buildRates(tier.Input, tier.Output, tier.CacheRead, tier.CacheWrite),
		})
	}
	// models.dev duplicates its tiered pricing under context_over_200k for older
	// catalog entries. Prefer the explicit tiers when present.
	if c.ContextOver200k != nil && len(c.Tiers) == 0 {
		table = append(table, TierRate{
			MinPromptTokens: 200_000,
			Rates:           buildRates(c.ContextOver200k.Input, c.ContextOver200k.Output, c.ContextOver200k.CacheRead, c.ContextOver200k.CacheWrite),
		})
	}
	sort.SliceStable(table, func(a, b int) bool { return table[a].MinPromptTokens < table[b].MinPromptTokens })
	return table
}

func buildRates(input, output float64, cacheRead, cacheWrite *float64) Rates {
	rates := Rates{Input: input, Output: output, CachedInput: input, CacheWrite: input}
	if cacheRead != nil {
		rates.CachedInput = *cacheRead
	}
	if cacheWrite != nil {
		rates.CacheWrite = *cacheWrite
	}
	return rates
}

func parseCatalog(data []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	if len(catalog) == 0 {
		return nil, errors.New("models.dev catalog is empty")
	}
	return catalog, nil
}

func catalogModelCount(catalog Catalog) int {
	count := 0
	for _, provider := range catalog {
		count += len(provider.Models)
	}
	return count
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	file, err := os.CreateTemp(dir, ".models.dev-*.json")
	if err != nil {
		return err
	}
	tmpName := file.Name()
	defer os.Remove(tmpName)

	if err := file.Chmod(perm); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
