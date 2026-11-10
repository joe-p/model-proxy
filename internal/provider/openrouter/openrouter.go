package openrouter

import (
	"time"

	"github.com/joe-p/model-proxy/internal/provider/openaicompat"
)

// Provider is an OpenAI-compatible transport preconfigured for OpenRouter.
type Provider = openaicompat.Provider

func New(name, baseURL, apiKey string, dialTimeout, responseHeaderTimeout time.Duration, referer, title string) (*Provider, error) {
	headers := make(map[string]string, 2)
	if referer != "" {
		headers["HTTP-Referer"] = referer
	}
	if title != "" {
		headers["X-Title"] = title
	}
	return openaicompat.New(openaicompat.Options{
		Name:                  name,
		BaseURL:               baseURL,
		APIKey:                apiKey,
		DialTimeout:           dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		Headers:               headers,
	})
}
