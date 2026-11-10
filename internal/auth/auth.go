package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/joe-p/model-proxy/internal/config"
)

// Authenticate validates the bearer token against the configured virtual keys.
// It returns the matched key so callers can inspect its policy. When auth is
// disabled it succeeds with a nil key.
func Authenticate(r *http.Request, enabled bool, keys []config.VirtualKey) (*config.VirtualKey, bool) {
	if !enabled {
		return nil, true
	}

	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return nil, false
	}
	token := fields[1]
	if token == "" {
		return nil, false
	}

	var matched *config.VirtualKey
	for i := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(keys[i].Key)) == 1 {
			matched = &keys[i]
		}
	}
	if matched == nil {
		return nil, false
	}
	return matched, true
}
