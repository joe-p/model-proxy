package codex_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestProviderBridgesChatCompletionsToResponses(t *testing.T) {
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
		if got := r.Header.Get("originator"); got != codex.DefaultOriginator {
			t.Errorf("originator = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != "responses=experimental" {
			t.Errorf("openai-beta = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("accept = %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatalf("request body: %v", err)
		}
		if string(fields["model"]) != `"gpt-5"` {
			t.Errorf("model = %s", fields["model"])
		}
		if string(fields["stream"]) != "true" {
			t.Errorf("stream = %s", fields["stream"])
		}
		if string(fields["store"]) != "false" {
			t.Errorf("store = %s", fields["store"])
		}
		if !strings.Contains(string(fields["input"]), `"text":"hi"`) {
			t.Errorf("input = %s", fields["input"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1700000000,"status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`))
	}))
	defer upstream.Close()

	p, err := codex.New("codex", upstream.URL+"/backend-api/codex", "", dir, "", time.Second, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		UpstreamModel: "gpt-5",
		Body:          []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}

	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if completion.Object != "chat.completion" {
		t.Errorf("object = %q", completion.Object)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "hello" {
		t.Errorf("choices = %+v", completion.Choices)
	}
	if completion.Usage.PromptTokens != 3 {
		t.Errorf("prompt tokens = %d", completion.Usage.PromptTokens)
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

	p, err := codex.New("codex", upstream.URL+"/backend-api/codex", "", dir, "", time.Second, time.Second)
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
		if got := r.URL.Path; got != "/responses" {
			t.Errorf("path = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+staticKey {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "static-account" {
			t.Errorf("account header = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n"))
	}))
	defer upstream.Close()

	p, err := codex.New("codex", upstream.URL, staticKey, t.TempDir(), "", time.Second, time.Second)
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
