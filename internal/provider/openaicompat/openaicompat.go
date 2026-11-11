// Package openaicompat implements the OpenAI-compatible chat completions
// transport shared by providers such as OpenRouter and Private AI Gateway.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
)

// DefaultPath is appended to the configured base URL for chat requests.
const DefaultPath = "/chat/completions"

// DefaultResponsesPath is appended to the configured base URL for Responses
// API requests.
const DefaultResponsesPath = "/responses"

// DefaultModelsPath is appended to the configured base URL for model listing.
const DefaultModelsPath = "/models"

// DefaultImagesPath is appended to the configured base URL for image
// generation. Some APIs use a different path, such as OpenRouter's
// "/images", so it is configurable per provider.
const DefaultImagesPath = "/images/generations"

// maxModelsBodyBytes caps how much of a model listing response is read.
const maxModelsBodyBytes = 4 << 20

// TokenSource resolves a per-request upstream credential. Implementations such
// as the Codex authenticator read and refresh credentials from disk.
type TokenSource interface {
	Token(ctx context.Context) (accessToken, accountID string, err error)
}

// Options configures an OpenAI-compatible provider.
type Options struct {
	Name                  string
	BaseURL               string
	APIKey                string
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration

	// TokenSource supplies a dynamic credential instead of APIKey. When set,
	// it takes precedence over APIKey.
	TokenSource TokenSource

	// Path is appended to BaseURL for chat completions. DefaultPath is used
	// when empty.
	Path string

	// ResponsesPath is appended to BaseURL for Responses API calls.
	// DefaultResponsesPath is used when empty.
	ResponsesPath string

	// ModelsPath is appended to BaseURL for model listing. DefaultModelsPath is
	// used when empty.
	ModelsPath string

	// ImagesPath is appended to BaseURL for image generation requests.
	// DefaultImagesPath is used when empty.
	ImagesPath string

	// ModelsQuery is an optional raw query appended to the model listing URL,
	// such as Codex's required client_version parameter.
	ModelsQuery string

	// UsageURL is an absolute URL for subscription usage queries. It is
	// optional and only used by providers that expose a usage endpoint.
	UsageURL string

	// Headers are additional request headers, such as OpenRouter's
	// attribution headers. They are set after the standard headers.
	Headers map[string]string
}

type Provider struct {
	name          string
	baseURL       string
	path          string
	responsesPath string
	imagesPath    string
	modelsPath    string
	modelsQuery   string
	usageURL      string
	apiKey        string
	tokenSource   TokenSource
	headers       http.Header
	client        *http.Client
}

