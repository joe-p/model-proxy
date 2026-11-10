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
	return matchToken(fields[1], keys)
}

// AuthenticateHeader validates a virtual key supplied in a non-Authorization
// header. The value may be a bare token or "Bearer <token>". The Claude
// subscription path uses this so Authorization remains the upstream OAuth
// credential.
func AuthenticateHeader(r *http.Request, header string, enabled bool, keys []config.VirtualKey) (*config.VirtualKey, bool) {
	if !enabled {
		return nil, true
	}

	value := strings.TrimSpace(r.Header.Get(header))
	if value == "" {
		return nil, false
	}
	if fields := strings.Fields(value); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		value = fields[1]
	}
	if value == "" {
		return nil, false
	}
	return matchToken(value, keys)
}

func matchToken(token string, keys []config.VirtualKey) (*config.VirtualKey, bool) {
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
