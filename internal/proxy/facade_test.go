package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/proxy"
)

type echoArgs struct {
	S string `json:"s"`
}

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

func newBackend(t *testing.T, opts *mcp.ServerOptions) *mcp.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, opts)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo s"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.S}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "add", Description: "add a+b"},
		func(_ context.Context, _ *mcp.CallToolRequest, in addArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprint(in.A + in.B)}}}, nil, nil
		})
	s.AddResource(&mcp.Resource{URI: "file:///a", Name: "a", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "A"}}}, nil
		})
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///{p}", Name: "files"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "T:" + req.Params.URI}}}, nil
		})
	s.AddPrompt(&mcp.Prompt{Name: "greet", Arguments: []*mcp.PromptArgument{{Name: "who"}}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["who"]},
			}}}, nil
		})
	return s
}

// connectBackend connects a client session to server over an in-memory pair.
func connectBackend(t *testing.T, server *mcp.Server, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "facade", Version: "1"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// boundFacade returns a facade bound to the given backend plus a client of the facade.
func boundFacade(t *testing.T, backend *mcp.Server) (*proxy.Facade, *mcp.ClientSession) {
	t.Helper()
	f := proxy.New("test", "1.0.0")
	if err := f.Bind(context.Background(), connectBackend(t, backend, nil)); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return f, connectBackend(t, f.Server(), nil)
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

func TestFacadeMirrorsLists(t *testing.T) {
	_, c := boundFacade(t, newBackend(t, nil))
	ctx := context.Background()

	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.InputSchema == nil {
			t.Errorf("tool %s has nil schema", tl.Name)
		}
	}
	if got := fmt.Sprint(sorted(names)); got != "[add echo]" {
		t.Errorf("tools = %s", got)
	}

	res, err := c.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resources) != 1 || res.Resources[0].URI != "file:///a" {
		t.Errorf("resources = %+v", res.Resources)
	}

	tpls, err := c.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tpls.ResourceTemplates) != 1 || tpls.ResourceTemplates[0].URITemplate != "file:///{p}" {
		t.Errorf("templates = %+v", tpls.ResourceTemplates)
	}

	prompts, err := c.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "greet" {
		t.Errorf("prompts = %+v", prompts.Prompts)
	}
}

func TestFacadeForwardsCallTool(t *testing.T) {
	_, c := boundFacade(t, newBackend(t, nil))
	res, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"s": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "hi" {
		t.Errorf("content = %#v", res.Content[0])
	}
}

func TestFacadeForwardsReadResourceAndPrompt(t *testing.T) {
	_, c := boundFacade(t, newBackend(t, nil))
	ctx := context.Background()

	r, err := c.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Contents) != 1 || r.Contents[0].Text != "A" {
		t.Errorf("resource = %+v", r.Contents)
	}

	r, err = c.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Contents) != 1 || r.Contents[0].Text != "T:file:///zzz" {
		t.Errorf("template read = %+v", r.Contents)
	}

	p, err := c.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"who": "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 {
		t.Fatalf("prompt = %+v", p)
	}
	if tc, ok := p.Messages[0].Content.(*mcp.TextContent); !ok || tc.Text != "hello bob" {
		t.Errorf("prompt content = %#v", p.Messages[0].Content)
	}
}

func TestFacadePaginates(t *testing.T) {
	backend := mcp.NewServer(&mcp.Implementation{Name: "b", Version: "1"}, &mcp.ServerOptions{PageSize: 2})
	for i := 0; i < 5; i++ {
		mcp.AddTool(backend, &mcp.Tool{Name: fmt.Sprintf("t%d", i)},
			func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			})
	}
	f, c := boundFacade(t, backend)
	if n := len(f.Snapshot().Tools); n != 5 {
		t.Errorf("snapshot tools = %d, want 5", n)
	}
	var names []string
	var cursor string
	for {
		res, err := c.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(names) != 5 {
		t.Errorf("facade lists %d tools, want 5: %v", len(names), names)
	}
}

func TestFacadeCountsCalls(t *testing.T) {
	f, c := boundFacade(t, newBackend(t, nil))
	ctx := context.Background()
	if f.Calls() != 0 {
		t.Fatalf("initial calls = %d", f.Calls())
	}
	if _, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": 1, "b": 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet"}); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestFacadeUnboundReturnsUnavailable(t *testing.T) {
	f, c := boundFacade(t, newBackend(t, nil))
	f.Unbind()
	if n := len(f.Snapshot().Tools); n != 2 {
		t.Errorf("lists should stay after Unbind, tools = %d", n)
	}
	_, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"s": "x"}})
	if err == nil {
		t.Fatal("expected error from unbound facade")
	}
	if f.Calls() != 0 {
		t.Errorf("failed forward must not count, calls = %d", f.Calls())
	}
}

