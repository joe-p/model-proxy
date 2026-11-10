package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/joe-p/model-proxy/internal/openroutercompat"
)

type requestLog struct {
	requestID      string
	method         string
	path           string
	remoteAddr     string
	userAgent      string
	keyName        string
	policyName     string
	policyMode     string
	policyModels   []string
	alias          string
	provider       string
	billingMode    string
	upstream       string
	stream         bool
	errorCode      string
	errorDetail    string
	clientCanceled bool
	usage          *usage
	estimatedCost  *float64
	pricingSource  string
}

type usage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	Cost                    *float64                 `json:"cost"`
	CacheDiscount           *float64                 `json:"cache_discount"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details"`
}

// promptTokensDetails carries the cache breakdown of prompt tokens. Pointers
// distinguish "provider did not report this" from a reported zero (a miss).
type promptTokensDetails struct {
	CachedTokens     *int64 `json:"cached_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

// completionTokensDetails carries the reasoning-token breakdown of completion
// tokens.
type completionTokensDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyResponseHeaders(dst, src http.Header) {
	connectionTokens := make(map[string]struct{})
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			connectionTokens[http.CanonicalHeaderKey(strings.TrimSpace(token))] = struct{}{}
		}
	}

	requestIDCanonical := http.CanonicalHeaderKey(requestIDHeader)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := hopByHopHeaders[canonical]; ok {
			continue
		}
		if _, ok := connectionTokens[canonical]; ok {
			continue
		}
		if canonical == "Content-Length" || canonical == "Set-Cookie" || canonical == requestIDCanonical {
			continue
		}
		if strings.HasPrefix(canonical, "X-Model-Proxy-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// copyJSON forwards the body, optionally normalizing usage metadata to the
// OpenRouter shape and observing it after the copy completes.
func copyJSON(dst io.Writer, src io.Reader, parseUsage, normalize bool) (*usage, error) {
	if !parseUsage && !normalize {
		_, err := io.Copy(dst, src)
		return nil, err
	}

	body, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if normalize {
		normalized, changed, normalizeErr := openroutercompat.NormalizeChatCompletion(body)
		if normalizeErr == nil && changed {
			body = normalized
		}
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	if !parseUsage {
		return nil, nil
	}

	var envelope struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil
	}
	return envelope.Usage, nil
}
