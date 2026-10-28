package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/ops"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/fake"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// These tests exercise newManager without a listener: the ops handlers are
// driven in-process.

func newTestDeps(t *testing.T, eng *fake.Engine, body string) (serveDeps, string, *http.ServeMux) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := (&configFlags{path: path}).layout()
	if err != nil {
		t.Fatal(err)
	}
	set, err := loadSet(l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	deps, err := newManager(ctx, l, set, eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); deps.close() })
	mux := http.NewServeMux()
	ops.Mount(mux, deps.ready, deps.metrics)
	return deps, path, mux
}

func get(mux *http.ServeMux, target string) (int, string) {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Body.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewManagerReadyAfterApplyRetry(t *testing.T) {
	eng := fake.New()
	eng.FailNext("ListContainers", errors.New("engine hiccup"))
	_, _, mux := newTestDeps(t, eng, "listen: 127.0.0.1:0\n")

	if code, body := get(mux, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "config applied: false") {
		t.Fatalf("readyz before apply = %d %q", code, body)
	}
	waitFor(t, "readyz 200", func() bool { code, _ := get(mux, "/readyz"); return code == http.StatusOK })
}

func TestNewManagerRecordsReloadError(t *testing.T) {
	deps, path, mux := newTestDeps(t, fake.New(), "listen: 127.0.0.1:0\n")
	waitFor(t, "readyz 200", func() bool { code, _ := get(mux, "/readyz"); return code == http.StatusOK })

	if err := os.WriteFile(path, []byte("servers: [this is: not valid: yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reload error metric", func() bool {
		_, body := get(mux, "/metrics")
		return strings.Contains(body, "mcp_machine_config_reload_errors_total 1\n")
	})
	if deps.state.LastError() == nil {
		t.Error("LastError is nil after a failed reload")
	}

	// A valid file reloads through Reload() and clears the error.
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:0\nlog: {level: debug}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deps.state.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if deps.state.LastError() != nil || deps.state.Current().Log.Level != "debug" {
		t.Errorf("after Reload: err=%v level=%q", deps.state.LastError(), deps.state.Current().Log.Level)
	}
}

func TestNewManagerAppliedDespiteServerErrors(t *testing.T) {
	// A stale managed container that cannot be removed makes Apply return a
	// per-server error (not an adoption-scan failure): the config still
	// counts as applied and Apply is not retried.
	eng := fake.New()
	eng.Seed(runtime.ContainerInfo{
		ID:     "stale-1",
		Name:   store.ContainerName("gone"),
		Image:  "img:1",
		State:  "exited",
		Labels: store.Labels("gone", "oldhash", "stdio"),
	})
	eng.FailNext("RemoveContainer", errors.New("remove refused"))
	_, _, mux := newTestDeps(t, eng, "listen: 127.0.0.1:0\n")

	waitFor(t, "readyz 200", func() bool { code, _ := get(mux, "/readyz"); return code == http.StatusOK })
	n := 0
	for _, c := range eng.Calls() {
		if c == "ListContainers" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ListContainers called %d times, want 1 (no retry)", n)
	}
}

func TestConfigStateRawKeepsPlaceholders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "servers:\n  - name: x\n    type: remote\n    remote:\n      url: https://x.example/mcp\n      headers:\n        Authorization: Bearer ${NOT_SET_ANYWHERE}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &configState{layout: config.Layout{BaseFile: path}}
	raw, err := s.Raw()
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.Servers[0].Remote.Headers["Authorization"]; got != "Bearer ${NOT_SET_ANYWHERE}" {
		t.Errorf("header = %q", got)
	}
	if raw.Servers[0].Remote.Transport != "auto" {
		t.Errorf("defaults not applied: %+v", raw.Servers[0].Remote)
	}

	// A missing base file is no error for a layout (defaults), but a
	// broken one is.
	if err := os.WriteFile(path, []byte("servers: [this is: not valid: yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Raw(); err == nil {
		t.Error("broken file: want error")
	}
}

// writeFiles writes each body to dir/<relative path>, creating directories.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigStateOriginAndFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"config.yaml":     "servers:\n  - {name: a, type: remote, remote: {url: \"http://a/mcp\"}}\n",
		"config.d/b.yaml": "servers:\n  - {name: b, type: remote, remote: {url: \"http://b/mcp\"}}\n",
	})
	l, err := (&configFlags{path: root}).layout()
	if err != nil {
		t.Fatal(err)
	}
	set, err := loadSet(l)
	if err != nil {
		t.Fatal(err)
	}
	s := &configState{layout: l}
	s.loaded(set)

	if got := s.Files(); !slices.Equal(got, set.Files) || len(got) != 2 {
		t.Errorf("Files() = %v, want %v", got, set.Files)
	}
	if o, ok := s.Origin("a"); !ok || o.String() != "Defined in "+filepath.Join(root, "config.yaml")+" (base)" {
		t.Errorf("Origin(a) = %q, %v", o.String(), ok)
	}
	if _, ok := s.Origin("zz"); ok {
		t.Error("Origin(zz) found")
	}
	if s.Current() != set.Config {
		t.Error("Current() is not the loaded set's config")
	}
	if w := s.Warning(); w != "" {
		t.Errorf("Warning() = %q, want none", w)
	}
}

