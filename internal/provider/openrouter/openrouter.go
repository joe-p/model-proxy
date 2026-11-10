package openrouter

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

type Provider struct {
	name    string
	baseURL string
	apiKey  string
	referer string
	title   string
	client  *http.Client
}

func New(name, baseURL, apiKey string, dialTimeout, responseHeaderTimeout time.Duration, referer, title string) (*Provider, error) {
	if name == "" {
		return nil, fmt.Errorf("provider name must not be empty")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("provider base URL must not be empty")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("provider API key must not be empty")
	}
	if dialTimeout <= 0 {
		return nil, fmt.Errorf("provider dial timeout must be positive")
	}
	if responseHeaderTimeout <= 0 {
		return nil, fmt.Errorf("provider response header timeout must be positive")
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}

	return &Provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		referer: referer,
		title:   title,
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
	endpoint := p.baseURL + "/chat/completions"
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
	if p.referer != "" {
		httpReq.Header.Set("HTTP-Referer", p.referer)
	}
	if p.title != "" {
		httpReq.Header.Set("X-Title", p.title)
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
