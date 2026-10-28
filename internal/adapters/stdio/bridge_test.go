package stdio

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/adapters/httpadapter"
	"github.com/kenmoini/mcp-machine/internal/proxy"
)

type pingArgs struct{}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// newBackend returns a connected client session to an in-memory server that
// exposes one tool, "ping".
func newBackend(t *testing.T) *mcp.ClientSession {
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
	return cs
}

// newUpstream serves a facade over Streamable HTTP at /servers/demo/mcp.
func newUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	f := proxy.New("demo", "1.0.0")
	if err := f.Bind(context.Background(), newBackend(t)); err != nil {
		t.Fatalf("bind: %v", err)
	}
	mux := http.NewServeMux()
	lookup := func(name string) (*mcp.Server, bool) {
		if name == "demo" {
			return f.Server(), true
		}
		return nil, false
	}
	if err := httpadapter.Mount(mux, lookup, httpadapter.Options{}); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func toolNames(ctx context.Context, t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func connectClient(ctx context.Context, t *testing.T, rwc io.ReadWriteCloser) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: rwc, Writer: rwc}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return cs
}

func waitDone(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatal("bridge did not return in time")
		return nil
	}
}

// TestBridgeListTools exercises the full path over a real HTTP listener.
func TestBridgeListTools(t *testing.T) {
	srv := newUpstream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv.URL+"/servers/demo/mcp", a, a, quiet) }()

	cs := connectClient(ctx, t, b)
	defer cs.Close()
	if got := toolNames(ctx, t, cs); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("tools = %v, want [ping]", got)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ping"})
	if err != nil || res.IsError {
		t.Fatalf("call ping: %v %+v", err, res)
	}
	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

// TestBridgeStopsOnContextCancel needs a listener for the upstream side.
func TestBridgeStopsOnContextCancel(t *testing.T) {
	srv := newUpstream(t)
	ctx, cancel := context.WithCancel(context.Background())
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv.URL+"/servers/demo/mcp", a, a, quiet) }()

	cs := connectClient(ctx, t, b)
	defer cs.Close()
	_ = toolNames(ctx, t, cs)

	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestRunUnreachableUpstreamFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := Run(ctx, "http://127.0.0.1:1/servers/demo/mcp", a, a, quiet); err == nil {
		t.Fatal("want error for unreachable upstream")
	}
}

// The tests below drive the bridge core against an in-memory upstream.

func TestRunListTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- run(ctx, newBackend(t), a, a, quiet) }()

	cs := connectClient(ctx, t, b)
	if got := toolNames(ctx, t, cs); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("tools = %v, want [ping]", got)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ping"})
	if err != nil || res.IsError {
		t.Fatalf("call ping: %v %+v", err, res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "pong" {
		t.Fatalf("content = %+v", res.Content)
	}
	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- run(ctx, newBackend(t), a, a, quiet) }()

	cs := connectClient(ctx, t, b)
	defer cs.Close()
	_ = toolNames(ctx, t, cs)

	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}

func TestRunStopsWhenStdioCloses(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), newBackend(t), a, a, quiet) }()

	cs := connectClient(context.Background(), t, b)
	_ = toolNames(context.Background(), t, cs)
	_ = cs.Close() // closes b: the bridge's stdin reaches EOF

	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("run = %v, want nil on clean stdio close", err)
	}
}

func TestRunStopsWhenUpstreamCloses(t *testing.T) {
	up := newBackend(t)
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), up, a, a, quiet) }()

	cs := connectClient(context.Background(), t, b)
	defer cs.Close()
	_ = toolNames(context.Background(), t, cs)

	_ = up.Close()
	if err := waitDone(t, done, 3*time.Second); err == nil {
		t.Fatal("run = nil, want error when upstream session is lost")
	}
}

// A stdin whose Close cannot unblock a pending Read (os.Stdin behind a
// NopCloser) must not keep the bridge from returning on cancel.
func TestRunStopsOnCancelWithUncloseableStdin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- run(ctx, newBackend(t), io.NopCloser(a), a, quiet) }()

	cs := connectClient(ctx, t, b)
	defer cs.Close()
	_ = toolNames(ctx, t, cs)

	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}
