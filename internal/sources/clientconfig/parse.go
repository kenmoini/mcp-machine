// Package clientconfig parses the "mcpServers" JSON used by MCP clients such
// as Claude Desktop, Claude Code (.mcp.json) and Cursor into server specs.
package clientconfig

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
)

const maxNameLen = 63

// entry is one client-config server definition.
type entry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// Parse converts client-config JSON into server specs sorted by name. The
// input is {"mcpServers": {"<name>": {...}}}; a bare object of entries
// (every value an object with "command" or "url") is accepted as well.
func Parse(data []byte) ([]config.ServerSpec, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse client config: %w", err)
	}
	var raw map[string]json.RawMessage
	if body, ok := top["mcpServers"]; ok {
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse mcpServers: %w", err)
		}
	} else {
		if !looksBare(top) {
			return nil, fmt.Errorf("no mcpServers key found and top-level is not a map of server entries")
		}
		raw = top
	}

	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)

	specs := make([]config.ServerSpec, 0, len(names))
	seen := map[string]string{}
	for _, orig := range names {
		name := SanitizeName(orig)
		if name == "" {
			return nil, fmt.Errorf("server name %q is empty after sanitizing", orig)
		}
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("server names %q and %q both sanitize to %q", prev, orig, name)
		}
		seen[name] = orig
		var e entry
		if err := json.Unmarshal(raw[orig], &e); err != nil {
			return nil, fmt.Errorf("server %q: %w", orig, err)
		}
		spec, err := convert(name, e)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", orig, err)
		}
		specs = append(specs, spec)
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

// looksBare reports whether top is a non-empty map whose values are all
// objects carrying "command" or "url".
func looksBare(top map[string]json.RawMessage) bool {
	if len(top) == 0 {
		return false
	}
	for _, v := range top {
		var probe map[string]json.RawMessage
		if json.Unmarshal(v, &probe) != nil {
			return false
		}
		_, hasCmd := probe["command"]
		_, hasURL := probe["url"]
		if !hasCmd && !hasURL {
			return false
		}
	}
	return true
}

func convert(name string, e entry) (config.ServerSpec, error) {
	remote := func(transport string) (config.ServerSpec, error) {
		if e.URL == "" {
			return config.ServerSpec{}, fmt.Errorf("type %q requires a url", e.Type)
		}
		return config.ServerSpec{
			Name:   name,
			Type:   "remote",
			Remote: &config.RemoteSpec{URL: e.URL, Transport: transport, Headers: e.Headers},
		}, nil
	}
	command := func() (config.ServerSpec, error) {
		if e.Command == "" {
			return config.ServerSpec{}, fmt.Errorf("type %q requires a command", e.Type)
		}
		return config.ServerSpec{
			Name:    name,
			Type:    "command",
			Command: &config.CommandSpec{Command: e.Command, Args: e.Args, Env: e.Env},
		}, nil
	}

	switch e.Type {
	case "http", "streamable-http":
		return remote("http")
	case "sse":
		return remote("sse")
	case "stdio":
		return command()
	case "":
		switch {
		case e.Command != "":
			return command()
		case e.URL != "":
			return remote("http")
		}
		return config.ServerSpec{}, fmt.Errorf("entry has neither command nor url")
	default:
		return config.ServerSpec{}, fmt.Errorf("unknown type %q", e.Type)
	}
}

// SanitizeName lowercases s, replaces every run of characters outside
// [a-z0-9] with a single "-", truncates to 63 characters and trims leading
// and trailing "-". The result may be empty.
func SanitizeName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxNameLen {
		out = strings.TrimRight(out[:maxNameLen], "-")
	}
	return out
}
