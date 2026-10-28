package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

const writerOriginal = `# top comment
listen: ":8080"
public_url: "http://mcp.lan:8080" # inline on public_url
# servers follow
servers:
  # the first server
  - name: alpha # inline on alpha
    type: remote
    remote:
      url: https://alpha.example.com/mcp
      transport: http
      headers:
        Authorization: "Bearer ${TOK}" # secret stays a placeholder
  # the second server
  - name: beta
    type: command
    command:
      command: npx
      args: ["-y", "some-server"]
`

func newTestConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loadAccept parses the file with every ${VAR} resolved, so tests need no env.
func loadAccept(t *testing.T, p string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(readFile(t, p)), func(string) (string, bool) { return "x", true })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gammaSpec() config.ServerSpec {
	return config.ServerSpec{
		Name: "gamma",
		Type: "remote",
		Remote: &config.RemoteSpec{
			URL:       "https://gamma.example.com/mcp",
			Transport: "sse",
			Headers:   map[string]string{"X-Key": "${GAMMA_KEY}"},
		},
	}
}

func TestWriterAddPreservesCommentsAndOrder(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	w := config.Writer{Path: p}
	if err := w.AddServer(gammaSpec()); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	got := readFile(t, p)
	if !strings.HasPrefix(got, writerOriginal) {
		t.Fatalf("original content not preserved byte-for-byte as prefix.\ngot:\n%s", got)
	}
	appended := strings.TrimPrefix(got, writerOriginal)
	for _, want := range []string{"- name: gamma", "type: remote", "url: https://gamma.example.com/mcp", "transport: sse", "${GAMMA_KEY}"} {
		if !strings.Contains(appended, want) {
			t.Errorf("appended block missing %q:\n%s", want, appended)
		}
	}
	for _, unwanted := range []string{"enabled", "build", "serve", "container", "command:"} {
		if strings.Contains(appended, unwanted) {
			t.Errorf("appended block contains %q, want nil sections omitted:\n%s", unwanted, appended)
		}
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind: %v", err)
	}
}

func TestWriterAddCreatesServersKey(t *testing.T) {
	p := newTestConfig(t, "# only a comment\nlisten: \":8080\"\n")
	if err := (config.Writer{Path: p}).AddServer(gammaSpec()); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	got := readFile(t, p)
	if !strings.Contains(got, "# only a comment") || !strings.Contains(got, "- name: gamma") {
		t.Errorf("unexpected content:\n%s", got)
	}
	cfg := loadAccept(t, p)
	if len(cfg.Servers) != 1 {
		t.Errorf("servers = %d, want 1", len(cfg.Servers))
	}
}

func TestWriterAddToEmptyFlowServers(t *testing.T) {
	p := newTestConfig(t, "listen: \":8080\"\nservers: []\n")
	if err := (config.Writer{Path: p}).AddServer(gammaSpec()); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	cfg := loadAccept(t, p)
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "gamma" {
		t.Errorf("servers = %+v", cfg.Servers)
	}
	if strings.Contains(readFile(t, p), "[") {
		t.Errorf("expected block style, got:\n%s", readFile(t, p))
	}
}

func TestWriterUpdateKeepsVarPlaceholders(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	w := config.Writer{Path: p}
	upd := config.ServerSpec{
		Name: "alpha",
		Type: "remote",
		Remote: &config.RemoteSpec{
			URL:       "https://alpha2.example.com/mcp",
			Transport: "http",
			Headers:   map[string]string{"Authorization": "Bearer ${TOK}"},
		},
	}
	if err := w.UpdateServer("alpha", upd); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	got := readFile(t, p)
	if !strings.Contains(got, "Bearer ${TOK}") {
		t.Errorf("placeholder not literal in file:\n%s", got)
	}
	if !strings.Contains(got, "alpha2.example.com") {
		t.Errorf("update not applied:\n%s", got)
	}
	if strings.Contains(got, "https://alpha.example.com") {
		t.Errorf("old value still present:\n%s", got)
	}
	// Unrelated content and the entry's position survive.
	for _, want := range []string{"# top comment", "# the second server", "# inline on public_url", "name: beta"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "name: alpha") > strings.Index(got, "name: beta") {
		t.Errorf("alpha moved after beta:\n%s", got)
	}
}

