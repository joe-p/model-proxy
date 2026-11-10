// Package privateaigateway implements the OpenAI-compatible transport for
// Private AI Gateway (https://github.com/Dstack-TEE/private-ai-gateway).
package privateaigateway

import (
	"time"

	"github.com/joe-p/model-proxy/internal/provider/openaicompat"
)

// Provider is an OpenAI-compatible transport preconfigured for Private AI
// Gateway. The gateway's ACI request constraints, such as
// {"provider":{"aci_verified":true}}, are supplied through extra_body.
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
