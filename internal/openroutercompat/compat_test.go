package openroutercompat_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/joe-p/model-proxy/internal/openroutercompat"
)

func TestNormalizeUsageMapsAlternateProviderFields(t *testing.T) {
	usage := map[string]json.RawMessage{
		"prompt_tokens":               json.RawMessage(`10`),
		"completion_tokens":           json.RawMessage(`5`),
		"cache_read_input_tokens":     json.RawMessage(`4`),
		"cache_creation_input_tokens": json.RawMessage(`2`),
		"reasoning_tokens":            json.RawMessage(`3`),
	}

	if !openroutercompat.NormalizeUsage(usage) {
		t.Fatal("expected usage to change")
	}

	var total int64
	if err := json.Unmarshal(usage["total_tokens"], &total); err != nil || total != 15 {
		t.Fatalf("total_tokens = %d, err = %v", total, err)
	}

	var promptDetails struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	}
	if err := json.Unmarshal(usage["prompt_tokens_details"], &promptDetails); err != nil {
		t.Fatalf("decode prompt_tokens_details: %v", err)
	}
	if promptDetails.CachedTokens == nil || *promptDetails.CachedTokens != 4 {
		t.Fatalf("cached_tokens = %v", promptDetails.CachedTokens)
	}
	if promptDetails.CacheWriteTokens == nil || *promptDetails.CacheWriteTokens != 2 {
		t.Fatalf("cache_write_tokens = %v", promptDetails.CacheWriteTokens)
	}

	var completionDetails struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	}
	if err := json.Unmarshal(usage["completion_tokens_details"], &completionDetails); err != nil {
		t.Fatalf("decode completion_tokens_details: %v", err)
	}
	if completionDetails.ReasoningTokens == nil || *completionDetails.ReasoningTokens != 3 {
		t.Fatalf("reasoning_tokens = %v", completionDetails.ReasoningTokens)
	}
}

func TestNormalizeUsageMapsPromptCacheHitTokens(t *testing.T) {
	usage := map[string]json.RawMessage{
		"prompt_tokens":           json.RawMessage(`10`),
		"completion_tokens":       json.RawMessage(`5`),
		"total_tokens":            json.RawMessage(`15`),
		"prompt_cache_hit_tokens": json.RawMessage(`6`),
	}

	if !openroutercompat.NormalizeUsage(usage) {
		t.Fatal("expected usage to change")
	}
	var details struct {
		CachedTokens *int64 `json:"cached_tokens"`
	}
	if err := json.Unmarshal(usage["prompt_tokens_details"], &details); err != nil {
		t.Fatalf("decode prompt_tokens_details: %v", err)
	}
	if details.CachedTokens == nil || *details.CachedTokens != 6 {
		t.Fatalf("cached_tokens = %v", details.CachedTokens)
	}
}

func TestNormalizeUsageDoesNotOverrideExistingFields(t *testing.T) {
	usage := map[string]json.RawMessage{
		"prompt_tokens":             json.RawMessage(`10`),
		"completion_tokens":         json.RawMessage(`5`),
		"total_tokens":              json.RawMessage(`15`),
		"cache_read_input_tokens":   json.RawMessage(`4`),
		"prompt_tokens_details":     json.RawMessage(`{"cached_tokens":9}`),
		"completion_tokens_details": json.RawMessage(`{"reasoning_tokens":8}`),
		"reasoning_tokens":          json.RawMessage(`3`),
	}

	if openroutercompat.NormalizeUsage(usage) {
		t.Fatal("expected usage to be unchanged")
	}
}

func TestNormalizeUsageLeavesOpenRouterShapeAlone(t *testing.T) {
	usage := map[string]json.RawMessage{
		"prompt_tokens":     json.RawMessage(`10`),
		"completion_tokens": json.RawMessage(`5`),
		"total_tokens":      json.RawMessage(`15`),
		"cost":              json.RawMessage(`0.1`),
		"prompt_tokens_details": json.RawMessage(
			`{"cached_tokens":4,"cache_write_tokens":2}`,
		),
		"completion_tokens_details": json.RawMessage(`{"reasoning_tokens":3}`),
	}

	if openroutercompat.NormalizeUsage(usage) {
		t.Fatal("expected OpenRouter-shaped usage to be unchanged")
	}
}

func TestNormalizeUsageSkipsNullAlternateFields(t *testing.T) {
	usage := map[string]json.RawMessage{
		"prompt_tokens":               json.RawMessage(`10`),
		"completion_tokens":           json.RawMessage(`5`),
		"total_tokens":                json.RawMessage(`15`),
		"cache_read_input_tokens":     json.RawMessage(`null`),
		"cache_creation_input_tokens": json.RawMessage(`null`),
	}

	if openroutercompat.NormalizeUsage(usage) {
		t.Fatal("expected null alternate fields to be ignored")
	}
}

func TestNormalizeChatCompletionReturnsOriginalWhenUnchanged(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	normalized, changed, err := openroutercompat.NormalizeChatCompletion(body, nil)
	if err != nil {
		t.Fatalf("NormalizeChatCompletion: %v", err)
	}
	if changed {
		t.Fatal("expected no change")
	}
	if string(normalized) != string(body) {
		t.Fatalf("body changed: %s", normalized)
	}
}

