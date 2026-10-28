// Package httpadapter serves proxied MCP servers over HTTP.
package httpadapter

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Lookup resolves a server name to the mcp.Server that serves it.
type Lookup func(name string) (*mcp.Server, bool)

// Options configures the HTTP adapter.
type Options struct {
	// AllowedOrigins are extra trusted origins ("scheme://host[:port]") for
	// cross-origin requests.
	AllowedOrigins []string
	// PublicURL is the externally visible base URL; its origin is trusted.
	PublicURL string
	Logger    *slog.Logger
}

const (
	denyBody        = `{"jsonrpc":"2.0","error":{"code":-32000,"message":"origin not allowed"}}`
	unknownBody     = `{"error":"unknown server"}`
	jsonContentType = "application/json"
)

// NewOriginProtection builds the cross-origin protection applied to every
// route: AllowedOrigins and the PublicURL origin are trusted, everything else
// that is cross-origin is answered with a 403 JSON-RPC error.
func NewOriginProtection(opts Options) (*http.CrossOriginProtection, error) {
	p := http.NewCrossOriginProtection()
	for _, o := range opts.AllowedOrigins {
		if err := p.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("allowed origin %q: %w", o, err)
		}
	}
	if opts.PublicURL != "" {
		u, err := url.Parse(opts.PublicURL)
		if err != nil {
			return nil, fmt.Errorf("public url %q: %w", opts.PublicURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("public url %q: missing scheme or host", opts.PublicURL)
		}
		origin := u.Scheme + "://" + u.Host
		if err := p.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("public url origin %q: %w", origin, err)
		}
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonContentType)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(denyBody))
	}))
	return p, nil
}

// Mount registers the streamable HTTP route "/servers/{name}/mcp" and the
// legacy HTTP+SSE route "/servers/{name}/sse" on mux. Both share the same
// lookup, host guard (see HostGuard) and origin protection.
func Mount(mux *http.ServeMux, lookup Lookup, opts Options) error {
	protection, err := NewOriginProtection(opts)
	if err != nil {
		return err
	}
	guard := HostGuard(opts)

	getServer := func(r *http.Request) *mcp.Server {
		s, ok := lookup(r.PathValue("name"))
		if !ok {
			return nil
		}
		return s
	}
	// Stateless is required to serve 2026-07-28 clients.
	streamable := protection.Handler(mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{
		Stateless: true,
		Logger:    opts.Logger,
	}))

	// The SSE handler serves both verbs: GET opens the event stream and
	// announces the POST endpoint, POST carries client messages.
	sse := protection.Handler(mcp.NewSSEHandler(getServer, &mcp.SSEOptions{}))

	mux.Handle("/servers/{name}/mcp", guard(withKnownServer(lookup, streamable)))
	mux.Handle("/servers/{name}/sse", guard(withKnownServer(lookup, sse)))
	return nil
}

// withKnownServer answers 404 for names lookup does not know.
func withKnownServer(lookup Lookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := lookup(r.PathValue("name")); !ok {
			w.Header().Set("Content-Type", jsonContentType)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(unknownBody))
			return
		}
		next.ServeHTTP(w, r)
	})
}