func TestWriterUpdateKeepsPosition(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	w := config.Writer{Path: p}
	upd := config.ServerSpec{
		Name:    "beta",
		Type:    "command",
		Command: &config.CommandSpec{Command: "npx", Args: []string{"-y", "other"}},
	}
	if err := w.UpdateServer("beta", upd); err != nil {
		t.Fatal(err)
	}
	cfg := loadAccept(t, p)
	if len(cfg.Servers) != 2 || cfg.Servers[0].Name != "alpha" || cfg.Servers[1].Name != "beta" {
		t.Fatalf("servers = %+v", cfg.Servers)
	}
	if cfg.Servers[1].Command.Args[1] != "other" {
		t.Errorf("args = %v", cfg.Servers[1].Command.Args)
	}
}

const mergeOriginal = `# head of servers
servers:
  # head comment on alpha
  - name: alpha # inline on alpha
    type: remote
    remote: # remote section comment
      url: https://alpha.example.com/mcp
      transport: http # http | sse | auto
      headers: { Authorization: "Bearer ${TOK}" } # flow headers
    container: # optional overrides
      image: docker.io/library/node:20
      volumes: ["/srv/a:/a:ro"]
  - name: beta
    type: remote
    remote:
      url: https://beta.example.com/mcp
`

func alphaMerged() config.ServerSpec {
	return config.ServerSpec{
		Name: "alpha",
		Type: "remote",
		Remote: &config.RemoteSpec{
			URL:       "https://alpha2.example.com/mcp",
			Transport: "http",
			Headers:   map[string]string{"Authorization": "Bearer ${TOK}"},
		},
		Container: &config.ContainerOverrides{
			Image:   "docker.io/library/node:20",
			Volumes: []string{"/srv/a:/a:ro"},
		},
	}
}

func TestWriterUpdatePreservesInEntryComments(t *testing.T) {
	p := newTestConfig(t, mergeOriginal)
	if err := (config.Writer{Path: p}).UpdateServer("alpha", alphaMerged()); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	for _, want := range []string{
		"# head comment on alpha",
		"- name: alpha # inline on alpha",
		"remote: # remote section comment",
		"transport: http # http | sse | auto",
		`headers: {Authorization: "Bearer ${TOK}"} # flow headers`, // yaml.v3 tightens flow-map spacing
		"container: # optional overrides",
		`volumes: ["/srv/a:/a:ro"]`,
		"url: https://alpha2.example.com/mcp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "https://alpha.example.com") {
		t.Errorf("old url still present:\n%s", got)
	}
	// Key order unchanged: name, type, remote, container; remote: url, transport, headers.
	idx := func(s string) int { return strings.Index(got, s) }
	order := []string{"name: alpha", "type: remote", "remote:", "url:", "transport:", "headers:", "container:", "name: beta"}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) >= idx(order[i]) {
			t.Errorf("order broken between %q and %q:\n%s", order[i-1], order[i], got)
		}
	}
}

func TestWriterUpdateRemovesClearedSection(t *testing.T) {
	p := newTestConfig(t, mergeOriginal)
	s := alphaMerged()
	s.Container = nil
	if err := (config.Writer{Path: p}).UpdateServer("alpha", s); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if strings.Contains(got, "container") || strings.Contains(got, "node:20") {
		t.Errorf("container not removed:\n%s", got)
	}
	if !strings.Contains(got, "transport: http # http | sse | auto") || !strings.Contains(got, "name: beta") {
		t.Errorf("other content lost:\n%s", got)
	}
}

