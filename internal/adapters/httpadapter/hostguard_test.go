package httpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/adapters/httpadapter"
)

func guarded(opts httpadapter.Options) http.Handler {
	return httpadapter.HostGuard(opts)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

// browserPost is a POST to host carrying the given browser header (name,
// value), or none when hdr is empty.
func browserPost(host string, hdr ...string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/servers/demo/mcp", nil)
	req.Host = host
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return req
}

func TestHostGuard(t *testing.T) {
	h := guarded(httpadapter.Options{
		PublicURL:      "https://mcp.example.com",
		AllowedOrigins: []string{"http://tools.lan:8080"},
	})

	for host, want := range map[string]int{
		"127.0.0.1":            http.StatusNoContent,
		"127.0.0.1:8080":       http.StatusNoContent,
		"192.168.1.20:8080":    http.StatusNoContent,
		"[::1]:8080":           http.StatusNoContent,
		"[fe80::1]":            http.StatusNoContent,
		"localhost":            http.StatusNoContent,
		"LOCALHOST:8080":       http.StatusNoContent,
		"mcp.example.com":      http.StatusNoContent,
		"MCP.example.com:443":  http.StatusNoContent,
		"mcp.example.com:9999": http.StatusNoContent, // the port is not the attacker's lever
		"tools.lan:8080":       http.StatusNoContent,
		"tools.lan":            http.StatusNoContent,
		"evil.example":         http.StatusForbidden, // a rebinding page: Origin == Host
		"evil.example:8080":    http.StatusForbidden,
		"mcp.example.com.evil": http.StatusForbidden,
		"localhost.evil":       http.StatusForbidden,
		"":                     http.StatusForbidden,
	} {
		// Origin or Sec-Fetch-Site alone marks the request as a browser's.
		for _, hdr := range [][2]string{
			{"Sec-Fetch-Site", "same-origin"},
			{"Origin", "http://" + host},
		} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, browserPost(host, hdr[0], hdr[1]))
			if rec.Code != want {
				t.Errorf("Host %q with %s: status %d, want %d", host, hdr[0], rec.Code, want)
				continue
			}
			if want == http.StatusForbidden {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Host %q: Content-Type %q", host, ct)
				}
				if !strings.Contains(rec.Body.String(), "host not allowed") {
					t.Errorf("Host %q: body %q", host, rec.Body)
				}
			}
		}
	}
}

func TestHostGuardIgnoresNonBrowserClients(t *testing.T) {
	// MCP clients, curl, the connect bridge and sidecars using a container
	// name send neither Origin nor fetch metadata: Host is not checked.
	h := guarded(httpadapter.Options{})
	for _, host := range []string{"mcp-machine:8080", "mcp.lan", "evil.example"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, browserPost(host))
		if rec.Code != http.StatusNoContent {
			t.Errorf("non-browser POST to %q: status %d, want 204", host, rec.Code)
		}
	}
	// The same POST from a browser is refused.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, browserPost("mcp-machine:8080", "Sec-Fetch-Site", "same-origin"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("browser POST to an unlisted host: status %d, want 403", rec.Code)
	}
}

func TestHostGuardWithoutConfiguredNames(t *testing.T) {
	// No public_url: a browser reaching MCP Machine by a DNS name needs
	// public_url (or allowed_origins); IP literals and localhost pass.
	h := guarded(httpadapter.Options{})
	for host, want := range map[string]int{"10.0.0.5:8080": 204, "localhost:8080": 204, "mcp.lan:8080": 403} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, browserPost(host, "Sec-Fetch-Site", "same-origin"))
		if rec.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, rec.Code, want)
		}
	}
}

func TestHostGuardAcceptsOwnHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no host name: %v", err)
	}
	rec := httptest.NewRecorder()
	guarded(httpadapter.Options{}).ServeHTTP(rec, browserPost(host+":8080", "Sec-Fetch-Site", "same-origin"))
	if rec.Code != http.StatusNoContent {
		t.Errorf("browser POST to own host name %q: status %d, want 204", host, rec.Code)
	}
}

func TestMountGuardsHost(t *testing.T) {
	mux := newMux(t, httpadapter.Options{})
	for _, path := range []string{"/servers/demo/mcp", "/servers/demo/sse", "/servers/nope/mcp"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, path, strings.NewReader(initBody))
			req.Host = "rebind.evil.example:8080"
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403", method, path, rec.Code)
			}
		}
	}
	// Without browser headers the request reaches the route (here: the
	// unknown-server 404).
	req := httptest.NewRequest(http.MethodPost, "/servers/nope/mcp", strings.NewReader(initBody))
	req.Host = "mcp-machine:8080"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-browser POST: status %d, want 404", rec.Code)
	}
}

// Node's fetch (undici) adds "Sec-Fetch-Mode: cors" to every request without
// Origin or Sec-Fetch-Site; such clients (mcp-remote, TypeScript SDK clients,
// Node sidecars) must pass whatever Host they use.
func TestHostGuardIgnoresNodeFetchMetadata(t *testing.T) {
	h := guarded(httpadapter.Options{})
	for _, host := range []string{"mcp.lan:8080", "mcp-machine:8080", "evil.example"} {
		for _, hdr := range [][2]string{{"Sec-Fetch-Mode", "cors"}, {"Sec-Fetch-Dest", "empty"}} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, browserPost(host, hdr[0], hdr[1]))
			if rec.Code != http.StatusNoContent {
				t.Errorf("Host %q with only %s: status %d, want %d", host, hdr[0], rec.Code, http.StatusNoContent)
			}
		}
	}
}
