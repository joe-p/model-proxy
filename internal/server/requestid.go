package server

import (
	"crypto/rand"
	"fmt"
	"net/http"
)

const requestIDHeader = "X-Request-ID"

func requestIDFrom(r *http.Request) string {
	if id := sanitizeRequestID(r.Header.Get(requestIDHeader)); id != "" {
		return id
	}
	return newRequestID()
}

func sanitizeRequestID(id string) string {
	if id == "" || len(id) > 128 {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return ""
		}
	}
	return id
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
