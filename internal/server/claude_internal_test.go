package server

import "testing"

func TestClaudePricingModelID(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5-20250929":       "claude-sonnet-4-5",
		"claude-sonnet-4-5":                "claude-sonnet-4-5",
		"claude-sonnet-4-5[1m]":            "claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929[1m]":   "claude-sonnet-4-5",
		"claude-opus-4-5-20251101":         "claude-opus-4-5",
		"claude-haiku-4-5-20251001":        "claude-haiku-4-5",
		"some-model-without-a-date-suffix": "some-model-without-a-date-suffix",
	}
	for input, want := range cases {
		if got := claudePricingModelID(input); got != want {
			t.Errorf("claudePricingModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAnthropicUsageTotalsIncludeCacheTokens(t *testing.T) {
	input := int64(3)
	output := int64(20)
	cacheWrite := int64(100)
	cached := int64(50)

	got := (&anthropicUsage{
		InputTokens:              &input,
		OutputTokens:             &output,
		CacheCreationInputTokens: &cacheWrite,
		CacheReadInputTokens:     &cached,
	}).toUsage()

	if got.PromptTokens != 153 || got.CompletionTokens != 20 || got.TotalTokens != 173 {
		t.Fatalf("unexpected totals: %+v", got)
	}
	if got.PromptTokensDetails == nil ||
		got.PromptTokensDetails.CachedTokens == nil || *got.PromptTokensDetails.CachedTokens != 50 ||
		got.PromptTokensDetails.CacheWriteTokens == nil || *got.PromptTokensDetails.CacheWriteTokens != 100 {
		t.Fatalf("unexpected cache details: %+v", got.PromptTokensDetails)
	}
}
