// Package codexauth reads and refreshes the OAuth credentials written by the
// OpenAI Codex CLI. It is the Go equivalent of LiteLLM's ChatGPT
// Authenticator: tokens are read from <auth_dir>/auth.json, expired access
// tokens are refreshed with the stored refresh token, and the refreshed tokens
// are written back to the same file.
package codexauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDirName is the Codex CLI directory, relative to the user's home.
	DefaultDirName = ".codex"
	// AuthFileName is the Codex CLI credential file.
	AuthFileName = "auth.json"
	// DefaultClientID is the OAuth client used by Codex and LiteLLM.
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// DefaultTokenURL is the OpenAI OAuth token endpoint.
	DefaultTokenURL = "https://auth.openai.com/oauth/token"

	refreshTimeout    = 30 * time.Second
	expirySkew        = 60 * time.Second
	maxResponseBytes  = 1 << 20
	maxErrorBodyBytes = 4 << 10
	accountClaimKey   = "https://api.openai.com/auth"
	accountClaimID    = "chatgpt_account_id"
)

// Options configures an Authenticator. Zero values use Codex CLI defaults,
// including $CODEX_HOME and ~/.codex for the credential directory.
type Options struct {
	// Dir is the directory containing auth.json. Ignored when File is set.
	Dir string
	// File is the explicit path to auth.json.
	File string
	// HTTPClient is used for token refresh. A default client is used when nil.
	HTTPClient *http.Client
	// TokenURL overrides the OAuth token endpoint.
	TokenURL string
	// ClientID overrides the OAuth client ID.
	ClientID string
	// Now overrides the clock, primarily for tests.
	Now func() time.Time
}

// Authenticator resolves a current Codex access token from auth.json.
type Authenticator struct {
	authFile string
	tokenURL string
	clientID string
	client   *http.Client
	now      func() time.Time
}

// New returns an Authenticator for the configured credential location. The file
// does not need to exist yet; requests fail with a NotLoggedInError until the
// user signs in.
func New(opts Options) *Authenticator {
	authFile := opts.File
	if authFile == "" {
		authFile = filepath.Join(authDir(opts.Dir), AuthFileName)
	}

	tokenURL := opts.TokenURL
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
	}
	clientID := opts.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: refreshTimeout}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Authenticator{
		authFile: expandHome(authFile),
		tokenURL: tokenURL,
		clientID: clientID,
		client:   client,
		now:      now,
	}
}

// AuthFile returns the resolved auth.json path.
func (a *Authenticator) AuthFile() string {
	return a.authFile
}

// Token returns a valid access token and the ChatGPT account ID associated
// with it. It refreshes the token when the access token is expired.
func (a *Authenticator) Token(ctx context.Context) (string, string, error) {
	lock := lockFor(a.authFile)
	lock.Lock()
	defer lock.Unlock()

	doc, err := a.read()
	if err != nil {
		return "", "", err
	}

	now := a.now()
	if doc.tokens.accessToken != "" && !tokenExpired(doc.tokens.accessToken, now) {
		return doc.tokens.accessToken, doc.tokens.accountID, nil
	}
	if doc.tokens.refreshToken == "" {
		return "", "", &NotLoggedInError{
			Path:   a.authFile,
			Reason: "access token is expired and no refresh token is present",
		}
	}

	refreshed, err := a.refresh(ctx, doc.tokens.refreshToken)
	if err != nil {
		return "", "", err
	}
	if refreshed.accountID == "" {
		refreshed.accountID = doc.tokens.accountID
	}
	if err := a.write(doc, refreshed); err != nil {
		return "", "", err
	}
	return refreshed.accessToken, refreshed.accountID, nil
}

// AccessToken is a convenience wrapper around Token.
func (a *Authenticator) AccessToken(ctx context.Context) (string, error) {
	token, _, err := a.Token(ctx)
	return token, err
}

// AccountID returns the ChatGPT account ID embedded in a token, if present.
func AccountID(token string) string {
	return accountIDFromToken(token)
}

// NotLoggedInError reports that no usable Codex credentials are available.
type NotLoggedInError struct {
	Path   string
	Reason string
}

func (e *NotLoggedInError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "no access token"
	}
	return fmt.Sprintf(
		"Codex is not logged in (%s): sign in with the Codex CLI or point the provider's auth_dir at a directory containing %s (looked in %s)",
		reason, AuthFileName, e.Path,
	)
}

// RefreshError reports a failed OAuth token refresh.
type RefreshError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *RefreshError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("refresh Codex access token: status %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("refresh Codex access token: %s", e.Message)
}

func (e *RefreshError) Unwrap() error {
	return e.Err
}

type tokenSet struct {
	accessToken  string
	refreshToken string
	idToken      string
	accountID    string
}

type document struct {
	raw      map[string]json.RawMessage
	tokenRaw map[string]json.RawMessage
	nested   bool
	tokens   tokenSet
}

