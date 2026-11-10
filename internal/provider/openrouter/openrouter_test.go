package openrouter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/openrouter"
)

func TestChatCompletionsSendsAttributionHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/chat/completions" {
			t.Errorf("path = %q", got)
		}
		if got := r.Header.Get("HTTP-Referer"); got != "https://example.test" {
			t.Errorf("referer = %q", got)
		}
		if got := r.Header.Get("X-Title"); got != "model-proxy-test" {
			t.Errorf("title = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	p, err := openrouter.New(
		"openrouter",
		upstream.URL,
		"key",
		time.Second,
		time.Second,
		"https://example.test",
		"model-proxy-test",
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		Body: []byte(`{"model":"m","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	resp.Body.Close()
}
