package openaicompat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/provider/openaicompat"
)

func newProvider(t *testing.T, opts openaicompat.Options) *openaicompat.Provider {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "test"
	}
	if opts.APIKey == "" {
		opts.APIKey = "upstream-key"
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = time.Second
	}
	if opts.ResponseHeaderTimeout == 0 {
		opts.ResponseHeaderTimeout = time.Second
	}
	p, err := openaicompat.New(opts)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func TestModelsParsesOpenAIShape(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"openai/gpt-4o-mini"},{"id":"anthropic/claude-sonnet-4"},{"id":"openai/gpt-4o-mini"}]}`))
	}))
	defer upstream.Close()

	p := newProvider(t, openaicompat.Options{BaseURL: upstream.URL + "/v1"})
	ids, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer upstream-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	want := []string{"openai/gpt-4o-mini", "anthropic/claude-sonnet-4"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestModelsParsesCodexShapeWithQuery(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.6-terra"},{"slug":""}]}`))
	}))
	defer upstream.Close()

	p := newProvider(t, openaicompat.Options{
		BaseURL:     upstream.URL + "/backend-api/codex",
		ModelsQuery: "client_version=0.167.0",
		Headers:     map[string]string{"originator": "codex_cli_rs"},
	})
	ids, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotPath != "/backend-api/codex/models?client_version=0.167.0" {
		t.Fatalf("path = %q", gotPath)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestModelsReportsUpstreamStatusAndBadBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
		}
	}))
	defer upstream.Close()

	unauthorized := newProvider(t, openaicompat.Options{BaseURL: upstream.URL + "/denied"})
	if _, err := unauthorized.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected status error, got %v", err)
	}

	bad := newProvider(t, openaicompat.Options{BaseURL: upstream.URL + "/bad"})
	if _, err := bad.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestModelsUsesTokenSourceCredentials(t *testing.T) {
	var gotAuth, gotAccount string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"}]}`))
	}))
	defer upstream.Close()

	p := newProvider(t, openaicompat.Options{
		BaseURL:     upstream.URL,
		TokenSource: staticToken{token: "access-token", account: "account-id"},
	})
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotAuth != "Bearer access-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotAccount != "account-id" {
		t.Fatalf("account header = %q", gotAccount)
	}
}

type staticToken struct{ token, account string }

func (s staticToken) Token(context.Context) (string, string, error) {
	return s.token, s.account, nil
}
