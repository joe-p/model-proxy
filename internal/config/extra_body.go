package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// reservedExtraBodyKeys are request fields the proxy owns or deliberately
// blocks. Allowing configuration to set them would bypass alias routing,
// streaming detection, or the client-facing field restrictions.
var reservedExtraBodyKeys = map[string]struct{}{
	"model":    {},
	"messages": {},
	"stream":   {},
	"models":   {},
	"route":    {},
	"plugins":  {},
}

// normalizeExtraBody converts a TOML-decoded extra_body table into raw JSON
// values and rejects fields that must remain under proxy/client control.
func normalizeExtraBody(value map[string]any, field string) (map[string]json.RawMessage, error) {
	if len(value) == 0 {
		return nil, nil
	}
	for key := range value {
		if _, reserved := reservedExtraBodyKeys[key]; reserved {
			return nil, fmt.Errorf("%s: field %q may not be set by the proxy", field, key)
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return out, nil
}

// MergeExtraBody returns a new map with override layered over base. Nested
// JSON objects are merged recursively; every other value in override replaces
// the base value. The inputs are not modified.
func MergeExtraBody(base, override map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(base) == 0 {
		return override, nil
	}
	if len(override) == 0 {
		return base, nil
	}
	return mergeObjects(base, override)
}

func mergeObjects(base, override map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, overrideValue := range override {
		baseValue, exists := out[key]
		if !exists || !isJSONObject(baseValue) || !isJSONObject(overrideValue) {
			out[key] = overrideValue
			continue
		}

		var baseObject, overrideObject map[string]json.RawMessage
		if err := json.Unmarshal(baseValue, &baseObject); err != nil {
			return nil, fmt.Errorf("extra body field %q: %w", key, err)
		}
		if err := json.Unmarshal(overrideValue, &overrideObject); err != nil {
			return nil, fmt.Errorf("extra body field %q: %w", key, err)
		}
		merged, err := mergeObjects(baseObject, overrideObject)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("extra body field %q: %w", key, err)
		}
		out[key] = data
	}
	return out, nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}
