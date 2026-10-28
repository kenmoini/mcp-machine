package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/buildinfo"
)

func newClient(opts *mcp.ClientOptions) *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{Name: "mcp-machine", Version: buildinfo.Version}, opts)
}

// DialIO connects an MCP client session over a duplex byte stream, such as an
// attach stream to a container's stdio. The session owns rwc and closes it
// when the session closes.
func DialIO(ctx context.Context, rwc io.ReadWriteCloser, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	return newClient(opts).Connect(ctx, &mcp.IOTransport{Reader: rwc, Writer: rwc}, nil)
}

// headerRoundTripper adds fixed headers to every outgoing request.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// DialHTTP connects an MCP client session to a remote server over HTTP.
// transport is "http" (Streamable HTTP), "sse" (legacy HTTP+SSE) or "auto"
// (also the empty string): try Streamable HTTP first, then SSE. headers are
// added to every request.
func DialHTTP(ctx context.Context, url, transport string, headers map[string]string, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	hc := &http.Client{Transport: headerRoundTripper{base: http.DefaultTransport, headers: headers}}
	streamable := func() (*mcp.ClientSession, error) {
		return newClient(opts).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	}
	sse := func() (*mcp.ClientSession, error) {
		return newClient(opts).Connect(ctx, &mcp.SSEClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	}
	switch transport {
	case "http":
		return streamable()
	case "sse":
		return sse()
	case "auto", "":
		cs, err := streamable()
		if err == nil {
			return cs, nil
		}
		cs, err2 := sse()
		if err2 == nil {
			return cs, nil
		}
		return nil, errors.Join(fmt.Errorf("streamable http: %w", err), fmt.Errorf("sse: %w", err2))
	default:
		return nil, fmt.Errorf("unknown transport %q; want http, sse or auto", transport)
	}
}
