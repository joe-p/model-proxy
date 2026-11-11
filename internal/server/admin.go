package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// adminTokenHeader carries the admin credential for clients that already use
// Authorization for something else. It is deliberately absent from the CORS
// allow-list: admin routes are not meant to be reachable from a browser.
const adminTokenHeader = "X-Model-Proxy-Admin"

// ErrReloadRequiresRestart marks a reload that was rejected because the change
// touches state bound to the running process rather than to a request snapshot.
var ErrReloadRequiresRestart = errors.New("configuration change requires a restart")

// restartRequiredError reports a specific setting that cannot be changed by
// reloading, while still matching ErrReloadRequiresRestart for classification.
type restartRequiredError struct{ msg string }

func (e *restartRequiredError) Error() string { return e.msg }

func (e *restartRequiredError) Is(target error) bool { return target == ErrReloadRequiresRestart }

func errRestartRequired(setting string) error {
	return &restartRequiredError{msg: setting + " changes require a restart"}
}

// ReloadResult describes a completed reload attempt.
type ReloadResult struct {
	// Changed reports whether the configuration file differed from the last
	// applied version. A false value means the reload was a no-op.
	Changed    bool   `json:"changed"`
	ConfigHash string `json:"config_hash,omitempty"`
	Models     int    `json:"models"`
	Providers  int    `json:"providers"`
	Keys       int    `json:"keys"`
	Policies   int    `json:"policies"`
	AccessSink bool   `json:"access_sink"`
}

// Reloader re-reads the configuration file and applies it atomically. It is
// implemented by the process entrypoint, which owns the resources a reload
// replaces (log sink and pricing refresh loop) in addition to the server state.
//
// A failed reload must leave the running configuration untouched. A rejection
// caused by a change that cannot be applied without a restart is reported by
// wrapping ErrReloadRequiresRestart.
type Reloader interface {
	ReloadConfig(ctx context.Context, force bool) (ReloadResult, error)
}

// WithReloader enables the administrative API for servers built by New.
func WithReloader(r Reloader) Option {
	return func(st *state) { st.reloader = r }
}

// reloadRecord tracks reload bookkeeping for GET /admin/status.
type reloadRecord struct {
	mu         sync.Mutex
	generation uint64
	at         time.Time
	lastError  string
	hash       string
}

func (s *Server) recordReload(res ReloadResult, err error) {
	s.reloadInfo.mu.Lock()
	defer s.reloadInfo.mu.Unlock()
	if err != nil {
		s.reloadInfo.lastError = err.Error()
		return
	}
	if res.Changed {
		s.reloadInfo.generation++
		s.reloadInfo.at = time.Now()
		s.reloadInfo.hash = res.ConfigHash
	}
	s.reloadInfo.lastError = ""
}

func (s *Server) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	st := stateFrom(r.Context())

	if r.Method != http.MethodPost {
		writeAPIError(w, lr, errMethodNotAllowed(http.MethodPost))
		return
	}
	if e, ok := s.authorizeAdmin(r, st); !ok {
		writeAPIError(w, lr, e)
		return
	}
	if st.reloader == nil {
		// No reloader was wired up: the endpoint does not exist.
		writeAPIError(w, lr, errNotFound())
		return
	}
	// The reload endpoint only ever re-reads the configured file. A request body
	// is rejected rather than ignored so that a client cannot believe it applied
	// an inline configuration.
	if requestHasBody(r) {
		writeAPIError(w, lr, &apiError{
			Status:  http.StatusBadRequest,
			Type:    "invalid_request_error",
			Code:    "reload_body_not_allowed",
			Message: "Reload requests must not include a body; the configured file is re-read",
		})
		return
	}

	res, err := st.reloader.ReloadConfig(r.Context(), wantsForce(r))
	s.recordReload(res, err)
	if err != nil {
		writeAPIError(w, lr, reloadError(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
		ReloadResult
		Generation uint64 `json:"generation"`
	}{
		Status:       "ok",
		ReloadResult: res,
		Generation:   s.generation(),
	})
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	st := stateFrom(r.Context())

	if r.Method != http.MethodGet {
		writeAPIError(w, lr, errMethodNotAllowed(http.MethodGet))
		return
	}
	if e, ok := s.authorizeAdmin(r, st); !ok {
		writeAPIError(w, lr, e)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.adminStatus(st)); err != nil {
		lr.errorDetail = err.Error()
	}
}

// authorizeAdmin validates the admin credential. When no token is configured
// the route reports 404 instead of 401 so an unconfigured server does not
// advertise an administrative surface at all.
func (s *Server) authorizeAdmin(r *http.Request, st *state) (*apiError, bool) {
	token := st.cfg.Server.AdminToken
	if token == "" {
		return errNotFound(), false
	}
	provided := adminCredential(r)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
		return errInvalidAPIKey(), false
	}
	return nil, true
}

func adminCredential(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get(adminTokenHeader)); value != "" {
		return value
	}
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		return fields[1]
	}
	return ""
}

func wantsForce(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// requestHasBody reports whether the request carries a payload without reading
// an unbounded amount of data from the client.
func requestHasBody(r *http.Request) bool {
	if r.ContentLength > 0 {
		return true
	}
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}
	var probe [1]byte
	n, _ := io.ReadFull(r.Body, probe[:])
	return n > 0
}

