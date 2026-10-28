// Package proxy mirrors a backend MCP server's capabilities onto a local
// mcp.Server and forwards every call to the backend session.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrBackendUnavailable is returned by forwarding handlers while no backend
// session is bound.
var ErrBackendUnavailable = errors.New("proxy: backend unavailable")

// Snapshot is a copy of the lists the facade currently mirrors.
type Snapshot struct {
	Tools             []*mcp.Tool
	Resources         []*mcp.Resource
	ResourceTemplates []*mcp.ResourceTemplate
	Prompts           []*mcp.Prompt
}

// Facade holds one backend client session and exposes an mcp.Server whose
// tools, resources, resource templates and prompts mirror the backend's.
type Facade struct {
	name   string
	server *mcp.Server

	resyncMu sync.Mutex // serializes Resync so diffs never interleave

	mu   sync.RWMutex
	sess *mcp.ClientSession
	snap Snapshot

	calls atomic.Uint64
}

// New creates a facade whose server identifies as "mcp-machine/<name>".
func New(name, version string) *Facade {
	return &Facade{
		name: name,
		server: mcp.NewServer(
			&mcp.Implementation{Name: "mcp-machine/" + name, Version: version},
			// Advertise all three capabilities even while the lists are empty.
			// Logging is deliberately not advertised: log messages are not forwarded.
			&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
				Tools:     &mcp.ToolCapabilities{ListChanged: true},
				Resources: &mcp.ResourceCapabilities{ListChanged: true},
				Prompts:   &mcp.PromptCapabilities{ListChanged: true},
			}},
		),
	}
}

// Server returns the mirrored server to serve to clients.
func (f *Facade) Server() *mcp.Server { return f.server }

// Bind sets the backend session and resyncs the mirrored lists from it.
func (f *Facade) Bind(ctx context.Context, sess *mcp.ClientSession) error {
	f.mu.Lock()
	f.sess = sess
	f.mu.Unlock()
	return f.Resync(ctx)
}

// Unbind clears the backend session. Mirrored lists stay until the next Bind.
func (f *Facade) Unbind() {
	f.mu.Lock()
	f.sess = nil
	f.mu.Unlock()
}

// Calls returns the number of successfully forwarded calls.
func (f *Facade) Calls() uint64 { return f.calls.Load() }

// Snapshot returns a copy of the currently mirrored lists.
func (f *Facade) Snapshot() Snapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Snapshot{
		Tools:             append([]*mcp.Tool(nil), f.snap.Tools...),
		Resources:         append([]*mcp.Resource(nil), f.snap.Resources...),
		ResourceTemplates: append([]*mcp.ResourceTemplate(nil), f.snap.ResourceTemplates...),
		Prompts:           append([]*mcp.Prompt(nil), f.snap.Prompts...),
	}
}

// ClientOptions returns client options whose list-changed handlers resync the
// facade. Pass them to the client that creates the backend session.
func (f *Facade) ClientOptions() *mcp.ClientOptions {
	resync := func(ctx context.Context, what string) {
		if err := f.Resync(ctx); err != nil {
			slog.Warn("proxy: resync failed", "server", f.name, "trigger", what, "err", err)
		}
	}
	return &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			resync(ctx, "tools/list_changed")
		},
		PromptListChangedHandler: func(ctx context.Context, _ *mcp.PromptListChangedRequest) {
			resync(ctx, "prompts/list_changed")
		},
		ResourceListChangedHandler: func(ctx context.Context, _ *mcp.ResourceListChangedRequest) {
			resync(ctx, "resources/list_changed")
		},
	}
}

func (f *Facade) session() (*mcp.ClientSession, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.sess == nil {
		return nil, ErrBackendUnavailable
	}
	return f.sess, nil
}

