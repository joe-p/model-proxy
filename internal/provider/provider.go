package provider

import (
	"context"
	"fmt"
	"net/http"
)

// Request is the provider-neutral input for a chat completion call.
type Request struct {
	UpstreamModel string
	Body          []byte
	Stream        bool
	Accept        string
	RequestID     string
	Query         string
}

// Provider is the boundary between the proxy and an upstream API.
// Implementations live under internal/provider.
type Provider interface {
	Name() string
	ChatCompletions(ctx context.Context, req Request) (*http.Response, error)
	Responses(ctx context.Context, req Request) (*http.Response, error)
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
