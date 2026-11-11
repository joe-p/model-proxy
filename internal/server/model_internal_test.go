package server

import (
	"testing"

	"github.com/joe-p/model-proxy/internal/config"
)

func modelTestConfig(passthrough ...string) *config.Config {
	providers := map[string]config.ProviderConfig{
		"openrouter": {Name: "openrouter"},
	}
	for _, name := range passthrough {
		providers[name] = config.ProviderConfig{Name: name, PassthroughModels: true}
	}
	return &config.Config{
		Providers: providers,
		Models: map[string]config.ModelConfig{
			"fast": {Alias: "fast", Provider: "openrouter", Upstream: "openai/gpt-4o-mini"},
		},
	}
}

func TestResolveModelPassthrough(t *testing.T) {
	cfg := modelTestConfig("codex", "a/b")

	tests := []struct {
		name         string
		request      string
		wantProvider string
		wantUpstream string
		wantCode     string
	}{
		{
			name:         "namespaced id selects provider",
			request:      "codex/gpt-5.6-sol",
			wantProvider: "codex",
			wantUpstream: "gpt-5.6-sol",
		},
		{
			name:         "namespaced id keeps upstream slashes",
			request:      "codex/openai/gpt-5.6-sol",
			wantProvider: "codex",
			wantUpstream: "openai/gpt-5.6-sol",
		},
		{
			name:         "longest provider prefix wins",
			request:      "a/b/model",
			wantProvider: "a/b",
			wantUpstream: "model",
		},
		{
			name:     "prefix without passthrough is rejected",
			request:  "openrouter/model",
			wantCode: "invalid_request",
		},
		{
			name:     "bare id is ambiguous with two passthrough providers",
			request:  "mystery-model",
			wantCode: "invalid_request",
		},
		{
			name:         "configured alias wins",
			request:      "fast",
			wantProvider: "openrouter",
			wantUpstream: "openai/gpt-4o-mini",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := resolveModel(cfg, nil, tt.request)
			if tt.wantCode != "" {
				if err == nil || err.Code != tt.wantCode {
					t.Fatalf("err = %+v, want code %q", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %+v", err)
			}
			if model.Provider != tt.wantProvider || model.Upstream != tt.wantUpstream {
				t.Fatalf("model = %+v, want provider %q upstream %q", model, tt.wantProvider, tt.wantUpstream)
			}
			if model.Alias != tt.request {
				t.Fatalf("alias = %q, want %q", model.Alias, tt.request)
			}
			wantPassthrough := tt.request != "fast"
			if model.Passthrough != wantPassthrough {
				t.Fatalf("passthrough = %v, want %v", model.Passthrough, wantPassthrough)
			}
		})
	}
}

func TestResolveModelBarePassthroughSingleProvider(t *testing.T) {
	cfg := modelTestConfig("codex")

	model, err := resolveModel(cfg, nil, "gpt-5.6-sol")
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if model.Provider != "codex" || model.Upstream != "gpt-5.6-sol" || model.Alias != "gpt-5.6-sol" {
		t.Fatalf("model = %+v, want codex passthrough with unchanged id", model)
	}
}

func TestResolveModelPassthroughPolicies(t *testing.T) {
	cfg := modelTestConfig("codex")
	cfg.Policies = map[string]config.PolicyConfig{
		"deny-bare": {Name: "deny-bare", Mode: config.PolicyBlacklist, Models: map[string]struct{}{"gpt-5.6-sol": {}}},
		"deny-full": {Name: "deny-full", Mode: config.PolicyBlacklist, Models: map[string]struct{}{"codex/gpt-5.6-sol": {}}},
		"only-fast": {Name: "only-fast", Mode: config.PolicyWhitelist, Models: map[string]struct{}{"fast": {}}},
	}

	for _, name := range []string{"gpt-5.6-sol", "codex/gpt-5.6-sol"} {
		for _, policy := range []string{"deny-bare", "deny-full"} {
			key := &config.VirtualKey{Name: "k", Key: "k", Policy: policy}
			if _, err := resolveModel(cfg, key, name); err == nil || err.Code != "model_not_allowed" {
				t.Fatalf("%s under %s: err = %+v, want model_not_allowed", name, policy, err)
			}
		}
	}

	key := &config.VirtualKey{Name: "k", Key: "k", Policy: "only-fast"}
	if _, err := resolveModel(cfg, key, "codex/gpt-5.6-sol"); err == nil || err.Code != "model_not_allowed" {
		t.Fatalf("whitelist passthrough err = %+v, want model_not_allowed", err)
	}
}