var objectSchema = map[string]any{"type": "object"}

func rawBackend(t *testing.T) *mcp.Server {
	t.Helper()
	return mcp.NewServer(&mcp.Implementation{Name: "raw", Version: "1"}, nil)
}

func TestFacadeForwardsRawArgumentsVerbatim(t *testing.T) {
	backend := rawBackend(t)
	var got json.RawMessage
	backend.AddTool(&mcp.Tool{Name: "rec", InputSchema: objectSchema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			got = append(json.RawMessage(nil), req.Params.Arguments...)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	_, c := boundFacade(t, backend)

	body := json.RawMessage(`{"o":{"n":[1,2,{"x":null}]},"f":1.50,"big":12345678901234567890,"s":"a\u00e9"}`)
	if _, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "rec", Arguments: body}); err != nil {
		t.Fatal(err)
	}
	var want, have bytes.Buffer
	if err := json.Compact(&want, body); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&have, got); err != nil {
		t.Fatalf("backend got invalid JSON %q: %v", got, err)
	}
	if want.String() != have.String() {
		t.Errorf("backend arguments\n got  %s\n want %s", have.String(), want.String())
	}
}

// The SDK client normalises absent arguments to {} on the wire, so a real
// client never reaches the facade with nil arguments over this transport. The
// test strips them in a facade-side middleware to exercise the handler's
// nil-RawMessage path directly: it must not forward "arguments":null.
func TestFacadeForwardsNoArgumentsAsAbsent(t *testing.T) {
	backend := rawBackend(t)
	var got json.RawMessage
	var called bool
	backend.AddTool(&mcp.Tool{Name: "noargs", InputSchema: objectSchema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			got = append(json.RawMessage(nil), req.Params.Arguments...)
			return &mcp.CallToolResult{}, nil
		})
	f, c := boundFacade(t, backend)

	var facadeSawNil bool
	f.Server().AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if r, ok := req.(*mcp.CallToolRequest); ok {
				r.Params.Arguments = nil
				facadeSawNil = len(r.Params.Arguments) == 0
			}
			return next(ctx, method, req)
		}
	})

	if _, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "noargs"}); err != nil {
		t.Fatal(err)
	}
	if !called || !facadeSawNil {
		t.Fatalf("called backend=%v facade nil args=%v", called, facadeSawNil)
	}
	// The backend SDK client turns absent arguments into {}; what matters is
	// that the facade did not send an explicit JSON null.
	if string(got) == "null" {
		t.Errorf("backend received arguments null, want absent or {}")
	}
	var m map[string]any
	if len(got) != 0 {
		if err := json.Unmarshal(got, &m); err != nil || m == nil {
			t.Errorf("backend arguments %q are not a JSON object: %v", got, err)
		}
	}
}

func TestFacadeKeepsIsErrorResult(t *testing.T) {
	backend := rawBackend(t)
	backend.AddTool(&mcp.Tool{Name: "fail", InputSchema: objectSchema},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "bad"}}}, nil
		})
	_, c := boundFacade(t, backend)
	res, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "fail"})
	if err != nil {
		t.Fatalf("IsError result must not become a protocol error: %v", err)
	}
	if !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != "bad" {
		t.Errorf("content = %#v", res.Content[0])
	}
}

func TestFacadeResyncReplacesAndRemoves(t *testing.T) {
	backend := newBackend(t, nil)
	f, c := boundFacade(t, backend)

	backend.AddTool(&mcp.Tool{Name: "echo", Description: "changed", InputSchema: objectSchema},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	backend.RemoveTools("add")
	if err := f.Resync(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := f.Snapshot()
	if len(snap.Tools) != 1 || snap.Tools[0].Name != "echo" || snap.Tools[0].Description != "changed" {
		t.Fatalf("snapshot tools = %+v", snap.Tools)
	}
	tools, err := c.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Description != "changed" {
		t.Errorf("facade tools = %+v", tools.Tools)
	}
}

// rewriteLists makes the backend advertise definitions that the SDK's own
// registration API would refuse, like a misbehaving third-party server.
func rewriteLists(backend *mcp.Server, rewrite func(method string, res mcp.Result)) {
	backend.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err == nil {
				rewrite(method, res)
			}
			return res, err
		}
	})
}