func (a *Authenticator) read() (*document, error) {
	data, err := os.ReadFile(a.authFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &NotLoggedInError{Path: a.authFile}
		}
		return nil, fmt.Errorf("read Codex auth file %s: %w", a.authFile, err)
	}

	doc, err := parseDocument(data)
	if err != nil {
		return nil, fmt.Errorf("parse Codex auth file %s: %w", a.authFile, err)
	}
	return doc, nil
}

func parseDocument(data []byte) (*document, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("credential file must contain a JSON object")
	}

	doc := &document{raw: raw, tokenRaw: make(map[string]json.RawMessage), nested: true}
	if tokensRaw, ok := raw["tokens"]; ok {
		if err := json.Unmarshal(tokensRaw, &doc.tokenRaw); err != nil {
			return nil, fmt.Errorf("tokens: %w", err)
		}
		if doc.tokenRaw == nil {
			doc.tokenRaw = make(map[string]json.RawMessage)
		}
	} else {
		// Accept LiteLLM's flat auth.json as a fallback.
		doc.nested = false
		doc.tokenRaw = raw
	}

	doc.tokens = tokenSet{
		accessToken:  stringField(doc.tokenRaw, "access_token"),
		refreshToken: stringField(doc.tokenRaw, "refresh_token"),
		idToken:      stringField(doc.tokenRaw, "id_token"),
		accountID:    stringField(doc.tokenRaw, "account_id"),
	}
	if doc.tokens.accountID == "" {
		doc.tokens.accountID = firstNonEmpty(
			accountIDFromToken(doc.tokens.idToken),
			accountIDFromToken(doc.tokens.accessToken),
		)
	}
	return doc, nil
}

func (a *Authenticator) write(doc *document, tokens tokenSet) error {
	doc.tokens = tokens
	setString(doc.tokenRaw, "access_token", tokens.accessToken)
	setString(doc.tokenRaw, "refresh_token", tokens.refreshToken)
	setString(doc.tokenRaw, "id_token", tokens.idToken)
	setString(doc.tokenRaw, "account_id", tokens.accountID)

	if doc.nested {
		encoded, err := json.Marshal(doc.tokenRaw)
		if err != nil {
			return fmt.Errorf("encode Codex auth tokens: %w", err)
		}
		doc.raw["tokens"] = encoded
	}
	doc.raw["last_refresh"] = json.RawMessage(strconv.Quote(a.now().UTC().Format(time.RFC3339)))

	encoded, err := json.MarshalIndent(doc.raw, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Codex auth file: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeFileAtomic(a.authFile, encoded, 0o600); err != nil {
		return fmt.Errorf("write Codex auth file %s: %w", a.authFile, err)
	}
	return nil
}

func (a *Authenticator) refresh(ctx context.Context, refreshToken string) (tokenSet, error) {
	payload, err := json.Marshal(map[string]string{
		"client_id":     a.clientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"scope":         "openid profile email",
	})
	if err != nil {
		return tokenSet{}, &RefreshError{Message: err.Error(), Err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return tokenSet{}, &RefreshError{Message: err.Error(), Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return tokenSet{}, &RefreshError{Message: err.Error(), Err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return tokenSet{}, &RefreshError{Message: err.Error(), Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenSet{}, &RefreshError{
			StatusCode: resp.StatusCode,
			Message:    truncate(strings.TrimSpace(string(body)), maxErrorBodyBytes),
		}
	}

	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return tokenSet{}, &RefreshError{Message: "decode response: " + err.Error(), Err: err}
	}
	if decoded.AccessToken == "" {
		return tokenSet{}, &RefreshError{Message: "response did not include an access token"}
	}
	if decoded.RefreshToken == "" {
		decoded.RefreshToken = refreshToken
	}

	accountID := firstNonEmpty(accountIDFromToken(decoded.IDToken), accountIDFromToken(decoded.AccessToken))
	return tokenSet{
		accessToken:  decoded.AccessToken,
		refreshToken: decoded.RefreshToken,
		idToken:      decoded.IDToken,
		accountID:    accountID,
	}, nil
}

func authDir(configured string) string {
	if configured != "" {
		return expandHome(configured)
	}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return expandHome(codexHome)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return DefaultDirName
	}
	return filepath.Join(home, DefaultDirName)
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func stringField(values map[string]json.RawMessage, name string) string {
	raw, ok := values[name]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func setString(values map[string]json.RawMessage, name, value string) {
	if value == "" {
		delete(values, name)
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	values[name] = encoded
}

func tokenExpired(token string, now time.Time) bool {
	claims := jwtClaims(token)
	exp, ok := claims["exp"].(float64)
	if !ok {
		return true
	}
	return !now.Add(expirySkew).Before(time.Unix(int64(exp), 0))
}

func accountIDFromToken(token string) string {
	if token == "" {
		return ""
	}
	authClaims, ok := jwtClaims(token)[accountClaimKey].(map[string]any)
	if !ok {
		return ""
	}
	accountID, _ := authClaims[accountClaimID].(string)
	return accountID
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	file, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return err
	}
	tmpName := file.Name()
	defer os.Remove(tmpName)

	if err := file.Chmod(perm); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

var fileLocks sync.Map

func lockFor(path string) *sync.Mutex {
	value, _ := fileLocks.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}
