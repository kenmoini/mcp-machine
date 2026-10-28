package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

const (
	eventTimeout = 3 * time.Second
	quietTimeout = 500 * time.Millisecond // 2× the 250 ms debounce interval
)

const validYAML = "listen: \":8080\"\nservers: []\n"

// watchHarness runs a Watcher over a Layout and exposes its callbacks as
// channels.
type watchHarness struct {
	path    string // the base file, when the layout has one
	changes chan *config.Set
	errs    chan error
}

// startWatcher writes initial content to a temp config file, starts a Watcher
// on it in a goroutine and registers cleanup that cancels it and waits for Run
// to return.
func startWatcher(t *testing.T, initial string) *watchHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeText(t, path, initial)
	return startLayoutWatcher(t, config.Layout{BaseFile: path})
}

// startLayoutWatcher starts a Watcher over l in a goroutine and registers
// cleanup that cancels it and waits for Run to return.
func startLayoutWatcher(t *testing.T, l config.Layout) *watchHarness {
	t.Helper()
	h := &watchHarness{
		path:    l.BaseFile,
		changes: make(chan *config.Set, 16),
		errs:    make(chan error, 16),
	}
	w, err := config.NewWatcher(l,
		func(s *config.Set) { h.changes <- s },
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

func writeText(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *watchHarness) expectChange(t *testing.T) *config.Set {
	t.Helper()
	select {
	case s := <-h.changes:
		return s
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

// expectQuiet fails if either callback runs within quietTimeout.
func (h *watchHarness) expectQuiet(t *testing.T) {
	t.Helper()
	select {
	case <-h.changes:
		t.Fatal("onChange called unexpectedly")
	case err := <-h.errs:
		t.Fatalf("onError(%v) called unexpectedly", err)
	case <-time.After(quietTimeout):
	}
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
	writeText(t, h.path, "listen: \":9090\"\nservers: []\n")
	if got := h.expectChange(t).Config.Listen; got != ":9090" {
		t.Errorf("Listen = %q, want :9090", got)
	}
}

func TestWatcherReloadsOnAtomicRename(t *testing.T) {
	h := startWatcher(t, validYAML)
	tmp := h.path + ".tmp"
	writeText(t, tmp, "listen: \":9191\"\nservers: []\n")
	if err := os.Rename(tmp, h.path); err != nil {
		t.Fatal(err)
	}
	if got := h.expectChange(t).Config.Listen; got != ":9191" {
		t.Errorf("Listen = %q, want :9191", got)
	}
}

func TestWatcherInvalidYAMLCallsOnErrorOnly(t *testing.T) {
	h := startWatcher(t, validYAML)
	writeText(t, h.path, "listen: [")
	_ = h.expectError(t)
	h.expectNoChange(t)
}

func TestWatcherEmptyFileCallsOnErrorOnly(t *testing.T) {
	h := startWatcher(t, validYAML)
	if err := os.Truncate(h.path, 0); err != nil {
		t.Fatal(err)
	}
	want := "config: " + h.path + ": file is empty"
	if err := h.expectError(t); err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	h.expectNoChange(t)
}

func TestWatcherIgnoresOtherFiles(t *testing.T) {
	h := startWatcher(t, validYAML)
	writeText(t, filepath.Join(filepath.Dir(h.path), "other.yaml"), validYAML)
	h.expectQuiet(t)
}

func TestWatcherDebouncesBurst(t *testing.T) {
	h := startWatcher(t, validYAML)
	for i := 0; i < 5; i++ {
		writeText(t, h.path, validYAML)
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
	writeText(t, path, dup)
	var changed int
	var reported error
	w, err := config.NewWatcher(config.Layout{BaseFile: path},
		func(*config.Set) { changed++ },
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

// A missing base file is not an error in a layered load: the set simply has
// no base, and the defaults apply.
func TestReloadMissingBaseFileLoadsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var got *config.Set
	var reported error
	w, err := config.NewWatcher(config.Layout{BaseFile: path},
		func(s *config.Set) { got = s },
		func(err error) { reported = err })
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()
	if err := w.Reload(); err != nil || reported != nil {
		t.Fatalf("Reload = %v, onError(%v); want success", err, reported)
	}
	if got == nil {
		t.Fatal("onChange not called")
	}
	if len(got.Files) != 0 {
		t.Errorf("Files = %v, want none", got.Files)
	}
	if got.Config.Listen != ":8080" {
		t.Errorf("Listen = %q, want default :8080", got.Config.Listen)
	}
}

func TestWatcherReloadsOnDropInCreate(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "config.yaml")
	sys := filepath.Join(root, "config.d")
	writeText(t, base, remoteYAML("a", ""))
	mkdir(t, sys)
	h := startLayoutWatcher(t, config.Layout{BaseFile: base, SystemDir: sys})

	writeText(t, filepath.Join(sys, "x.yaml"), remoteYAML("x", ""))
	s := h.expectChange(t)
	if got := serverNames(s.Config); len(got) != 2 || got[0] != "a" || got[1] != "x" {
		t.Errorf("servers = %v, want [a x]", got)
	}
}

func TestWatcherIgnoresTempAndHiddenNames(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "config.yaml")
	sys := filepath.Join(root, "config.d")
	writeText(t, base, validYAML)
	mkdir(t, sys)
	h := startLayoutWatcher(t, config.Layout{BaseFile: base, SystemDir: sys})

	writeText(t, filepath.Join(sys, ".x.yaml.swp"), "not: [yaml")
	tmp := filepath.Join(sys, "x.yaml.tmp")
	writeText(t, tmp, remoteYAML("x", ""))
	h.expectQuiet(t)

	if err := os.Rename(tmp, filepath.Join(sys, "x.yaml")); err != nil {
		t.Fatal(err)
	}
	s := h.expectChange(t)
	if got := serverNames(s.Config); len(got) != 1 || got[0] != "x" {
		t.Errorf("servers = %v, want [x]", got)
	}
	h.expectQuiet(t) // exactly one reload
}

func TestWatcherImportBurstReloadsOnce(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user.d")
	mkdir(t, user)
	var calls atomic.Int32
	w, err := config.NewWatcher(config.Layout{UserDir: user},
		func(*config.Set) { calls.Add(1) },
		func(err error) { t.Errorf("onError(%v)", err) })
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v after cancel, want nil", err)
		}
	}()

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeText(t, filepath.Join(user, name+".yaml"), remoteYAML(name, ""))
		time.Sleep(10 * time.Millisecond)
	}
	// Wait for the first reload, then for 3× the debounce interval more.
	deadline := time.Now().Add(eventTimeout)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(750 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Errorf("onChange called %d times, want 1", got)
	}
}

func TestWatcherLateUserDir(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "config.yaml")
	user := filepath.Join(root, "user.d")
	writeText(t, base, validYAML)
	h := startLayoutWatcher(t, config.Layout{BaseFile: base, UserDir: user})

	mkdir(t, user)
	writeText(t, filepath.Join(user, "a.yaml"), remoteYAML("a", ""))
	deadline := time.After(eventTimeout)
	for {
		select {
		case s := <-h.changes:
			if got := serverNames(s.Config); len(got) == 1 && got[0] == "a" {
				return
			}
		case err := <-h.errs:
			t.Fatalf("onError(%v)", err)
		case <-deadline:
			t.Fatal("timed out waiting for a reload with server a")
		}
	}
}

