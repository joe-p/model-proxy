package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/joe-p/model-proxy/internal/openroutercompat"
)

// copySSE forwards SSE bytes, optionally normalizing usage metadata to the
// OpenRouter shape. When parsing is enabled it observes the terminal usage
// chunk without modifying it.
func copySSE(dst http.ResponseWriter, src io.Reader, parseUsage, normalize bool) (*usage, error) {
	reader := bufio.NewReader(src)
	var lastUsage *usage

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if normalize {
				if normalized, changed := normalizeSSELine(line); changed {
					line = normalized
				}
			}
			if parseUsage {
				if observed := usageFromSSELine(line); observed != nil {
					lastUsage = observed
				}
			}
			if _, writeErr := dst.Write(line); writeErr != nil {
				return lastUsage, writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return lastUsage, nil
			}
			return lastUsage, err
		}
	}
}

// normalizeSSELine rewrites an SSE data line's usage object to the OpenRouter
// shape. Lines that are not JSON data events, or that need no change, are
// returned unchanged.
func normalizeSSELine(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line, false
	}
	normalized, changed, err := openroutercompat.NormalizeChatCompletion(payload)
	if err != nil || !changed {
		return line, false
	}

	ending := line[len(bytes.TrimRight(line, "\r\n")):]
	out := make([]byte, 0, len("data: ")+len(normalized)+len(ending))
	out = append(out, "data: "...)
	out = append(out, normalized...)
	out = append(out, ending...)
	return out, true
}

func usageFromSSELine(line []byte) *usage {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return nil
	}
	var event struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil
	}
	return event.Usage
}
