package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

const (
	eventTimeout = 3 * time.Second
	quietTimeout = 500 * time.Millisecond
)

const validYAML = "listen: \":8080\"\nservers: []\n"

// watchHarness runs a Watcher over a temp config file and exposes its
// callbacks as channels.
type watchHarness struct {
	path    string
	changes chan *config.Config
	errs    chan error
}

// startWatcher writes initial content to a temp config file, starts a Watcher
// on it in a goroutine and registers cleanup that cancels it and waits for Run
// to return.
func startWatcher(t *testing.T, initial string) *watchHarness {
	t.Helper()
	h := &watchHarness{
		path:    filepath.Join(t.TempDir(), "config.yaml"),
		changes: make(chan *config.Config, 16),
		errs:    make(chan error, 16),
	}
	if err := os.WriteFile(h.path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := config.NewWatcher(h.path,
		func(c *config.Config) { h.changes <- c },
		func(err error) { h.errs <- err })
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v after cancel, want nil", err)
			}
		case <-time.After(eventTimeout):
			t.Error("Run did not return after context cancel")
		}
	})
	return h
}

func (h *watchHarness) expectChange(t *testing.T) *config.Config {
	t.Helper()
	select {
	case c := <-h.changes:
		return c
	case err := <-h.errs:
		t.Fatalf("got onError(%v), want onChange", err)
	case <-time.After(eventTimeout):
		t.Fatal("timed out waiting for onChange")
	}
	return nil
}

func (h *watchHarness) expectError(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.errs:
		return err
	case <-h.changes:
		t.Fatal("got onChange, want onError")
	case <-time.After(eventTimeout):
		t.Fatal("timed out waiting for onError")
	}
	return nil
}

func (h *watchHarness) expectNoChange(t *testing.T) {
	t.Helper()
	select {
	case <-h.changes:
		t.Fatal("onChange called unexpectedly")
	case <-time.After(quietTimeout):
	}
}

func TestWatcherReloadsOnOverwrite(t *testing.T) {
	h := startWatcher(t, validYAML)
	if err := os.WriteFile(h.path, []byte("listen: \":9090\"\nservers: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.expectChange(t).Listen; got != ":9090" {
		t.Errorf("Listen = %q, want :9090", got)
	}
}

func TestWatcherReloadsOnAtomicRename(t *testing.T) {
	h := startWatcher(t, validYAML)
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, []byte("listen: \":9191\"\nservers: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, h.path); err != nil {
		t.Fatal(err)
	}
	if got := h.expectChange(t).Listen; got != ":9191" {
		t.Errorf("Listen = %q, want :9191", got)
	}
}

func TestWatcherInvalidYAMLCallsOnErrorOnly(t *testing.T) {
	h := startWatcher(t, validYAML)
	if err := os.WriteFile(h.path, []byte("listen: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	h.expectError(t)
	h.expectNoChange(t)
}

func TestWatcherEmptyFileCallsOnErrorOnly(t *testing.T) {
	h := startWatcher(t, validYAML)
	if err := os.Truncate(h.path, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.expectError(t); err == nil || err.Error() != "config: file is empty" {
		t.Errorf("error = %v, want %q", err, "config: file is empty")
	}
	h.expectNoChange(t)
}

func TestWatcherIgnoresOtherFiles(t *testing.T) {
	h := startWatcher(t, validYAML)
	other := filepath.Join(filepath.Dir(h.path), "other.yaml")
	if err := os.WriteFile(other, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	h.expectNoChange(t)
	select {
	case err := <-h.errs:
		t.Fatalf("onError(%v) for unrelated file", err)
	default:
	}
}

func TestWatcherDebouncesBurst(t *testing.T) {
	h := startWatcher(t, validYAML)
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(h.path, []byte(validYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.expectChange(t)
	// A second reload within the window would be a debounce failure.
	h.expectNoChange(t)
}

func TestReloadReturnsValidationError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	dup := `
servers:
  - {name: a, type: remote, remote: {url: "http://x/mcp"}}
  - {name: a, type: remote, remote: {url: "http://y/mcp"}}
`
	if err := os.WriteFile(path, []byte(dup), 0o644); err != nil {
		t.Fatal(err)
	}
	var changed int
	var reported error
	w, err := config.NewWatcher(path,
		func(*config.Config) { changed++ },
		func(err error) { reported = err })
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()
	got := w.Reload()
	if got == nil {
		t.Fatal("Reload returned nil, want validation error")
	}
	if reported == nil || reported.Error() != got.Error() {
		t.Errorf("onError got %v, want %v", reported, got)
	}
	if changed != 0 {
		t.Errorf("onChange called %d times, want 0", changed)
	}
}

func TestReloadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var reported error
	w, err := config.NewWatcher(path, func(*config.Config) {}, func(err error) { reported = err })
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()
	if w.Reload() == nil || reported == nil {
		t.Error("Reload of a missing file must return and report an error")
	}
}