func TestNormalizeChatCompletionRewritesOnlyWhenNeeded(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"cache_read_input_tokens":1}}`)
	normalized, changed, err := openroutercompat.NormalizeChatCompletion(body, nil)
	if err != nil {
		t.Fatalf("NormalizeChatCompletion: %v", err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	var envelope struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(normalized, &envelope); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	var total int64
	if err := json.Unmarshal(envelope.Usage["total_tokens"], &total); err != nil || total != 3 {
		t.Fatalf("total_tokens = %d, err = %v", total, err)
	}
}

func TestNormalizeChatCompletionIgnoresInvalidOrMissingUsage(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`not json`),
		[]byte(`{"choices":[]}`),
		[]byte(`{"usage":null}`),
		[]byte(`{"usage":"nope"}`),
	} {
		normalized, changed, err := openroutercompat.NormalizeChatCompletion(body, nil)
		if err != nil {
			t.Fatalf("NormalizeChatCompletion(%s): %v", body, err)
		}
		if changed || string(normalized) != string(body) {
			t.Fatalf("body %s was changed", body)
		}
	}
}

func TestNormalizeChatCompletionInjectsCost(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
	normalized, changed, err := openroutercompat.NormalizeChatCompletion(body, func(tokens openroutercompat.Tokens) *openroutercompat.Cost {
		if tokens.PromptTokens != 1000 || tokens.CompletionTokens != 500 {
			t.Errorf("tokens = %+v", tokens)
		}
		return &openroutercompat.Cost{Total: 0.02, Prompt: 0.004, Completion: 0.016}
	})
	if err != nil {
		t.Fatalf("NormalizeChatCompletion: %v", err)
	}
	if !changed {
		t.Fatal("expected change")
	}

	var envelope struct {
		Usage struct {
			Cost        *float64 `json:"cost"`
			IsByok      *bool    `json:"is_byok"`
			CostDetails struct {
				Total      float64 `json:"upstream_inference_cost"`
				Prompt     float64 `json:"upstream_inference_prompt_cost"`
				Completion float64 `json:"upstream_inference_completions_cost"`
			} `json:"cost_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(normalized, &envelope); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	if envelope.Usage.Cost == nil || *envelope.Usage.Cost != 0.02 {
		t.Fatalf("cost = %v", envelope.Usage.Cost)
	}
	if envelope.Usage.IsByok == nil || *envelope.Usage.IsByok {
		t.Fatalf("is_byok = %v", envelope.Usage.IsByok)
	}
	if envelope.Usage.CostDetails.Prompt != 0.004 || envelope.Usage.CostDetails.Completion != 0.016 {
		t.Fatalf("cost details = %+v", envelope.Usage.CostDetails)
	}
}

func TestNormalizeChatCompletionKeepsReportedCost(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":0.5}}`)
	normalized, _, err := openroutercompat.NormalizeChatCompletion(body, func(openroutercompat.Tokens) *openroutercompat.Cost {
		return &openroutercompat.Cost{Total: 9, Prompt: 4, Completion: 5}
	})
	if err != nil {
		t.Fatalf("NormalizeChatCompletion: %v", err)
	}
	if !bytes.Contains(normalized, []byte(`"cost":0.5`)) {
		t.Fatalf("reported cost was overwritten: %s", normalized)
	}
	if bytes.Contains(normalized, []byte("cost_details")) {
		t.Fatalf("reported cost should not gain cost_details: %s", normalized)
	}
}

func TestNormalizeChatCompletionSkipsNilAndEmptyCost(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	for name, cost := range map[string]openroutercompat.CostFunc{
		"nil result": func(openroutercompat.Tokens) *openroutercompat.Cost { return nil },
		"zero cost":  func(openroutercompat.Tokens) *openroutercompat.Cost { return &openroutercompat.Cost{} },
	} {
		normalized, changed, err := openroutercompat.NormalizeChatCompletion(body, cost)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if changed || string(normalized) != string(body) {
			t.Fatalf("%s: body changed to %s", name, normalized)
		}
	}
}

func TestNormalizeError(t *testing.T) {
	cases := map[string]struct {
		body    string
		message string
	}{
		"openai shape": {`{"error":{"message":"rate limited","type":"rate_limit"}}`, "rate limited"},
		"detail shape": {`{"detail":"Not Found"}`, "Not Found"},
		"plain text":   {`bad gateway`, "bad gateway"},
		"empty":        {``, "Upstream request failed"},
	}
	for name, tc := range cases {
		normalized := openroutercompat.NormalizeError([]byte(tc.body), "codex", 502)
		var envelope struct {
			Error struct {
				Message  string         `json:"message"`
				Code     int            `json:"code"`
				Metadata map[string]any `json:"metadata"`
			} `json:"error"`
		}
		if err := json.Unmarshal(normalized, &envelope); err != nil {
			t.Fatalf("%s: decode %s: %v", name, normalized, err)
		}
		if envelope.Error.Message != tc.message {
			t.Fatalf("%s: message = %q, want %q", name, envelope.Error.Message, tc.message)
		}
		if envelope.Error.Code != 502 {
			t.Fatalf("%s: code = %d", name, envelope.Error.Code)
		}
		if envelope.Error.Metadata["provider_name"] != "codex" {
			t.Fatalf("%s: metadata = %+v", name, envelope.Error.Metadata)
		}
	}
}
