// Package openaicompat implements the OpenAI-compatible chat completions
// transport shared by providers such as OpenRouter and Private AI Gateway.
package openaicompat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
)

// DefaultPath is appended to the configured base URL for chat requests.
const DefaultPath = "/chat/completions"

// Options configures an OpenAI-compatible provider.
type Options struct {
	Name                  string
	BaseURL               string
	APIKey                string
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration

	// Path is appended to BaseURL. DefaultPath is used when empty.
	Path string

	// Headers are additional request headers, such as OpenRouter's
	// attribution headers. They are set after the standard headers.
	Headers map[string]string
}

type Provider struct {
	name    string
	baseURL string
	path    string
	apiKey  string
	headers http.Header
	client  *http.Client
}

func New(opts Options) (*Provider, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("provider name must not be empty")
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("provider base URL must not be empty")
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("provider API key must not be empty")
	}
	if opts.DialTimeout <= 0 {
		return nil, fmt.Errorf("provider dial timeout must be positive")
	}
	if opts.ResponseHeaderTimeout <= 0 {
		return nil, fmt.Errorf("provider response header timeout must be positive")
	}

	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	headers := make(http.Header, len(opts.Headers))
	for key, value := range opts.Headers {
		headers.Set(key, value)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   opts.DialTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}

	return &Provider{
		name:    opts.Name,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		path:    path,
		apiKey:  opts.APIKey,
		headers: headers,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (p *Provider) Name() string {
	return p.name
}

func (p *Provider) ChatCompletions(ctx context.Context, req provider.Request) (*http.Response, error) {
	endpoint := p.baseURL + p.path
	if req.Query != "" {
		endpoint += "?" + req.Query
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: err}
	}

	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Accept != "" {
		httpReq.Header.Set("Accept", req.Accept)
	} else if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if req.RequestID != "" {
		httpReq.Header.Set("X-Request-ID", req.RequestID)
	}
	for key, values := range p.headers {
		for _, value := range values {
			httpReq.Header.Set(key, value)
		}
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		kind := provider.Unavailable
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
			kind = provider.Timeout
		}
		return nil, &provider.Error{Kind: kind, Err: err}
	}
	return resp, nil
}