func TestWriterUpdateAddsNewKey(t *testing.T) {
	p := newTestConfig(t, mergeOriginal)
	s := alphaMerged()
	s.Env = map[string]string{"A": "b"}
	if err := (config.Writer{Path: p}).UpdateServer("alpha", s); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if !strings.Contains(got, "env:") || !strings.Contains(got, "A: b") {
		t.Fatalf("env not added:\n%s", got)
	}
	if strings.Index(got, "env:") < strings.Index(got, "container:") || strings.Index(got, "env:") > strings.Index(got, "name: beta") {
		t.Errorf("env should be appended at end of alpha entry:\n%s", got)
	}
	if !strings.Contains(got, "container: # optional overrides") {
		t.Errorf("comment lost:\n%s", got)
	}
}

func TestWriterUpdateMissing(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	err := (config.Writer{Path: p}).UpdateServer("nope", gammaSpec())
	if err == nil || err.Error() != `config: server "nope" not found` {
		t.Errorf("err = %v", err)
	}
	if readFile(t, p) != writerOriginal {
		t.Error("file changed")
	}
}

func TestWriterDeleteRemovesOnlyThatServer(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	w := config.Writer{Path: p}
	if err := w.DeleteServer("alpha"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	got := readFile(t, p)
	if strings.Contains(got, "name: alpha") {
		t.Errorf("alpha still present:\n%s", got)
	}
	for _, want := range []string{"# top comment", "# inline on public_url", "name: beta", `args: ["-y", "some-server"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	cfg := loadAccept(t, p)
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "beta" {
		t.Errorf("servers = %+v", cfg.Servers)
	}
	if err := w.DeleteServer("alpha"); err == nil || err.Error() != `config: server "alpha" not found` {
		t.Errorf("second delete err = %v", err)
	}
}

func TestWriterRejectsInvalidWithoutTouchingFile(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	w := config.Writer{Path: p}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	dup := gammaSpec()
	dup.Name = "alpha"
	err := w.AddServer(dup)
	if err == nil || err.Error() != `config: server "alpha" already exists` {
		t.Errorf("dup err = %v", err)
	}

	bad := gammaSpec()
	bad.Name = "Bad Name"
	if err := w.AddServer(bad); err == nil {
		t.Error("invalid spec accepted")
	}
	if err := w.UpdateServer("alpha", config.ServerSpec{Name: "alpha", Type: "remote"}); err == nil {
		t.Error("invalid update accepted")
	}

	if readFile(t, p) != writerOriginal {
		t.Error("file content changed")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("mtime changed: %v != %v", fi.ModTime(), old)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind: %v", err)
	}
}

func TestWriterPreservesFileMode(t *testing.T) {
	p := newTestConfig(t, writerOriginal)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (config.Writer{Path: p}).AddServer(gammaSpec()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestWriterMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.yaml")
	if err := (config.Writer{Path: p}).AddServer(gammaSpec()); err == nil {
		t.Error("want error for missing file")
	}
}

func TestWriterTriggersSingleWatcherCallback(t *testing.T) {
	h := startWatcher(t, "listen: \":8080\"\n# keep me\nservers: []\n")
	spec := config.ServerSpec{
		Name:   "delta",
		Type:   "remote",
		Remote: &config.RemoteSpec{URL: "https://delta.example.com/mcp", Transport: "http"},
	}
	if err := (config.Writer{Path: h.path}).AddServer(spec); err != nil {
		t.Fatal(err)
	}
	c := h.expectChange(t).Config
	if len(c.Servers) != 1 || c.Servers[0].Name != "delta" {
		t.Errorf("servers = %+v", c.Servers)
	}
	h.expectNoChange(t)
}

func TestWriterConcurrentAddsAllLand(t *testing.T) {
	// Each Writer value edits the same file (the web layer builds one per
	// process, but nothing stops two): edits are serialized, none is lost,
	// and no temp file is left behind.
	p := newTestConfig(t, writerOriginal)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			s := gammaSpec()
			s.Name = fmt.Sprintf("srv-%02d", i)
			errs <- config.Writer{Path: p}.AddServer(s)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AddServer: %v", err)
		}
	}
	c := loadAccept(t, p)
	if len(c.Servers) != 2+n {
		t.Fatalf("servers = %d, want %d", len(c.Servers), 2+n)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("leftover file %s", e.Name())
		}
	}
}
