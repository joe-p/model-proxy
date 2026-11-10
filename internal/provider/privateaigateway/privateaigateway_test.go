package privateaigateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/privateaigateway"
)

func TestChatCompletionsUsesGatewayEndpointAndHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "foo=bar" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer phala-key" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept = %q", got)
		}
		if got := r.Header.Get("X-Request-ID"); got != "request-1" {
			t.Errorf("request id = %q", got)
		}
		// Private AI Gateway does not use OpenRouter attribution headers.
		if got := r.Header.Get("HTTP-Referer"); got != "" {
			t.Errorf("unexpected referer = %q", got)
		}
		if got := r.Header.Get("X-Title"); got != "" {
			t.Errorf("unexpected title = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"z-ai/glm-5.3","messages":[]}` {
			t.Errorf("body = %s", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Receipt-Id", "receipt-1")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	p, err := privateaigateway.New(
		"phala",
		upstream.URL+"/v1",
		"phala-key",
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "phala" {
		t.Fatalf("name = %q", p.Name())
	}

	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		Body:      []byte(`{"model":"z-ai/glm-5.3","messages":[]}`),
		RequestID: "request-1",
		Query:     "foo=bar",
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Receipt-Id"); got != "receipt-1" {
		t.Fatalf("receipt header = %q", got)
	}
}

func TestChatCompletionsDefaultsAcceptForStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("accept = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:"))
	}))
	defer upstream.Close()

	p, err := privateaigateway.New("phala", upstream.URL+"/v1", "key", time.Second, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		Body:   []byte(`{"model":"m","messages":[],"stream":true}`),
		Stream: true,
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	resp.Body.Close()
}
