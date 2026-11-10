// Package openroutercompat normalizes provider usage metadata to the
// OpenRouter Chat Completions shape so OpenRouter-compatible clients can use
// any configured provider.
package openroutercompat

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Tokens are the counters read from a usage object.
type Tokens struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     *int64
	CacheWriteTokens *int64
}

// Cost is the OpenRouter cost block injected into a usage object.
type Cost struct {
	Total      float64
	Prompt     float64
	Completion float64
}

// CostFunc derives a cost block from observed tokens. Returning nil leaves the
// provider-reported usage untouched.
type CostFunc func(Tokens) *Cost

// NormalizeChatCompletion rewrites the usage object of a Chat Completions
// response into OpenRouter's shape. When cost is non-nil and the provider did
// not report a cost, the derived cost block is injected as well. The original
// bytes are returned unchanged when no fields need to be added.
func NormalizeChatCompletion(body []byte, cost CostFunc) ([]byte, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return body, false, nil
	}

	rawUsage, ok := envelope["usage"]
	if !ok {
		return body, false, nil
	}
	usage, ok := objectField(rawUsage)
	if !ok {
		return body, false, nil
	}
	changed := NormalizeUsage(usage)
	if cost != nil {
		if injected := injectCost(usage, cost(tokensFromUsage(usage))); injected {
			changed = true
		}
	}
	if !changed {
		return body, false, nil
	}

	normalizedUsage, err := json.Marshal(usage)
	if err != nil {
		return body, false, err
	}
	envelope["usage"] = normalizedUsage
	normalized, err := json.Marshal(envelope)
	if err != nil {
		return body, false, err
	}
	return normalized, true, nil
}

// NormalizeUsage adds the OpenRouter usage fields that can be derived from
// another provider's representation. It never invents token counts or costs
// that the provider did not report.
func NormalizeUsage(usage map[string]json.RawMessage) bool {
	changed := false

	promptDetails, _ := objectField(usage["prompt_tokens_details"])
	promptDetailsChanged := false
	if promptDetails == nil {
		promptDetails = make(map[string]json.RawMessage)
	}
	if !present(promptDetails, "cached_tokens") {
		if value, ok := firstPresent(usage, "cache_read_input_tokens", "prompt_cache_hit_tokens"); ok {
			promptDetails["cached_tokens"] = value
			promptDetailsChanged = true
		}
	}
	if !present(promptDetails, "cache_write_tokens") {
		if value, ok := firstPresent(usage, "cache_creation_input_tokens"); ok {
			promptDetails["cache_write_tokens"] = value
			promptDetailsChanged = true
		}
	}
	if promptDetailsChanged {
		encoded, err := json.Marshal(promptDetails)
		if err == nil {
			usage["prompt_tokens_details"] = encoded
			changed = true
		}
	}

	completionDetails, _ := objectField(usage["completion_tokens_details"])
	completionDetailsChanged := false
	if completionDetails == nil {
		completionDetails = make(map[string]json.RawMessage)
	}
	if !present(completionDetails, "reasoning_tokens") {
		if value, ok := firstPresent(usage, "reasoning_tokens"); ok {
			completionDetails["reasoning_tokens"] = value
			completionDetailsChanged = true
		}
	}
	if completionDetailsChanged {
		encoded, err := json.Marshal(completionDetails)
		if err == nil {
			usage["completion_tokens_details"] = encoded
			changed = true
		}
	}

	if !present(usage, "total_tokens") {
		prompt, promptOK := integer(usage["prompt_tokens"])
		completion, completionOK := integer(usage["completion_tokens"])
		if promptOK && completionOK {
			usage["total_tokens"] = json.RawMessage(strconv.FormatInt(prompt+completion, 10))
			changed = true
		}
	}

	return changed
}

// tokensFromUsage reads the token counters NormalizeUsage works with.
func tokensFromUsage(usage map[string]json.RawMessage) Tokens {
	tokens := Tokens{}
	tokens.PromptTokens, _ = integerOrZero(usage["prompt_tokens"])
	tokens.CompletionTokens, _ = integerOrZero(usage["completion_tokens"])
	if details, ok := objectField(usage["prompt_tokens_details"]); ok {
		if value, ok := integerOrZero(details["cached_tokens"]); ok {
			tokens.CachedTokens = &value
		}
		if value, ok := integerOrZero(details["cache_write_tokens"]); ok {
			tokens.CacheWriteTokens = &value
		}
	}
	return tokens
}

// injectCost writes OpenRouter's cost fields. A provider-reported cost always
// wins, and a nil cost or an empty token count is left alone.
func injectCost(usage map[string]json.RawMessage, cost *Cost) bool {
	if cost == nil || present(usage, "cost") {
		return false
	}
	if cost.Total == 0 && cost.Prompt == 0 && cost.Completion == 0 {
		return false
	}

	costJSON, err := marshalNumber(cost.Total)
	if err != nil {
		return false
	}
	costDetails, err := json.Marshal(struct {
		UpstreamInferenceCost            float64 `json:"upstream_inference_cost"`
		UpstreamInferencePromptCost      float64 `json:"upstream_inference_prompt_cost"`
		UpstreamInferenceCompletionsCost float64 `json:"upstream_inference_completions_cost"`
	}{
		UpstreamInferenceCost:            cost.Total,
		UpstreamInferencePromptCost:      cost.Prompt,
		UpstreamInferenceCompletionsCost: cost.Completion,
	})
	if err != nil {
		return false
	}

	usage["cost"] = costJSON
	usage["is_byok"] = json.RawMessage("false")
	usage["cost_details"] = costDetails
	return true
}

func marshalNumber(value float64) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

// NormalizeError rewrites an upstream error body into OpenRouter's error
// envelope. providerName and status are exposed as metadata. Bodies that cannot
// be parsed fall back to their text as the message.
func NormalizeError(body []byte, providerName string, status int) []byte {
	message := errorMessage(body)
	envelope := map[string]any{
		"error": map[string]any{
			"message": message,
			"code":    status,
			"metadata": map[string]any{
				"provider_name":   providerName,
				"upstream_status": status,
			},
		},
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return encoded
}

// errorMessage finds the most useful message in an upstream error body.
func errorMessage(body []byte) string {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err == nil {
		if nested, ok := objectField(payload["error"]); ok {
			for _, key := range []string{"message", "detail", "error"} {
				if message, ok := stringField(nested, key); ok {
					return message
				}
			}
		}
		for _, key := range []string{"detail", "message", "error"} {
			if message, ok := stringField(payload, key); ok {
				return message
			}
		}
	}

	const maxMessage = 1024
	text := bytes.TrimSpace(body)
	if len(text) == 0 {
		return "Upstream request failed"
	}
	if len(text) > maxMessage {
		text = text[:maxMessage]
	}
	return string(text)
}

func stringField(object map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := object[key]
	if !ok || isNull(raw) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func integerOrZero(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	return integer(raw)
}

func objectField(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func present(object map[string]json.RawMessage, key string) bool {
	value, ok := object[key]
	return ok && !isNull(value)
}

func firstPresent(object map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if value, ok := object[key]; ok && !isNull(value) {
			return value, true
		}
	}
	return nil, false
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func integer(raw json.RawMessage) (int64, bool) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	if value, err := number.Int64(); err == nil {
		return value, true
	}
	value, err := number.Float64()
	if err != nil {
		return 0, false
	}
	return int64(value), true
}
