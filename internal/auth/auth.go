package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/joe-p/model-proxy/internal/config"
)

// Authenticate validates the bearer token against the configured virtual keys.
// It returns the key's name for logging. When auth is disabled it succeeds with
// an empty name.
func Authenticate(r *http.Request, enabled bool, keys []config.VirtualKey) (string, bool) {
	if !enabled {
		return "", true
	}

	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", false
	}
	token := fields[1]
	if token == "" {
		return "", false
	}

	matched := ""
	for _, key := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(key.Key)) == 1 {
			matched = key.Name
		}
	}
	if matched == "" {
		return "", false
	}
	return matched, true
}
