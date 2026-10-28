package httpadapter_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/adapters/httpadapter"
	"github.com/kenmoini/mcp-machine/internal/proxy"
)

type pingArgs struct{}

// demoFacade returns a facade bound to a backend that exposes one tool, "ping".
func demoFacade(t *testing.T) *proxy.Facade {
	t.Helper()
	ctx := context.Background()
	backend := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
	mcp.AddTool(backend, &mcp.Tool{Name: "ping", Description: "ping"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := backend.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("backend connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "facade", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	f := proxy.New("demo", "1.0.0")
	if err := f.Bind(ctx, cs); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return f
}

// newServer mounts the demo facade under "demo" and serves it.
func newServer(t *testing.T, opts httpadapter.Options) *httptest.Server {
	t.Helper()
	f := demoFacade(t)
	mux := http.NewServeMux()
	lookup := func(name string) (*mcp.Server, bool) {
		if name == "demo" {
			return f.Server(), true
		}
		return nil, false
	}
	if err := httpadapter.Mount(mux, lookup, opts); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`

// rawPost sends an initialize request with extra headers and returns the response.
func rawPost(t *testing.T, url string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var evilHeaders = map[string]string{
	"Origin":         "http://evil.example",
	"Sec-Fetch-Site": "cross-site",
}

func TestStreamableListTools(t *testing.T) {
	srv := newServer(t, httpadapter.Options{})
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/servers/demo/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "ping" {
		t.Fatalf("tools = %v, want [ping]", names)
	}
}

func TestUnknownServerIs404(t *testing.T) {
	srv := newServer(t, httpadapter.Options{})
	code, body := rawPost(t, srv.URL+"/servers/nope/mcp", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if strings.TrimSpace(body) != `{"error":"unknown server"}` {
		t.Fatalf("body = %q", body)
	}
}

func TestCrossOriginDenied(t *testing.T) {
	srv := newServer(t, httpadapter.Options{})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/servers/demo/mcp", strings.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range evilHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	const want = `{"jsonrpc":"2.0","error":{"code":-32000,"message":"origin not allowed"}}`
	if strings.TrimSpace(string(b)) != want {
		t.Fatalf("body = %q, want %q", b, want)
	}
}

func TestAllowedOriginAccepted(t *testing.T) {
	srv := newServer(t, httpadapter.Options{AllowedOrigins: []string{"http://evil.example"}})
	code, body := rawPost(t, srv.URL+"/servers/demo/mcp", evilHeaders)
	if code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 200/202", code, body)
	}
}

func TestPublicURLOriginAccepted(t *testing.T) {
	srv := newServer(t, httpadapter.Options{PublicURL: "http://evil.example/some/path"})
	code, body := rawPost(t, srv.URL+"/servers/demo/mcp", evilHeaders)
	if code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 200/202", code, body)
	}
}

func TestNoOriginAccepted(t *testing.T) {
	srv := newServer(t, httpadapter.Options{})
	code, body := rawPost(t, srv.URL+"/servers/demo/mcp", nil)
	if code == http.StatusForbidden {
		t.Fatalf("status = 403 (%s), want not 403", body)
	}
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, body)
	}
}

func TestGetOnMcpIsRejected(t *testing.T) {
	srv := newServer(t, httpadapter.Options{})
	resp, err := http.Get(srv.URL + "/servers/demo/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode > 499 {
		t.Fatalf("status = %d, want 4xx", resp.StatusCode)
	}
	// Pinned to what the SDK answers for GET in stateless mode.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestNewOriginProtectionRejectsBadOrigin(t *testing.T) {
	if _, err := httpadapter.NewOriginProtection(httpadapter.Options{AllowedOrigins: []string{"not a url"}}); err == nil {
		t.Fatal("want error for malformed origin")
	}
}
