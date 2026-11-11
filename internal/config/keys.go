package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/hujson"
)

// keyEntry is a virtual key read from the file named by key_file.
type keyEntry struct {
	name      string
	key       string
	keyEnv    string
	policy    string
	userAgent string
	location  string
}

// keyFileVirtualKey is the on-disk shape of an entry in key_file.
type keyFileVirtualKey struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	KeyEnv    string `json:"key_env"`
	Policy    string `json:"policy"`
	UserAgent string `json:"user_agent"`
}

// resolveVirtualKeys loads virtual keys from key_file. It returns the resolved
// keys and the absolute key file path, which is empty when no key file is
// configured.
func resolveVirtualKeys(keyFile, configDir string, dotenv map[string]string) ([]VirtualKey, string, error) {
	keyFile = strings.TrimSpace(keyFile)
	if keyFile == "" {
		return nil, "", nil
	}

	path := resolveConfigPath(keyFile, configDir)
	fileKeys, err := loadKeyFile(path)
	if err != nil {
		return nil, "", err
	}
	entries := make([]keyEntry, 0, len(fileKeys))
	for i, rawKey := range fileKeys {
		entries = append(entries, keyEntry{
			name:      rawKey.Name,
			key:       rawKey.Key,
			keyEnv:    rawKey.KeyEnv,
			policy:    rawKey.Policy,
			userAgent: rawKey.UserAgent,
			location:  fmt.Sprintf("key_file[%d]", i),
		})
	}
	keys, err := resolveKeyEntries(entries, dotenv)
	if err != nil {
		return nil, "", fmt.Errorf("key file %s: %w", path, err)
	}
	return keys, path, nil
}

func resolveKeyEntries(entries []keyEntry, dotenv map[string]string) ([]VirtualKey, error) {
	keys := make([]VirtualKey, 0, len(entries))
	keyNames := make(map[string]struct{}, len(entries))
	keyValues := make(map[string]string, len(entries))

	for _, entry := range entries {
		name := strings.TrimSpace(entry.name)
		keyEnv := strings.TrimSpace(entry.keyEnv)
		key := strings.TrimSpace(entry.key)
		userAgent := strings.TrimSpace(entry.userAgent)

		if name == "" {
			return nil, fmt.Errorf("%s.name must not be empty", entry.location)
		}
		if err := validateLogValue(userAgent, fmt.Sprintf("%s (%s).user_agent", entry.location, name)); err != nil {
			return nil, err
		}
		if _, exists := keyNames[name]; exists {
			return nil, fmt.Errorf("duplicate virtual key name %q", name)
		}
		if key != "" && keyEnv != "" {
			return nil, fmt.Errorf("%s (%s): key and key_env are mutually exclusive", entry.location, name)
		}
		if key == "" {
			if keyEnv == "" {
				return nil, fmt.Errorf("%s (%s): key_env must not be empty", entry.location, name)
			}
			var ok bool
			key, ok = lookupEnv(dotenv, keyEnv)
			if !ok {
				return nil, fmt.Errorf("virtual key %q: environment variable %s is not set", name, keyEnv)
			}
			if key == "" {
				return nil, fmt.Errorf("virtual key %q: environment variable %s is empty", name, keyEnv)
			}
		}
		if other, exists := keyValues[key]; exists {
			return nil, fmt.Errorf("virtual keys %q and %q resolve to the same value", other, name)
		}

		keyNames[name] = struct{}{}
		keyValues[key] = name
		keys = append(keys, VirtualKey{
			Name:      name,
			Key:       key,
			Policy:    strings.TrimSpace(entry.policy),
			UserAgent: userAgent,
		})
	}
	return keys, nil
}

// loadKeyFile reads a JWCC key file containing an array of key objects.
func loadKeyFile(path string) ([]keyFileVirtualKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	standard, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse key file %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(standard)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("key file %s is empty", path)
	}

	var keys []keyFileVirtualKey
	if err := decodeStrict(standard, &keys); err != nil {
		return nil, fmt.Errorf("parse key file %s: %w", path, err)
	}
	return keys, nil
}

func decodeStrict(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	return dec.Decode(target)
}

func resolveConfigPath(value, configDir string) string {
	value = expandHomePath(strings.TrimSpace(value))
	if !filepath.IsAbs(value) {
		value = filepath.Join(configDir, value)
	}
	return filepath.Clean(value)
}