func reloadError(err error) *apiError {
	if errors.Is(err, ErrReloadRequiresRestart) {
		return &apiError{
			Status:  http.StatusConflict,
			Type:    "invalid_request_error",
			Code:    "reload_requires_restart",
			Message: err.Error(),
		}
	}
	return &apiError{
		Status:  http.StatusUnprocessableEntity,
		Type:    "invalid_request_error",
		Code:    "reload_failed",
		Message: err.Error(),
	}
}

func (s *Server) generation() uint64 {
	s.reloadInfo.mu.Lock()
	defer s.reloadInfo.mu.Unlock()
	return s.reloadInfo.generation
}

type adminStatus struct {
	Status             string              `json:"status"`
	UptimeSeconds      float64             `json:"uptime_seconds"`
	ConfigPath         string              `json:"config_path,omitempty"`
	EnvFile            string              `json:"env_file,omitempty"`
	KeyFile            string              `json:"key_file,omitempty"`
	Listen             string              `json:"listen"`
	AdminListen        string              `json:"admin_listen,omitempty"`
	ReloadAvailable    bool                `json:"reload_available"`
	Generation         uint64              `json:"generation"`
	ConfigHash         string              `json:"config_hash,omitempty"`
	LastReloadAt       *time.Time          `json:"last_reload_at,omitempty"`
	LastReloadError    string              `json:"last_reload_error,omitempty"`
	AuthEnabled        bool                `json:"auth_enabled"`
	PricingEnabled     bool                `json:"pricing_enabled"`
	ClaudeSubscription bool                `json:"claude_subscription"`
	Models             []adminStatusModel  `json:"models"`
	Providers          []adminStatusSource `json:"providers"`
	Keys               []adminStatusKey    `json:"virtual_keys"`
	Policies           []adminStatusPolicy `json:"policies"`
}

type adminStatusModel struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Upstream string `json:"upstream"`
}

type adminStatusSource struct {
	Name              string `json:"name"`
	Type              string `json:"type"`
	BaseURL           string `json:"base_url"`
	BillingMode       string `json:"billing_mode,omitempty"`
	PassthroughModels bool   `json:"passthrough_models"`
}

// adminStatusKey intentionally omits the key value: secrets never leave memory.
type adminStatusKey struct {
	Name   string `json:"name"`
	Policy string `json:"policy,omitempty"`
}

type adminStatusPolicy struct {
	Name   string   `json:"name"`
	Mode   string   `json:"mode"`
	Models []string `json:"models"`
}

// adminStatus reports the resolved, non-secret configuration of the running
// process. It answers "did my reload take effect?" without exposing credentials.
func (s *Server) adminStatus(st *state) adminStatus {
	cfg := st.cfg

	status := adminStatus{
		Models:             make([]adminStatusModel, 0, len(cfg.Models)),
		Providers:          make([]adminStatusSource, 0, len(cfg.Providers)),
		Keys:               make([]adminStatusKey, 0, len(cfg.Keys)),
		Policies:           make([]adminStatusPolicy, 0, len(cfg.Policies)),
		Status:             "ok",
		UptimeSeconds:      time.Since(s.startedAt).Seconds(),
		ConfigPath:         cfg.SourcePath,
		EnvFile:            cfg.EnvFile,
		KeyFile:            cfg.KeyFile,
		Listen:             cfg.Server.Listen,
		AdminListen:        cfg.Server.AdminListen,
		ReloadAvailable:    st.reloader != nil,
		AuthEnabled:        cfg.Auth.Enabled,
		PricingEnabled:     cfg.Pricing.Enabled(),
		ClaudeSubscription: cfg.EnableClaudeSubscription,
	}

	s.reloadInfo.mu.Lock()
	status.Generation = s.reloadInfo.generation
	status.ConfigHash = s.reloadInfo.hash
	status.LastReloadError = s.reloadInfo.lastError
	if !s.reloadInfo.at.IsZero() {
		at := s.reloadInfo.at
		status.LastReloadAt = &at
	}
	s.reloadInfo.mu.Unlock()

	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		model := cfg.Models[alias]
		status.Models = append(status.Models, adminStatusModel{
			Alias:    alias,
			Provider: model.Provider,
			Upstream: model.Upstream,
		})
	}

	providerNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	for _, name := range providerNames {
		p := cfg.Providers[name]
		status.Providers = append(status.Providers, adminStatusSource{
			Name:              p.Name,
			Type:              p.Type,
			BaseURL:           p.BaseURL,
			BillingMode:       p.BillingMode,
			PassthroughModels: p.PassthroughModels,
		})
	}

	for _, key := range cfg.Keys {
		status.Keys = append(status.Keys, adminStatusKey{Name: key.Name, Policy: key.Policy})
	}

	policyNames := make([]string, 0, len(cfg.Policies))
	for name := range cfg.Policies {
		policyNames = append(policyNames, name)
	}
	sort.Strings(policyNames)
	for _, name := range policyNames {
		policy := cfg.Policies[name]
		models := make([]string, 0, len(policy.Models))
		for alias := range policy.Models {
			models = append(models, alias)
		}
		sort.Strings(models)
		status.Policies = append(status.Policies, adminStatusPolicy{Name: policy.Name, Mode: policy.Mode, Models: models})
	}

	return status
}

// AdminHandler serves the administrative API. It is mounted on its own
// listener so the reload endpoint never appears on the public port, and it does
// not emit permissive CORS headers.
func (s *Server) AdminHandler() http.Handler { return s.adminHandler }
