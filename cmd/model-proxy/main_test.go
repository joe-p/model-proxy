package main

import (
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/config"
)

func TestBuildProvidersSupportsOpenAICompatibleAliases(t *testing.T) {
	for _, providerType := range []string{"openai-compatible", "openai", "private-ai-gateway", "phala"} {
		t.Run(providerType, func(t *testing.T) {
			cfg := &config.Config{
				Providers: map[string]config.ProviderConfig{
					"phala": {
						Name:                  "phala",
						Type:                  providerType,
						BaseURL:               "https://inference.phala.com/v1",
						APIKey:                "key",
						DialTimeout:           time.Second,
						ResponseHeaderTimeout: time.Second,
					},
				},
				Models: map[string]config.ModelConfig{
					"private": {Alias: "private", Provider: "phala", Upstream: "z-ai/glm-5.3"},
				},
			}

			providers, err := buildProviders(cfg)
			if err != nil {
				t.Fatalf("buildProviders: %v", err)
			}
			if got := providers["phala"].Name(); got != "phala" {
				t.Fatalf("provider name = %q", got)
			}
		})
	}
}

func TestBuildProvidersRejectsUnknownType(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"custom": {
				Name:                  "custom",
				Type:                  "unknown",
				BaseURL:               "https://example.test/v1",
				APIKey:                "key",
				DialTimeout:           time.Second,
				ResponseHeaderTimeout: time.Second,
			},
		},
		Models: map[string]config.ModelConfig{
			"model": {Alias: "model", Provider: "custom", Upstream: "model"},
		},
	}
	if _, err := buildProviders(cfg); err == nil {
		t.Fatal("expected unsupported provider type error")
	}
}
