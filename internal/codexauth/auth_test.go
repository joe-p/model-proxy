package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func tokenWithExpiry(t *testing.T, expires time.Time, accountID string) string {
	t.Helper()
	claims := map[string]any{"exp": expires.Unix()}
	if accountID != "" {
		claims[accountClaimKey] = map[string]any{accountClaimID: accountID}
	}
	return makeJWT(t, claims)
}

func writeAuthFile(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
}

func readAuthFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode auth file: %v", err)
	}
	return doc
}

func TestNewUsesCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	a := New(Options{})
	if got := a.AuthFile(); got != filepath.Join(dir, AuthFileName) {
		t.Fatalf("auth file = %q", got)
	}
}

func TestNewExplicitDirOverridesCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	dir := t.TempDir()

	a := New(Options{Dir: dir})
	if got := a.AuthFile(); got != filepath.Join(dir, AuthFileName) {
		t.Fatalf("auth file = %q", got)
	}
}

func TestTokenReturnsValidAccessTokenAndDerivesAccountID(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	access := tokenWithExpiry(t, now.Add(time.Hour), "account-123")
	writeAuthFile(t, filepath.Join(dir, AuthFileName), map[string]any{
		"tokens": map[string]any{
			"access_token":  access,
			"refresh_token": "refresh",
			"id_token":      "",
		},
	})

	a := New(Options{Dir: dir, Now: func() time.Time { return now }})
	token, accountID, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != access {
		t.Fatalf("token = %q", token)
	}
	if accountID != "account-123" {
		t.Fatalf("account id = %q", accountID)
	}
}

func TestTokenRefreshesExpiredTokenAndWritesFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	oldAccess := tokenWithExpiry(t, now.Add(-time.Hour), "old-account")
	newAccess := tokenWithExpiry(t, now.Add(time.Hour), "new-account")
	newID := tokenWithExpiry(t, now.Add(time.Hour), "new-account")

	var requests atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode refresh body: %v", err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_id"] != DefaultClientID {
			t.Errorf("unexpected refresh body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newAccess,
			"refresh_token": "new-refresh",
			"id_token":      newID,
		})
	}))
	defer tokenServer.Close()

	authFile := filepath.Join(dir, AuthFileName)
	writeAuthFile(t, authFile, map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"access_token":  oldAccess,
			"refresh_token": "old-refresh",
			"id_token":      "",
		},
		"last_refresh": "2026-11-01T00:00:00Z",
		"extra":        "preserved",
	})

	a := New(Options{
		Dir:        dir,
		TokenURL:   tokenServer.URL,
		HTTPClient: tokenServer.Client(),
		Now:        func() time.Time { return now },
	})
	token, accountID, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != newAccess {
		t.Fatalf("token = %q", token)
	}
	if accountID != "new-account" {
		t.Fatalf("account id = %q", accountID)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("refresh requests = %d", got)
	}

	doc := readAuthFile(t, authFile)
	if doc["extra"] != "preserved" {
		t.Fatalf("unknown field was not preserved: %+v", doc)
	}
	if doc["last_refresh"] != now.UTC().Format(time.RFC3339) {
		t.Fatalf("last_refresh = %v", doc["last_refresh"])
	}
	tokens, ok := doc["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %+v", doc["tokens"])
	}
	if tokens["access_token"] != newAccess || tokens["refresh_token"] != "new-refresh" {
		t.Fatalf("refreshed tokens = %+v", tokens)
	}
	if tokens["account_id"] != "new-account" {
		t.Fatalf("account_id = %v", tokens["account_id"])
	}
}

func TestTokenAcceptsLiteLLMFlatFormat(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	access := tokenWithExpiry(t, now.Add(time.Hour), "flat-account")
	writeAuthFile(t, filepath.Join(dir, AuthFileName), map[string]any{
		"access_token":  access,
		"refresh_token": "refresh",
		"account_id":    "flat-account",
	})

	a := New(Options{Dir: dir, Now: func() time.Time { return now }})
	token, accountID, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != access || accountID != "flat-account" {
		t.Fatalf("token = %q, account = %q", token, accountID)
	}
}

func TestTokenMissingFile(t *testing.T) {
	a := New(Options{Dir: t.TempDir()})
	_, _, err := a.Token(context.Background())
	var notLoggedIn *NotLoggedInError
	if !errors.As(err, &notLoggedIn) {
		t.Fatalf("error = %v, want NotLoggedInError", err)
	}
	if !strings.Contains(err.Error(), "auth_dir") {
		t.Fatalf("error should mention auth_dir: %v", err)
	}
}

func TestTokenExpiredWithoutRefreshToken(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	writeAuthFile(t, filepath.Join(dir, AuthFileName), map[string]any{
		"tokens": map[string]any{
			"access_token": tokenWithExpiry(t, now.Add(-time.Hour), ""),
		},
	})

	a := New(Options{Dir: dir, Now: func() time.Time { return now }})
	_, _, err := a.Token(context.Background())
	var notLoggedIn *NotLoggedInError
	if !errors.As(err, &notLoggedIn) {
		t.Fatalf("error = %v, want NotLoggedInError", err)
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTokenRefreshFailure(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	writeAuthFile(t, filepath.Join(dir, AuthFileName), map[string]any{
		"tokens": map[string]any{
			"access_token":  tokenWithExpiry(t, now.Add(-time.Hour), ""),
			"refresh_token": "old-refresh",
		},
	})

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusUnauthorized)
	}))
	defer tokenServer.Close()

	a := New(Options{
		Dir:        dir,
		TokenURL:   tokenServer.URL,
		HTTPClient: tokenServer.Client(),
		Now:        func() time.Time { return now },
	})
	_, _, err := a.Token(context.Background())
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("error = %v, want RefreshError", err)
	}
	if refreshErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", refreshErr.StatusCode)
	}
}

func TestTokenConcurrentRefreshHappensOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	newAccess := tokenWithExpiry(t, now.Add(time.Hour), "account")
	writeAuthFile(t, filepath.Join(dir, AuthFileName), map[string]any{
		"tokens": map[string]any{
			"access_token":  tokenWithExpiry(t, now.Add(-time.Hour), ""),
			"refresh_token": "old-refresh",
		},
	})

	var requests atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newAccess,
			"refresh_token": "new-refresh",
			"id_token":      newAccess,
		})
	}))
	defer tokenServer.Close()

	a := New(Options{
		Dir:        dir,
		TokenURL:   tokenServer.URL,
		HTTPClient: tokenServer.Client(),
		Now:        func() time.Time { return now },
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, _, err := a.Token(context.Background())
			if err != nil {
				t.Errorf("Token: %v", err)
				return
			}
			if token != newAccess {
				t.Errorf("token = %q", token)
			}
		}()
	}
	wg.Wait()

	if got := requests.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
}