func TestFacadeDefaultsNilInputSchema(t *testing.T) {
	backend := rawBackend(t)
	for _, n := range []string{"nilschema", "emptyschema"} {
		backend.AddTool(&mcp.Tool{Name: n, InputSchema: objectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	}
	rewriteLists(backend, func(method string, res mcp.Result) {
		if r, ok := res.(*mcp.ListToolsResult); ok {
			for _, tl := range r.Tools {
				if tl.Name == "nilschema" {
					tl.InputSchema = nil
				} else {
					tl.InputSchema = map[string]any{"description": "no type"}
				}
			}
		}
	})
	f, c := boundFacade(t, backend)

	byName := map[string]*mcp.Tool{}
	for _, tl := range f.Snapshot().Tools {
		byName[tl.Name] = tl
	}
	if len(byName) != 2 {
		t.Fatalf("snapshot tools = %v", byName)
	}
	for n, tl := range byName {
		m, ok := tl.InputSchema.(map[string]any)
		if !ok || m["type"] != "object" {
			t.Errorf("%s schema = %#v, want type object", n, tl.InputSchema)
		}
	}
	if m := byName["emptyschema"].InputSchema.(map[string]any); m["description"] != "no type" {
		t.Errorf("existing schema keys must be preserved: %#v", m)
	}
	tools, err := c.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 2 {
		t.Errorf("facade lists %d tools, want 2", len(tools.Tools))
	}
}

func TestFacadeSkipsUnregisterableDefinitions(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	backend := newBackend(t, nil) // echo, add, file:///a, file:///{p}, greet
	for _, n := range []string{"boolschema", "stringschema"} {
		backend.AddTool(&mcp.Tool{Name: n, InputSchema: objectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	}
	backend.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "mem:///{q}", Name: "badtpl"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		})
	backend.AddResource(&mcp.Resource{URI: "mem:///ok", Name: "badres"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		})
	rewriteLists(backend, func(method string, res mcp.Result) {
		switch r := res.(type) {
		case *mcp.ListToolsResult:
			for _, tl := range r.Tools {
				switch tl.Name {
				case "boolschema":
					tl.InputSchema = true
				case "stringschema":
					tl.InputSchema = map[string]any{"type": "string"}
				}
			}
		case *mcp.ListResourceTemplatesResult:
			for _, tp := range r.ResourceTemplates {
				if tp.Name == "badtpl" {
					tp.URITemplate = "mem:///{q"
				}
			}
		case *mcp.ListResourcesResult:
			for _, rs := range r.Resources {
				if rs.Name == "badres" {
					rs.URI = "ht tp://%zz"
				}
			}
		}
	})

	f := proxy.New("test", "1")
	if err := f.Bind(context.Background(), connectBackend(t, backend, nil)); err != nil {
		t.Fatalf("Bind/Resync must not fail on rejectable entries: %v", err)
	}
	if err := f.Resync(context.Background()); err != nil {
		t.Fatalf("second Resync: %v", err)
	}
	c := connectBackend(t, f.Server(), nil)
	ctx := context.Background()

	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if got := fmt.Sprint(sorted(names)); got != "[add echo]" {
		t.Errorf("tools = %s", got)
	}
	tpls, err := c.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tpls.ResourceTemplates) != 1 || tpls.ResourceTemplates[0].URITemplate != "file:///{p}" {
		t.Errorf("templates = %+v", tpls.ResourceTemplates)
	}
	res, err := c.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resources) != 1 || res.Resources[0].URI != "file:///a" {
		t.Errorf("resources = %+v", res.Resources)
	}
	snap := f.Snapshot()
	if len(snap.Tools) != 2 || len(snap.ResourceTemplates) != 1 || len(snap.Resources) != 1 {
		t.Errorf("snapshot must hold only mirrored entries: %d tools, %d templates, %d resources",
			len(snap.Tools), len(snap.ResourceTemplates), len(snap.Resources))
	}
}

// waitFor polls cond until it holds or two seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func toolNames(t *testing.T, c *mcp.ClientSession) []string {
	t.Helper()
	res, err := c.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return sorted(names)
}

func TestFacadeResyncOnToolListChanged(t *testing.T) {
	backend := newBackend(t, nil)
	f := proxy.New("test", "1")
	if err := f.Bind(context.Background(), connectBackend(t, backend, f.ClientOptions())); err != nil {
		t.Fatal(err)
	}
	c := connectBackend(t, f.Server(), nil)

	backend.AddTool(&mcp.Tool{Name: "later", InputSchema: objectSchema},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	waitFor(t, "later to appear", func() bool { return strings.Contains(fmt.Sprint(toolNames(t, c)), "later") })

	backend.RemoveTools("echo")
	waitFor(t, "echo to vanish", func() bool { return !strings.Contains(fmt.Sprint(toolNames(t, c)), "echo") })
	if got := fmt.Sprint(toolNames(t, c)); got != "[add later]" {
		t.Errorf("tools = %s", got)
	}
}

func TestFacadeRebindAfterSessionLoss(t *testing.T) {
	ctx := context.Background()
	f := proxy.New("test", "1")
	first := connectBackend(t, newBackend(t, nil), nil)
	if err := f.Bind(ctx, first); err != nil {
		t.Fatal(err)
	}
	c := connectBackend(t, f.Server(), nil)
	call := func() error {
		_, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"s": "x"}})
		return err
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}

	if err := first.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	err := call()
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("call after session loss: err = %v, want backend unavailable", err)
	}
	// The mirrored lists stay while the session is gone.
	if n := len(f.Snapshot().Tools); n != 2 {
		t.Errorf("tools after loss = %d, want 2", n)
	}

	if err := f.Bind(ctx, connectBackend(t, newBackend(t, nil), nil)); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil {
		t.Fatalf("call after rebind: %v", err)
	}
	// The first session's watcher must not have unbound the new session; give
	// it time to have run (it fired when first closed).
	time.Sleep(50 * time.Millisecond)
	if err := call(); err != nil {
		t.Fatalf("call after stale watcher ran: %v", err)
	}
}

