// Package openai implements the generic OpenAI-compatible chat completions
// transport. Providers such as Private AI Gateway use this transport directly.
package openai

import (
	"time"

	"github.com/joe-p/model-proxy/internal/provider/openaicompat"
)

// Provider is an OpenAI-compatible chat completions provider.
type Provider = openaicompat.Provider

func New(name, baseURL, apiKey string, dialTimeout, responseHeaderTimeout time.Duration) (*Provider, error) {
	return openaicompat.New(openaicompat.Options{
		Name:                  name,
		BaseURL:               baseURL,
		APIKey:                apiKey,
		DialTimeout:           dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	})
}
