package httpadapter

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const hostDenyBody = `{"jsonrpc":"2.0","error":{"code":-32000,"message":"host not allowed"}}`

// HostGuard returns middleware that refuses browser requests whose Host
// header names a host MCP Machine was not told about, which defeats DNS
// rebinding: a rebinding page's requests carry the attacker's host name in
// both Origin and Host, so origin checks alone let them through. Accepted
// are IP literals (with or without a port), localhost, the process's own host
// name, the host of opts.PublicURL and the hosts of opts.AllowedOrigins;
// ports are not compared. Everything else gets a 403 JSON error.
//
// DNS rebinding is a browser attack, and browsers send an Origin header or
// fetch metadata (Sec-Fetch-*) with their requests, so only requests that
// carry one of those are checked. Other clients (MCP SDKs, curl, the connect
// bridge, a sidecar using a container name) pass whatever their Host.
func HostGuard(opts Options) func(http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true}
	if h, err := os.Hostname(); err == nil && h != "" {
		allowed[strings.ToLower(h)] = true
	}
	add := func(raw string) {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			allowed[strings.ToLower(u.Hostname())] = true
		}
	}
	add(opts.PublicURL)
	for _, o := range opts.AllowedOrigins {
		add(o)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fromBrowser(r) && !hostAllowed(r.Host, allowed) {
				w.Header().Set("Content-Type", jsonContentType)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(hostDenyBody))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// fromBrowser reports whether r carries the headers browsers attach: Origin
// or any fetch-metadata header.
func fromBrowser(r *http.Request) bool {
	for _, h := range []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// hostAllowed reports whether the Host header value hostport names an IP
// literal or one of the allowed host names.
func hostAllowed(hostport string, allowed map[string]bool) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return allowed[strings.ToLower(host)]
}