func TestFacadeToleratesMissingCapabilities(t *testing.T) {
	backend := mcp.NewServer(&mcp.Implementation{Name: "empty", Version: "1"}, nil)
	f := proxy.New("test", "1")
	if err := f.Bind(context.Background(), connectBackend(t, backend, nil)); err != nil {
		t.Fatalf("Bind to capability-less backend: %v", err)
	}
	snap := f.Snapshot()
	if len(snap.Tools)+len(snap.Resources)+len(snap.ResourceTemplates)+len(snap.Prompts) != 0 {
		t.Errorf("snapshot not empty: %+v", snap)
	}
}

func TestFacadeWrapsBackendErrors(t *testing.T) {
	backend := rawBackend(t)
	backend.AddTool(&mcp.Tool{Name: "explode", InputSchema: objectSchema},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("boom")
		})
	f, c := boundFacade(t, backend)
	_, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "explode"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to contain boom", err)
	}
	if f.Calls() != 0 {
		t.Errorf("failed forward must not count, calls = %d", f.Calls())
	}
}

func TestFacadeUnboundListsAreEmptyNoPanic(t *testing.T) {
	f := proxy.New("test", "1")
	c := connectBackend(t, f.Server(), nil)
	ctx := context.Background()
	tools, err := c.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 0 {
		t.Errorf("tools = %+v, err = %v", tools, err)
	}
	res, err := c.ListResources(ctx, nil)
	if err != nil || len(res.Resources) != 0 {
		t.Errorf("resources = %+v, err = %v", res, err)
	}
	tpls, err := c.ListResourceTemplates(ctx, nil)
	if err != nil || len(tpls.ResourceTemplates) != 0 {
		t.Errorf("templates = %+v, err = %v", tpls, err)
	}
	prompts, err := c.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 0 {
		t.Errorf("prompts = %+v, err = %v", prompts, err)
	}
	if _, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "x"}); err == nil {
		t.Error("calling a tool on an unbound facade must fail")
	}
	if err := f.Resync(ctx); !errors.Is(err, proxy.ErrBackendUnavailable) {
		t.Errorf("Resync on unbound facade = %v, want ErrBackendUnavailable", err)
	}
}

// failLists makes every list method of the backend fail with the given code,
// like a server that does not implement the feature.
func failLists(backend *mcp.Server, code int64) {
	backend.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if strings.HasSuffix(method, "/list") || method == "resources/templates/list" {
				return nil, &jsonrpc.Error{Code: code, Message: "nope"}
			}
			return next(ctx, method, req)
		}
	})
}

func TestFacadeTreatsMethodNotFoundAsEmptyList(t *testing.T) {
	backend := newBackend(t, nil)
	failLists(backend, jsonrpc.CodeMethodNotFound)
	f := proxy.New("test", "1")
	if err := f.Bind(context.Background(), connectBackend(t, backend, nil)); err != nil {
		t.Fatalf("method-not-found must read as an empty list: %v", err)
	}
	if n := len(f.Snapshot().Tools); n != 0 {
		t.Errorf("tools = %d, want 0", n)
	}
}

func TestFacadeSkipsListsOfUnadvertisedCapabilities(t *testing.T) {
	// A feature-less backend advertises no capabilities, so the facade must not
	// even ask: a list error that is not method-not-found would otherwise fail Bind.
	backend := mcp.NewServer(&mcp.Implementation{Name: "empty", Version: "1"}, nil)
	failLists(backend, jsonrpc.CodeInternalError)
	f := proxy.New("test", "1")
	if err := f.Bind(context.Background(), connectBackend(t, backend, nil)); err != nil {
		t.Fatalf("lists of unadvertised capabilities must not be requested: %v", err)
	}
}

func TestFacadeBindFailsOnRealListErrors(t *testing.T) {
	backend := newBackend(t, nil)
	failLists(backend, jsonrpc.CodeInternalError)
	f := proxy.New("test", "1")
	if err := f.Bind(context.Background(), connectBackend(t, backend, nil)); err == nil {
		t.Fatal("an internal error from an advertised list must fail Bind")
	}
}
