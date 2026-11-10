//go:build integration

package openrouter_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/openrouter"
)

func TestLiveChatCompletion(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY is not set")
	}

	p, err := openrouter.New(
		"openrouter",
		"https://openrouter.ai/api/v1",
		apiKey,
		10*time.Second,
		120*time.Second,
		"",
		"model-proxy-integration-test",
	)
	if err != nil {
		t.Fatalf("openrouter.New: %v", err)
	}

	body := []byte(`{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"Reply with exactly: ok"}],"max_tokens":10}`)
	resp, err := p.ChatCompletions(context.Background(), provider.Request{
		UpstreamModel: "openai/gpt-4o-mini",
		Body:          body,
		RequestID:     "model-proxy-integration-test",
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}

	var decoded struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Choices) == 0 {
		t.Fatalf("no choices in response: %s", data)
	}
	if len(decoded.Usage) == 0 {
		t.Fatalf("no usage in response: %s", data)
	}
}
