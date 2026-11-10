package codex_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/codex"
)

func makeJWT(t *testing.T, expires time.Time, accountID string) string {
	t.Helper()
	claims := map[string]any{
		"exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeAuthFile(t *testing.T, dir, accessToken string) {
	t.Helper()
	doc := map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh",
			"id_token":      accessToken,
			"account_id":    "account-from-token",
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
}

func TestProviderUsesCodexAuthFile(t *testing.T) {
	dir := t.TempDir()
	accessToken := makeJWT(t, time.Now().Add(time.Hour), "account-123")
	writeAuthFile(t, dir, accessToken)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/backend-api/codex/chat/completions" {
			t.Errorf("path = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "account-from-token" {
			t.Errorf("account header = %q", got)
		}
		if got := r.Header.Get("originator"); got != codex.DefaultOriginator {
			t.Errorf("originator = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	p, err := codex.New("codex", upstream.URL+"/backend-api/codex", "", dir, time.Second, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		Body: []byte(`{"model":"gpt-5","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestProviderResponsesUsesCodexResponsesPath(t *testing.T) {
	dir := t.TempDir()
	accessToken := makeJWT(t, time.Now().Add(time.Hour), "account-123")
	writeAuthFile(t, dir, accessToken)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/backend-api/codex/responses" {
			t.Errorf("path = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "account-from-token" {
			t.Errorf("account header = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer upstream.Close()

	p, err := codex.New("codex", upstream.URL+"/backend-api/codex", "", dir, time.Second, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.Responses(context.Background(), provider.Request{
		Body:   []byte(`{"model":"gpt-5","input":[],"stream":true}`),
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	resp.Body.Close()
}

func TestProviderStaticAPIKeyTakesPrecedence(t *testing.T) {
	staticKey := makeJWT(t, time.Now().Add(time.Hour), "static-account")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+staticKey {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "static-account" {
			t.Errorf("account header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	p, err := codex.New("codex", upstream.URL, staticKey, t.TempDir(), time.Second, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		Body: []byte(`{"model":"gpt-5","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	resp.Body.Close()
}
