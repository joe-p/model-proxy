package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/joe-p/model-proxy/internal/config"
)

func TestAuthenticateHeaderAcceptsBareAndBearerTokens(t *testing.T) {
	keys := []config.VirtualKey{{Name: "test", Key: "secret"}}

	for _, value := range []string{"secret", "Bearer secret", "bearer secret"} {
		req := httptest.NewRequest(http.MethodPost, "/claude_sub/v1/messages", nil)
		req.Header.Set("X-Model-Proxy-Key", value)
		key, ok := AuthenticateHeader(req, "X-Model-Proxy-Key", true, keys)
		if !ok || key == nil || key.Name != "test" {
			t.Fatalf("value %q did not authenticate", value)
		}
	}
}

func TestAuthenticateHeaderRejectsMissingOrWrongToken(t *testing.T) {
	keys := []config.VirtualKey{{Name: "test", Key: "secret"}}

	for _, value := range []string{"", "wrong", "Bearer wrong", "Basic secret"} {
		req := httptest.NewRequest(http.MethodPost, "/claude_sub/v1/messages", nil)
		if value != "" {
			req.Header.Set("X-Model-Proxy-Key", value)
		}
		if _, ok := AuthenticateHeader(req, "X-Model-Proxy-Key", true, keys); ok {
			t.Fatalf("value %q should not authenticate", value)
		}
	}
}

func TestAuthenticateHeaderIgnoresAuthorization(t *testing.T) {
	keys := []config.VirtualKey{{Name: "test", Key: "secret"}}
	req := httptest.NewRequest(http.MethodPost, "/claude_sub/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer secret")

	if _, ok := AuthenticateHeader(req, "X-Model-Proxy-Key", true, keys); ok {
		t.Fatal("Authorization must not authenticate the subscription path")
	}
}
