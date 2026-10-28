// Package proxy mirrors a backend MCP server's capabilities onto a local
// mcp.Server and forwards every call to the backend session.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	tools = f.mirrorableTools(tools)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.snap.Tools = apply(f, "tool", f.snap.Tools, tools, func(t *mcp.Tool) string { return t.Name },
		f.server.RemoveTools, func(t *mcp.Tool) { f.server.AddTool(t, f.forwardTool) })
	f.snap.Resources = apply(f, "resource", f.snap.Resources, resources, func(r *mcp.Resource) string { return r.URI },
		f.server.RemoveResources, func(r *mcp.Resource) { f.server.AddResource(r, f.forwardResource) })
	f.snap.ResourceTemplates = apply(f, "resource template", f.snap.ResourceTemplates, templates,
		func(t *mcp.ResourceTemplate) string { return t.URITemplate },
		f.server.RemoveResourceTemplates, func(t *mcp.ResourceTemplate) { f.server.AddResourceTemplate(t, f.forwardResource) })
	f.snap.Prompts = apply(f, "prompt", f.snap.Prompts, prompts, func(p *mcp.Prompt) string { return p.Name },
		f.server.RemovePrompts, func(p *mcp.Prompt) { f.server.AddPrompt(p, f.forwardPrompt) })

	return nil
}

// mirrorableTools normalises backend tool definitions so the SDK accepts them.
// A missing input schema, or an object schema without "type", becomes
// {"type":"object"}-compatible; tools whose input schema is not an object
// schema are skipped with a warning, since the SDK would panic on them.
func (f *Facade) mirrorableTools(tools []*mcp.Tool) []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		switch schema := t.InputSchema.(type) {
		case nil:
			c := *t
			c.InputSchema = map[string]any{"type": "object"}
			t = &c
		case map[string]any:
			switch typ, has := schema["type"]; {
			case !has:
				m := make(map[string]any, len(schema)+1)
				for k, v := range schema {
					m[k] = v
				}
				m["type"] = "object"
				c := *t
				c.InputSchema = m
				t = &c
			case typ != "object":
				slog.Warn("proxy: skipping tool with non-object input schema", "server", f.name, "tool", t.Name)
				continue
			}
		default:
			slog.Warn("proxy: skipping tool with non-object input schema", "server", f.name, "tool", t.Name)
			continue
		}
		out = append(out, t)
	}
	return out
}

// apply diffs updated against old, removes vanished entries, and adds or
// replaces changed ones. An add that panics (the SDK panics on definitions it
// cannot accept) is recovered, logged, and skipped; the entry is then left
// out of the returned snapshot and any previous version is removed.
func apply[T any](f *Facade, kind string, old, updated []T, key func(T) string, remove func(...string), add func(T)) []T {
	removed, changed := diff(old, updated, key)
	remove(removed...)
	failed := map[string]bool{}
	for _, c := range changed {
		if err := safely(func() { add(c) }); err != nil {
			k := key(c)
			slog.Warn("proxy: skipping definition the server rejected", "server", f.name, "kind", kind, "key", k, "err", err)
			failed[k] = true
			remove(k)
		}
	}
	if len(failed) == 0 {
		return updated
	}
	kept := make([]T, 0, len(updated))
	for _, u := range updated {
		if !failed[key(u)] {
			kept = append(kept, u)
		}
	}
	return kept
}

// safely runs fn and converts a panic into an error.
func safely(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	fn()
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

// unmarshalableSeq makes fingerprints of unmarshalable entries unique.
var unmarshalableSeq atomic.Uint64

func fingerprint(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Never equal to any other fingerprint, so the entry counts as changed.
		return []byte(fmt.Sprintf("unmarshalable-%d", unmarshalableSeq.Add(1)))
	}
	return b
}

// forwardTool relays a tool call to the backend. The client's _meta is not
// forwarded: protocol metadata belongs to each hop. Arguments are passed as the
// raw JSON the client sent; when there are none, an untyped nil is used so the
// field is omitted rather than sent as null.
func (f *Facade) forwardTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sess, err := f.session()
	if err != nil {
		return nil, err
	}
	var args any
	if len(req.Params.Arguments) > 0 {
		args = req.Params.Arguments
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: args})
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
