package proxy_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

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
