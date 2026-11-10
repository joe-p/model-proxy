package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
	upstream       string
	stream         bool
	errorCode      string
	errorDetail    string
	clientCanceled bool
	usage          *usage
}

type usage struct {
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	Cost             *float64 `json:"cost"`
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

// copyJSON forwards the body unchanged while optionally observing top-level
// usage metadata after the copy completes.
func copyJSON(dst io.Writer, src io.Reader, parseUsage bool) (*usage, error) {
	if !parseUsage {
		_, err := io.Copy(dst, src)
		return nil, err
	}

	var captured bytes.Buffer
	_, err := io.Copy(dst, io.TeeReader(src, &captured))
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal(captured.Bytes(), &envelope); err != nil {
		return nil, nil
	}
	return envelope.Usage, nil
}
