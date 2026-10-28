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
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	deps, err := newManager(ctx, path, cfg, eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	s := &configState{path: path}
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

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Raw(); err == nil {
		t.Error("missing file: want error")
	}
}
