// Package codex implements the ChatGPT/Codex provider using credentials from
// the OpenAI Codex CLI. Credentials are read from <auth_dir>/auth.json and
// refreshed automatically, matching LiteLLM's ChatGPT auth behavior.
package codex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joe-p/model-proxy/internal/codexauth"
	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/openaicompat"
	"github.com/joe-p/model-proxy/internal/responsesbridge"
)

// DefaultOriginator is the originator header sent by the Codex CLI.
const DefaultOriginator = "codex_cli_rs"

// Provider is an OpenAI-compatible Responses API transport preconfigured for
// Codex. Chat Completions requests are bridged to the Responses API because
// the Codex backend does not serve /chat/completions.
type Provider struct {
	upstream *openaicompat.Provider
}

// New returns a Codex provider. When apiKey is empty, credentials are resolved
// from authDir (or $CODEX_HOME / ~/.codex when authDir is empty) on every
// request. A configured apiKey is used as a static token instead.
//
// clientVersion is required by the Codex model listing endpoint. When empty,
// the provider still serves chat and responses; only model discovery fails.
func New(name, baseURL, apiKey, authDir, clientVersion string, dialTimeout, responseHeaderTimeout time.Duration) (*Provider, error) {
	usageURL, err := usageEndpoint(baseURL)
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		"originator":  DefaultOriginator,
		"OpenAI-Beta": "responses=experimental",
	}
	opts := openaicompat.Options{
		Name:                  name,
		BaseURL:               baseURL,
		APIKey:                apiKey,
		DialTimeout:           dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		Headers:               headers,
		ModelsQuery:           modelsQuery(clientVersion),
		UsageURL:              usageURL,
	}
	if apiKey == "" {
		opts.TokenSource = codexauth.New(codexauth.Options{Dir: authDir})
	} else if accountID := codexauth.AccountID(apiKey); accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	upstream, err := openaicompat.New(opts)
	if err != nil {
		return nil, err
	}
	return &Provider{upstream: upstream}, nil
}

func (p *Provider) Name() string {
	return p.upstream.Name()
}

// ChatCompletions bridges the Chat Completions contract onto the Responses API.
func (p *Provider) ChatCompletions(ctx context.Context, req provider.Request) (*http.Response, error) {
	return responsesbridge.ChatCompletions(ctx, req, p.upstream.Responses)
}

func (p *Provider) Responses(ctx context.Context, req provider.Request) (*http.Response, error) {
	return p.upstream.Responses(ctx, req)
}

func (p *Provider) Models(ctx context.Context) ([]string, error) {
	return p.upstream.Models(ctx)
}

// Usage fetches ChatGPT subscription usage from the Codex backend. The raw
// response is returned so clients can consume the same payload as the Codex
// CLI's /backend-api/wham/usage endpoint.
func (p *Provider) Usage(ctx context.Context) (*http.Response, error) {
	return p.upstream.Usage(ctx)
}

// usageEndpoint derives ChatGPT's wham/usage URL from the configured Codex
// base URL. The Codex backend base URL is typically
// https://chatgpt.com/backend-api/codex, so the usage endpoint lives at the
// same /backend-api prefix.
func usageEndpoint(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse Codex base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("Codex base URL must include a scheme and host")
	}

	prefix := ""
	if i := strings.Index(u.Path, "/backend-api"); i >= 0 {
		prefix = u.Path[:i]
	}
	u.Path = prefix + "/backend-api/wham/usage"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// modelsQuery builds the client_version parameter the Codex model listing
// requires.
func modelsQuery(clientVersion string) string {
	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		return ""
	}
	return "client_version=" + url.QueryEscape(clientVersion)
}
