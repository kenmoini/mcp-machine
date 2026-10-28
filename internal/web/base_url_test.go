package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBaseURL(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/servers/a", nil)
	plain.Host = "mcp.lan:8080"
	secure := httptest.NewRequest(http.MethodGet, "/servers/a", nil)
	secure.Host = "mcp.example.com"
	secure.TLS = &tls.ConnectionState{}
	// Proxy headers are not trusted: only public_url overrides the request.
	forwarded := httptest.NewRequest(http.MethodGet, "/servers/a", nil)
	forwarded.Host = "internal:8080"
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	forwarded.Header.Set("X-Forwarded-Host", "evil.example")

	for _, tc := range []struct {
		name      string
		publicURL string
		req       *http.Request
		want      string
	}{
		{"request host over http", "", plain, "http://mcp.lan:8080"},
		{"request host over tls", "", secure, "https://mcp.example.com"},
		{"forwarded headers ignored", "", forwarded, "http://internal:8080"},
		{"public url wins", "https://mcp.example.org", plain, "https://mcp.example.org"},
		{"public url trailing slash trimmed", "https://mcp.example.org/base/", plain, "https://mcp.example.org/base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &ui{Deps: Deps{PublicURL: tc.publicURL}}
			if got := u.baseURL(tc.req); got != tc.want {
				t.Fatalf("baseURL = %q, want %q", got, tc.want)
			}
		})
	}
}