func New(opts Options) (*Provider, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("provider name must not be empty")
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("provider base URL must not be empty")
	}
	if opts.APIKey == "" && opts.TokenSource == nil {
		return nil, fmt.Errorf("provider API key or token source must not be empty")
	}
	if opts.DialTimeout <= 0 {
		return nil, fmt.Errorf("provider dial timeout must be positive")
	}
	if opts.ResponseHeaderTimeout <= 0 {
		return nil, fmt.Errorf("provider response header timeout must be positive")
	}

	path := normalizePath(opts.Path, DefaultPath)
	responsesPath := normalizePath(opts.ResponsesPath, DefaultResponsesPath)
	imagesPath := normalizePath(opts.ImagesPath, DefaultImagesPath)
	modelsPath := normalizePath(opts.ModelsPath, DefaultModelsPath)

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
		name:          opts.Name,
		baseURL:       strings.TrimRight(opts.BaseURL, "/"),
		path:          path,
		responsesPath: responsesPath,
		imagesPath:    imagesPath,
		modelsPath:    modelsPath,
		modelsQuery:   strings.TrimPrefix(strings.TrimSpace(opts.ModelsQuery), "?"),
		usageURL:      strings.TrimSpace(opts.UsageURL),
		apiKey:        opts.APIKey,
		tokenSource:   opts.TokenSource,
		headers:       headers,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func normalizePath(path, fallback string) string {
	if path == "" {
		path = fallback
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func (p *Provider) Name() string {
	return p.name
}

func (p *Provider) ChatCompletions(ctx context.Context, req provider.Request) (*http.Response, error) {
	return p.do(ctx, p.path, req)
}

func (p *Provider) Responses(ctx context.Context, req provider.Request) (*http.Response, error) {
	return p.do(ctx, p.responsesPath, req)
}

// Images sends an OpenAI-compatible image generation request. The upstream
// response is returned unmodified; image payloads are opaque to the proxy.
func (p *Provider) Images(ctx context.Context, req provider.Request) (*http.Response, error) {
	return p.do(ctx, p.imagesPath, req)
}

// Usage fetches subscription usage from the configured usage URL. The upstream
// response is returned unmodified; the server decides how to expose it.
func (p *Provider) Usage(ctx context.Context) (*http.Response, error) {
	if p.usageURL == "" {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: errors.New("usage endpoint is not configured")}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.usageURL, nil)
	if err != nil {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: err}
	}
	httpReq.Header.Set("Accept", "application/json")
	if err := p.authorize(ctx, httpReq); err != nil {
		return nil, err
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

func (p *Provider) do(ctx context.Context, path string, req provider.Request) (*http.Response, error) {
	endpoint := p.baseURL + path
	if req.Query != "" {
		endpoint += "?" + req.Query
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: err}
	}

	if err := p.authorize(ctx, httpReq); err != nil {
		return nil, err
	}

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

// authorize applies the provider credential to a request. Token sources are
// consulted per request so refreshed credentials are picked up immediately.
func (p *Provider) authorize(ctx context.Context, req *http.Request) error {
	token := p.apiKey
	if p.tokenSource != nil {
		var accountID string
		var err error
		token, accountID, err = p.tokenSource.Token(ctx)
		if err != nil {
			return &provider.Error{Kind: provider.Unavailable, Err: fmt.Errorf("resolve upstream credential: %w", err)}
		}
		if token == "" {
			return &provider.Error{Kind: provider.Unavailable, Err: errors.New("upstream credential is empty")}
		}
		if accountID != "" {
			req.Header.Set("ChatGPT-Account-Id", accountID)
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for key, values := range p.headers {
		for _, value := range values {
			req.Header.Set(key, value)
		}
	}
	return nil
}

// Models lists the upstream model ids. It understands both the OpenAI
// shape ({"data":[{"id":...}]}) and the Codex shape
// ({"models":[{"slug":...}]}), so a single implementation serves every
// provider.
func (p *Provider) Models(ctx context.Context) ([]string, error) {
	endpoint := p.baseURL + p.modelsPath
	if p.modelsQuery != "" {
		endpoint += "?" + p.modelsQuery
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: err}
	}
	httpReq.Header.Set("Accept", "application/json")
	if err := p.authorize(ctx, httpReq); err != nil {
		return nil, err
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
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes))
	if err != nil {
		return nil, &provider.Error{Kind: provider.Unavailable, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream model listing returned %d: %s", resp.StatusCode, snippet(body))
	}

	ids, err := parseModelIDs(body)
	if err != nil {
		return nil, fmt.Errorf("parse upstream model listing: %w", err)
	}
	return ids, nil
}

func parseModelIDs(body []byte) ([]string, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Slug string `json:"slug"`
			ID   string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(payload.Data)+len(payload.Models))
	ids := make([]string, 0, len(payload.Data)+len(payload.Models))
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, entry := range payload.Data {
		add(entry.ID)
	}
	for _, entry := range payload.Models {
		if entry.Slug != "" {
			add(entry.Slug)
			continue
		}
		add(entry.ID)
	}
	return ids, nil
}

func snippet(body []byte) string {
	const maxSnippet = 200
	text := strings.TrimSpace(string(body))
	if len(text) > maxSnippet {
		text = text[:maxSnippet] + "..."
	}
	return text
}
