package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/proxy"
)

type pingArgs struct{}

func pingBackend() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "ping", Description: "ping"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})
	return s
}

// newDemoMux serves one backend over Streamable HTTP and/or legacy SSE.
func newDemoMux(streamable, sse bool) *http.ServeMux {
	srv := pingBackend()
	get := func(*http.Request) *mcp.Server { return srv }
	mux := http.NewServeMux()
	if streamable {
		mux.Handle("/servers/demo/mcp", mcp.NewStreamableHTTPHandler(get, nil))
	}
	if sse {
		mux.Handle("/servers/demo/sse", mcp.NewSSEHandler(get, nil))
	}
	return mux
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func listPing(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "ping" {
		t.Fatalf("tools = %+v, want [ping]", res.Tools)
	}
}

func TestDialHTTPStreamable(t *testing.T) {
	srv := serve(t, newDemoMux(true, true))
	cs, err := proxy.DialHTTP(context.Background(), srv.URL+"/servers/demo/mcp", "http", nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cs.Close()
	listPing(t, cs)
}

func TestDialHTTPSSE(t *testing.T) {
	srv := serve(t, newDemoMux(true, true))
	cs, err := proxy.DialHTTP(context.Background(), srv.URL+"/servers/demo/sse", "sse", nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cs.Close()
	listPing(t, cs)
}

func TestDialHTTPAutoFallsBackToSSE(t *testing.T) {
	// Only the SSE route is mounted; a Streamable POST to it fails, so "auto"
	// must fall back to SSE.
	srv := serve(t, newDemoMux(false, true))
	cs, err := proxy.DialHTTP(context.Background(), srv.URL+"/servers/demo/sse", "auto", nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cs.Close()
	listPing(t, cs)
}

func TestDialHTTPAutoPrefersStreamable(t *testing.T) {
	srv := serve(t, newDemoMux(true, false))
	cs, err := proxy.DialHTTP(context.Background(), srv.URL+"/servers/demo/mcp", "", nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cs.Close()
	listPing(t, cs)
}

func TestDialHTTPSendsHeaders(t *testing.T) {
	inner := newDemoMux(true, true)
	var seen int
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		seen++
		inner.ServeHTTP(w, r)
	})
	srv := serve(t, h)
	hdr := map[string]string{"Authorization": "Bearer t"}
	for _, tc := range []struct{ transport, path string }{
		{"http", "/servers/demo/mcp"},
		{"sse", "/servers/demo/sse"},
	} {
		cs, err := proxy.DialHTTP(context.Background(), srv.URL+tc.path, tc.transport, hdr, nil)
		if err != nil {
			t.Fatalf("%s dial: %v", tc.transport, err)
		}
		listPing(t, cs)
		_ = cs.Close()
	}
	if seen == 0 {
		t.Fatal("handler never saw authorized request")
	}
	if _, err := proxy.DialHTTP(context.Background(), srv.URL+"/servers/demo/mcp", "http", nil, nil); err == nil {
		t.Fatal("dial without header should fail")
	}
}

func TestDialHTTPUnknownTransport(t *testing.T) {
	if _, err := proxy.DialHTTP(context.Background(), "http://127.0.0.1:1/x", "carrier-pigeon", nil, nil); err == nil {
		t.Fatal("want error for unknown transport")
	}
}
