package server

import (
	"strings"
	"testing"
)

func TestUpstreamErrorDetail(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"envelope": {
			body: `{"error":{"message":"Provider returned error","code":400}}`,
			want: "Provider returned error",
		},
		"collapses whitespace": {
			body: `{"detail":"rate limited\n  try  later"}`,
			want: "rate limited try later",
		},
		"plain text": {body: "bad gateway", want: "bad gateway"},
		"empty":      {body: "", want: "Upstream request failed"},
	}
	for name, tc := range cases {
		if got := upstreamErrorDetail([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: upstreamErrorDetail = %q, want %q", name, got, tc.want)
		}
	}
}

func TestUpstreamErrorDetailClipsLongBodies(t *testing.T) {
	body := strings.Repeat("x", upstreamErrorDetailLimit*2)
	got := upstreamErrorDetail([]byte(body))
	if len(got) != upstreamErrorDetailLimit+len("...") {
		t.Fatalf("detail length = %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("detail = %q", got)
	}
}
