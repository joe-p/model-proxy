// Package codex implements the ChatGPT/Codex provider using credentials from
// the OpenAI Codex CLI. Credentials are read from <auth_dir>/auth.json and
// refreshed automatically, matching LiteLLM's ChatGPT auth behavior.
package codex

import (
	"net/url"
	"strings"
	"time"

	"github.com/joe-p/model-proxy/internal/codexauth"
	"github.com/joe-p/model-proxy/internal/provider/openaicompat"
)

// DefaultOriginator is the originator header sent by the Codex CLI.
const DefaultOriginator = "codex_cli_rs"

// Provider is an OpenAI-compatible transport preconfigured for Codex.
type Provider = openaicompat.Provider

// New returns a Codex provider. When apiKey is empty, credentials are resolved
// from authDir (or $CODEX_HOME / ~/.codex when authDir is empty) on every
// request. A configured apiKey is used as a static token instead.
//
// clientVersion is required by the Codex model listing endpoint. When empty,
// the provider still serves chat and responses; only model discovery fails.
func New(name, baseURL, apiKey, authDir, clientVersion string, dialTimeout, responseHeaderTimeout time.Duration) (*Provider, error) {
	headers := map[string]string{
		"originator": DefaultOriginator,
	}
	opts := openaicompat.Options{
		Name:                  name,
		BaseURL:               baseURL,
		APIKey:                apiKey,
		DialTimeout:           dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		Headers:               headers,
		ModelsQuery:           modelsQuery(clientVersion),
	}
	if apiKey == "" {
		opts.TokenSource = codexauth.New(codexauth.Options{Dir: authDir})
	} else if accountID := codexauth.AccountID(apiKey); accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	return openaicompat.New(opts)
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