// Resync reads every list from the backend (following pagination), diffs it
// against the current snapshot, and adds, replaces or removes only the entries
// that changed. Nothing is modified if any list fails to load.
func (f *Facade) Resync(ctx context.Context) error {
	f.resyncMu.Lock()
	defer f.resyncMu.Unlock()

	sess, err := f.session()
	if err != nil {
		return err
	}

	tools, err := listAll(func(cursor string) ([]*mcp.Tool, string, error) {
		r, err := sess.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return r.Tools, r.NextCursor, nil
	})
	if err != nil {
		return err
	}
	resources, err := listAll(func(cursor string) ([]*mcp.Resource, string, error) {
		r, err := sess.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return r.Resources, r.NextCursor, nil
	})
	if err != nil {
		return err
	}
	templates, err := listAll(func(cursor string) ([]*mcp.ResourceTemplate, string, error) {
		r, err := sess.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return r.ResourceTemplates, r.NextCursor, nil
	})
	if err != nil {
		return err
	}
	prompts, err := listAll(func(cursor string) ([]*mcp.Prompt, string, error) {
		r, err := sess.ListPrompts(ctx, &mcp.ListPromptsParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return r.Prompts, r.NextCursor, nil
	})
	if err != nil {
		return err
	}

	for _, t := range tools {
		if t.InputSchema == nil {
			t.InputSchema = map[string]any{"type": "object"}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	removed, changed := diff(f.snap.Tools, tools, func(t *mcp.Tool) string { return t.Name })
	f.server.RemoveTools(removed...)
	for _, t := range changed {
		f.server.AddTool(t, f.forwardTool)
	}
	f.snap.Tools = tools

	removed, changedR := diff(f.snap.Resources, resources, func(r *mcp.Resource) string { return r.URI })
	f.server.RemoveResources(removed...)
	for _, r := range changedR {
		f.server.AddResource(r, f.forwardResource)
	}
	f.snap.Resources = resources

	removed, changedT := diff(f.snap.ResourceTemplates, templates, func(t *mcp.ResourceTemplate) string { return t.URITemplate })
	f.server.RemoveResourceTemplates(removed...)
	for _, t := range changedT {
		f.server.AddResourceTemplate(t, f.forwardResource)
	}
	f.snap.ResourceTemplates = templates

	removed, changedP := diff(f.snap.Prompts, prompts, func(p *mcp.Prompt) string { return p.Name })
	f.server.RemovePrompts(removed...)
	for _, p := range changedP {
		f.server.AddPrompt(p, f.forwardPrompt)
	}
	f.snap.Prompts = prompts

	return nil
}

// listAll follows NextCursor until the backend reports no more pages.
func listAll[T any](page func(cursor string) ([]T, string, error)) ([]T, error) {
	var all []T
	cursor := ""
	for {
		items, next, err := page(cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

// diff returns the keys present in old but not in updated, and the entries of
// updated that are new or differ from their old counterpart.
func diff[T any](old, updated []T, key func(T) string) (removed []string, changed []T) {
	oldByKey := make(map[string][]byte, len(old))
	for _, o := range old {
		oldByKey[key(o)] = fingerprint(o)
	}
	seen := make(map[string]struct{}, len(updated))
	for _, u := range updated {
		k := key(u)
		seen[k] = struct{}{}
		if prev, ok := oldByKey[k]; !ok || !bytes.Equal(prev, fingerprint(u)) {
			changed = append(changed, u)
		}
	}
	for _, o := range old {
		if _, ok := seen[key(o)]; !ok {
			removed = append(removed, key(o))
		}
	}
	return removed, changed
}

func fingerprint(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func (f *Facade) forwardTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sess, err := f.session()
	if err != nil {
		return nil, err
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
	if err != nil {
		return nil, err
	}
	f.calls.Add(1)
	return res, nil
}

func (f *Facade) forwardResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	sess, err := f.session()
	if err != nil {
		return nil, err
	}
	res, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
	if err != nil {
		return nil, err
	}
	f.calls.Add(1)
	return res, nil
}

func (f *Facade) forwardPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	sess, err := f.session()
	if err != nil {
		return nil, err
	}
	res, err := sess.GetPrompt(ctx, &mcp.GetPromptParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
	if err != nil {
		return nil, err
	}
	f.calls.Add(1)
	return res, nil
}
