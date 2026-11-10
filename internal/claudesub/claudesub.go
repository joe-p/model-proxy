// Package claudesub implements the transparent Anthropic Messages API
// transport used by the /claude_sub/ path. The client supplies its own Claude
// Code OAuth token, so the transport never holds or refreshes a credential.
package claudesub

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

// DefaultBaseURL is the Anthropic API root used by /claude_sub/.
const DefaultBaseURL = "https://api.anthropic.com"

const (
	dialTimeout           = 10 * time.Second
	responseHeaderTimeout = 120 * time.Second
)

// Transport forwards requests to Anthropic without translating the request or
// response.
type Transport struct {
	baseURL string
	client  *http.Client
}

// New returns a transparent Claude subscription transport. An empty baseURL
// uses DefaultBaseURL.
func New(baseURL string) *Transport {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
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

	return &Transport{
		baseURL: baseURL,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Proxy sends a request to Anthropic unchanged apart from the URL prefix. It
// does not add an Authorization header; the client's credential is already in
// req.Header.
func (t *Transport) Proxy(ctx context.Context, req provider.ProxyRequest) (*http.Response, error) {
	path := req.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	endpoint := t.baseURL + path
	if req.Query != "" {
		endpoint += "?" + req.Query
	}

	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: err}
	}
	httpReq.Header = req.Header.Clone()
	if httpReq.Header == nil {
		httpReq.Header = make(http.Header)
	}
	if len(req.Body) > 0 && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		kind := provider.Unavailable
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
			kind = provider.Timeout
		}
		return nil, &provider.Error{Kind: kind, Err: fmt.Errorf("claude subscription upstream: %w", err)}
	}
	return resp, nil
}