func TestConfigStateWarning(t *testing.T) {
	t.Run("deprecated location", func(t *testing.T) {
		root, data := t.TempDir(), t.TempDir()
		legacy := filepath.Join(data, "config.yaml")
		writeFiles(t, data, map[string]string{"config.yaml": "log: {level: debug}\n"})
		SetLegacyConfigPath(t, legacy)
		l, err := (&configFlags{path: root}).layout()
		if err != nil {
			t.Fatal(err)
		}
		set, err := loadSet(l)
		if err != nil {
			t.Fatal(err)
		}
		s := &configState{layout: l, set: set}
		want := "configuration read from the deprecated location " + legacy +
			"; move it to " + filepath.Join(root, "config.yaml")
		if got := s.Warning(); got != want {
			t.Errorf("Warning() = %q, want %q", got, want)
		}
	})
	t.Run("nothing found", func(t *testing.T) {
		root, user := t.TempDir(), t.TempDir()
		legacy := filepath.Join(t.TempDir(), "config.yaml")
		SetLegacyConfigPath(t, legacy)
		l, err := (&configFlags{path: root, userDir: user}).layout()
		if err != nil {
			t.Fatal(err)
		}
		set, err := loadSet(l)
		if err != nil {
			t.Fatal(err)
		}
		s := &configState{layout: l, set: set}
		want := "no configuration found (searched: " + filepath.Join(root, "config.yaml") + ", " +
			filepath.Join(root, "config.d") + ", " + legacy + ", " + user + "); running with defaults"
		if got := s.Warning(); got != want {
			t.Errorf("Warning() = %q, want %q", got, want)
		}
	})
}

func TestPrepareUserDir(t *testing.T) {
	t.Run("created and writable", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "config.d")
		l, notice := prepareUserDir(config.Layout{UserDir: dir})
		if notice != "" || l.UserDir != dir {
			t.Fatalf("notice=%q UserDir=%q", notice, l.UserDir)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("user dir not created: %v", err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("probe left files behind: %v", entries)
		}
	})
	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		writeFiles(t, filepath.Dir(file), map[string]string{"file": "x"})
		l, notice := prepareUserDir(config.Layout{UserDir: file})
		if !strings.HasPrefix(notice, "editing is disabled: "+file+" is not writable (") || !strings.HasSuffix(notice, ")") {
			t.Errorf("notice = %q", notice)
		}
		if l.UserDir != "" {
			t.Errorf("UserDir = %q, want the user layer dropped", l.UserDir)
		}
	})
	t.Run("no user layer", func(t *testing.T) {
		if l, notice := prepareUserDir(config.Layout{}); notice != "" || l.UserDir != "" {
			t.Errorf("notice=%q UserDir=%q", notice, l.UserDir)
		}
	})
}

func TestNewManagerJoinsNetworkWithoutIDLine(t *testing.T) {
	// A non-privileged container's /run/.containerenv has no id= line: the
	// ID is resolved from the host name through the engine and the manager
	// joins the servers' network with it.
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no host name: %v", err)
	}
	envFile := filepath.Join(t.TempDir(), "containerenv")
	if err := os.WriteFile(envFile, []byte("graphRootMounted=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := containerenvPath
	containerenvPath = envFile
	t.Cleanup(func() { containerenvPath = old })

	eng := fake.New()
	eng.Seed(runtime.ContainerInfo{ID: "self-0123456789", Name: host, State: "running"})
	_, _, mux := newTestDeps(t, eng, "listen: 127.0.0.1:0\n")
	waitFor(t, "readyz 200", func() bool { code, _ := get(mux, "/readyz"); return code == http.StatusOK })

	conns := eng.Connections()
	if len(conns) != 1 || conns[0] != [2]string{"self-0123456789", "mcp-machine"} {
		t.Fatalf("ConnectNetwork calls = %v, want [[self-0123456789 mcp-machine]]", conns)
	}
}

func TestNewManagerInContainerUnknownIDSkipsJoin(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "containerenv")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := containerenvPath
	containerenvPath = envFile
	t.Cleanup(func() { containerenvPath = old })
	t.Setenv(runtime.SelfIDEnv, "")

	eng := fake.New()
	_, _, mux := newTestDeps(t, eng, "listen: 127.0.0.1:0\n")
	waitFor(t, "readyz 200", func() bool { code, _ := get(mux, "/readyz"); return code == http.StatusOK })
	if conns := eng.Connections(); len(conns) != 0 {
		t.Fatalf("ConnectNetwork calls = %v, want none", conns)
	}
}
