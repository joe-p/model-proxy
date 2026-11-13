package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/joe-p/model-proxy/internal/auth"
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
)

// handleUsage proxies ChatGPT's subscription usage endpoint through a Codex
// provider. The upstream payload is returned unchanged so clients such as
// pi-openai-usage can parse the same response they get from ChatGPT directly.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	if r.Method != http.MethodGet {
		writeAPIError(w, lr, errMethodNotAllowed(http.MethodGet))
		return
	}

	st := stateFrom(r.Context())
	key, ok := auth.Authenticate(r, st.cfg.Auth.Enabled, st.cfg.Keys)
	if !ok {
		writeAPIError(w, lr, errInvalidAPIKey())
		return
	}
	logKey(lr, st.cfg, key)

	providerName, resolveErr := resolveUsageProvider(st.cfg, r.URL.Query().Get("provider"))
	if resolveErr != nil {
		writeAPIError(w, lr, resolveErr)
		return
	}

	prov, exists := st.providers[providerName]
	if !exists {
		lr.errorDetail = fmt.Sprintf("provider %q is not available", providerName)
		writeAPIError(w, lr, errInternal())
		return
	}
	usageProvider, ok := prov.(provider.UsageProvider)
	if !ok {
		lr.errorDetail = fmt.Sprintf("provider %q does not expose usage", providerName)
		writeAPIError(w, lr, errInternal())
		return
	}

	lr.provider = providerName
	lr.billingMode = st.cfg.Providers[providerName].BillingMode

	resp, err := usageProvider.Usage(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		lr.errorDetail = err.Error()
		var providerErr *provider.Error
		if errors.As(err, &providerErr) && providerErr.Kind == provider.Timeout {
			writeAPIError(w, lr, errUpstreamTimeout())
		} else {
			writeAPIError(w, lr, errUpstreamUnavailable())
		}
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set(requestIDHeader, lr.requestID)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}

	if resp.StatusCode >= 400 {
		lr.errorCode = "upstream_error"
		if err := writeUpstreamError(w, lr, resp, providerName); err != nil && r.Context().Err() == nil {
			lr.errorDetail = err.Error()
		}
		if r.Context().Err() != nil {
			lr.clientCanceled = true
		}
		return
	}

	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		lr.errorDetail = err.Error()
	}
	if r.Context().Err() != nil {
		lr.clientCanceled = true
	}
}

// resolveUsageProvider chooses the Codex provider that serves a usage request.
// A single configured Codex provider is used automatically; when several are
// configured, the caller must disambiguate with ?provider=<name>.
func resolveUsageProvider(cfg *config.Config, requested string) (string, *apiError) {
	names := make([]string, 0, len(cfg.Providers))
	for name, providerCfg := range cfg.Providers {
		if isCodexProviderType(providerCfg) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	if requested = strings.TrimSpace(requested); requested != "" {
		providerCfg, exists := cfg.Providers[requested]
		if !exists || !isCodexProviderType(providerCfg) {
			return "", errInvalidRequest(fmt.Sprintf("Provider %q is not a codex provider", requested))
		}
		return requested, nil
	}

	switch len(names) {
	case 0:
		return "", errInvalidRequest("Usage is only available when a codex provider is configured")
	case 1:
		return names[0], nil
	default:
		return "", errInvalidRequest(fmt.Sprintf(
			"More than one codex provider is configured (%s); specify ?provider=<name>",
			strings.Join(names, ", "),
		))
	}
}

func isCodexProviderType(providerCfg config.ProviderConfig) bool {
	providerType := providerCfg.Type
	if providerType == "" {
		providerType = providerCfg.Name
	}
	return providerType == "codex" || providerType == "chatgpt"
}
