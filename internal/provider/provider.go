package provider

import (
	"context"
	"fmt"
	"net/http"
)

// Request is the provider-neutral input for an upstream POST request. The
// server fills the fields a given endpoint understands.
type Request struct {
	UpstreamModel string
	Body          []byte
	Stream        bool
	Accept        string
	RequestID     string
	Query         string
}

// ProxyRequest is a request forwarded to an upstream API without translation.
// The caller supplies the already-sanitized headers, including the upstream
// credential, because subscription credentials belong to the client.
type ProxyRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// Provider is the boundary between the proxy and an upstream API.
// Implementations live under internal/provider.
type Provider interface {
	Name() string
	ChatCompletions(ctx context.Context, req Request) (*http.Response, error)
	Responses(ctx context.Context, req Request) (*http.Response, error)
}

// ImagesProvider is implemented by providers that serve OpenAI-compatible
// image generation requests. The server exposes POST /v1/images/generations for
// these providers and reports a configuration error for the others.
type ImagesProvider interface {
	Images(ctx context.Context, req Request) (*http.Response, error)
}

// ModelLister is implemented by providers that can enumerate the models they
// serve. The server uses it to expand /v1/models for providers configured with
// passthrough models.
type ModelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// UsageProvider is implemented by providers that expose subscription usage
// information. The server uses it for the Codex usage passthrough endpoint.
type UsageProvider interface {
	Usage(ctx context.Context) (*http.Response, error)
}

// TransparentProxy is implemented by providers that forward client requests
// unchanged, such as the Claude subscription path. Unlike Provider, it does not
// interpret the request body or inject a credential.
type TransparentProxy interface {
	Proxy(ctx context.Context, req ProxyRequest) (*http.Response, error)
}

type ErrorKind int

const (
	Unavailable ErrorKind = iota
	Timeout
)

// Error classifies transport-level failures so the server can choose between a
// 502 and a 504.
type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider error (kind %d): %v", e.Kind, e.Err)
}

func (e *Error) Unwrap() error {
	return e.Err
}
