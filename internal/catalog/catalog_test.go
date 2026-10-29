package catalog_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/catalog"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const githubYAML = `# GitHub's official MCP server, run locally from its image.
# https://github.com/github/github-mcp-server
# Secret: vault kv put kv/mcp-machine/github token=ghp_...
# category: Development
servers:
  - name: github
    type: command
    command:
      run: "docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server"
      env:
        GITHUB_PERSONAL_ACCESS_TOKEN: "${vault:kv/data/mcp-machine/github#token}"
`

func TestLoadParsesHeaderAndSpec(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"github.yaml": githubYAML,
		"fetch.yaml":  "# Fetch pages.\nservers:\n  - {name: fetch, type: command, command: {command: uvx, args: [mcp-server-fetch]}}\n",
		"notes.txt":   "ignored",
	})
	entries := catalog.Load(dir, nil)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2", entries)
	}
	if entries[0].Name != "fetch" || entries[1].Name != "github" {
		t.Errorf("order = %s, %s; want sorted by name", entries[0].Name, entries[1].Name)
	}
	g := entries[1]
	if g.Type != "command" || g.File != "github.yaml" || g.Category != "development" {
		t.Errorf("entry = %+v", g)
	}
	if g.Description != "GitHub's official MCP server, run locally from its image." {
		t.Errorf("description = %q", g.Description)
	}
	if g.Upstream != "https://github.com/github/github-mcp-server" {
		t.Errorf("upstream = %q", g.Upstream)
	}
	if g.Secret != "vault kv put kv/mcp-machine/github token=ghp_..." {
		t.Errorf("secret = %q", g.Secret)
	}
	if got := g.Spec.Command.Env["GITHUB_PERSONAL_ACCESS_TOKEN"]; got != "${vault:kv/data/mcp-machine/github#token}" {
		t.Errorf("reference not verbatim: %q", got)
	}
	if !g.HasVaultRefs() || entries[0].HasVaultRefs() {
		t.Error("HasVaultRefs wrong")
	}
	f := entries[0]
	if f.Description != "Fetch pages." || f.Category != "" || f.Upstream != "" || f.Secret != "" {
		t.Errorf("fetch entry = %+v", f)
	}
	if f.Spec.Serve != nil {
		t.Error("defaults were applied to a catalog spec")
	}
}

func TestLoadSkipsBadFiles(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dir := writeFiles(t, map[string]string{
		"good.yaml":      "# ok\nservers:\n  - {name: good, type: remote, remote: {url: https://x/mcp}}\n",
		"two.yaml":       "# two\nservers:\n  - {name: two, type: remote}\n  - {name: other, type: remote}\n",
		"renamed.yaml":   "# wrong name\nservers:\n  - {name: something-else, type: remote}\n",
		"broken.yaml":    "# broken\nservers: [\n",
		"global.yaml":    "# global key\nlisten: ':1'\nservers:\n  - {name: global, type: remote}\n",
		"untyped.yaml":   "# no type\nservers:\n  - {name: untyped}\n",
		"nocomment.yaml": "servers:\n  - {name: nocomment, type: remote}\n",
	})
	entries := catalog.Load(dir, log)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "good,nocomment" {
		t.Errorf("entries = %v, want good and nocomment only", names)
	}
	for _, want := range []string{"two.yaml", "renamed.yaml", "broken.yaml", "global.yaml", "untyped.yaml", "holds 2 servers", "something-else"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "good.yaml") {
		t.Error("a good file was logged as skipped")
	}
}

func TestLoadMissingOrEmptyDir(t *testing.T) {
	if got := catalog.Load(filepath.Join(t.TempDir(), "nope"), nil); got != nil {
		t.Errorf("missing dir: %v", got)
	}
	if got := catalog.Load("", nil); got != nil {
		t.Errorf("empty dir name: %v", got)
	}
	if got := catalog.Load(t.TempDir(), nil); got != nil {
		t.Errorf("empty dir: %v", got)
	}
}
