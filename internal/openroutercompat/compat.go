// Package openroutercompat normalizes provider usage metadata to the
// OpenRouter Chat Completions shape so OpenRouter-compatible clients can use
// any configured provider.
package openroutercompat

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// NormalizeChatCompletion rewrites the usage object of a Chat Completions
// response into OpenRouter's shape. The original bytes are returned unchanged
// when no fields need to be added.
func NormalizeChatCompletion(body []byte) ([]byte, bool, error) {
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
	if !NormalizeUsage(usage) {
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
