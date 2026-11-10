package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// copySSE forwards SSE bytes unchanged, flushing after each line. When parsing
// is enabled it observes the terminal usage chunk without modifying it.
func copySSE(dst http.ResponseWriter, src io.Reader, parseUsage bool) (*usage, error) {
	reader := bufio.NewReader(src)
	var lastUsage *usage

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
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