func TestWatcherInvalidDropInKeepsLastGood(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "config.yaml")
	sys := filepath.Join(root, "config.d")
	writeText(t, base, validYAML)
	mkdir(t, sys)
	h := startLayoutWatcher(t, config.Layout{BaseFile: base, SystemDir: sys})

	bad := filepath.Join(sys, "bad.yaml")
	writeText(t, bad, "listen: :1\n")
	if err := h.expectError(t); !strings.Contains(err.Error(), bad) {
		t.Errorf("error = %v, want it to name %s", err, bad)
	}
	h.expectNoChange(t)

	writeText(t, bad, remoteYAML("fixed", ""))
	s := h.expectChange(t)
	if got := serverNames(s.Config); len(got) != 1 || got[0] != "fixed" {
		t.Errorf("servers = %v, want [fixed]", got)
	}
}

func TestWatcherNothingToWatch(t *testing.T) {
	_, err := config.NewWatcher(config.Layout{}, nil, nil)
	if err == nil || err.Error() != "config: watcher has nothing to watch" {
		t.Errorf("err = %v, want %q", err, "config: watcher has nothing to watch")
	}
}

// A directory watched both in its own right and as the parent of a pending
// directory stays watched once the pending directory appears.
func TestWatcherSharedParentStaysWatched(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "late")
	h := startLayoutWatcher(t, config.Layout{
		BaseFile:  filepath.Join(baseDir, "config.yaml"),
		SystemDir: root,
	})

	mkdir(t, baseDir)
	h.expectChange(t) // the new directory triggers a reload

	writeText(t, filepath.Join(root, "x.yaml"), remoteYAML("x", ""))
	s := h.expectChange(t)
	if got := serverNames(s.Config); len(got) != 1 || got[0] != "x" {
		t.Errorf("servers = %v, want [x]", got)
	}
}

// A UI write (temp file plus rename into the user drop-in directory)
// produces exactly one reload: the hidden temp file is ignored.
func TestDirWriterTriggersSingleWatcherCallback(t *testing.T) {
	l := layoutDir(t, map[string]string{"config.yaml": validYAML})
	mkdir(t, l.UserDir)
	h := startLayoutWatcher(t, l)
	spec := config.ServerSpec{
		Name:   "delta",
		Type:   "remote",
		Remote: &config.RemoteSpec{URL: "https://delta.example.com/mcp", Transport: "http"},
	}
	if err := (config.DirWriter{Dir: l.UserDir, Layout: l}).AddServer(spec); err != nil {
		t.Fatal(err)
	}
	c := h.expectChange(t).Config
	if len(c.Servers) != 1 || c.Servers[0].Name != "delta" {
		t.Errorf("servers = %+v", c.Servers)
	}
	h.expectNoChange(t)
}
