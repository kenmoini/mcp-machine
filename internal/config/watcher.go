package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceInterval is the quiet period after the last matching filesystem
// event before the config is reloaded. It is a variable (not a constant) so
// in-package tests can adjust it; the default is used otherwise.
var debounceInterval = 250 * time.Millisecond

// Watcher reloads a Layout when any of its files changes on disk: the base
// file, or a drop-in in the system or user directory.
type Watcher struct {
	layout   Layout // paths cleaned, so they compare equal to event names
	fsw      *fsnotify.Watcher
	onChange func(*Set)
	onError  func(error)

	// pending holds watched-for directories that did not exist yet, mapped
	// to the parent watched in their place ("" when the parent is missing
	// too). Only NewWatcher and Run touch it.
	pending map[string]string
	// watched holds every path added to fsw, with a count of the reasons it
	// is watched, so a parent watched only for pending directories can be
	// removed once they all exist.
	watched map[string]int
}

// NewWatcher returns a Watcher for the layout l. After each successful
// reload (Layout.Load followed by Validate) onChange is called with the new
// set; after a failed one onError is called and the previous config should
// stay in effect. A nil callback is treated as a no-op.
//
// The watcher watches the base file's directory, SystemDir and UserDir. A
// directory that does not exist yet is picked up when it is created, as long
// as its parent exists. NewWatcher starts watching immediately, so changes
// made after it returns are not missed even if Run has not yet been
// scheduled. The caller must either call Run (which closes the watcher when
// it returns) or Close.
func NewWatcher(l Layout, onChange func(*Set), onError func(error)) (*Watcher, error) {
	if l.BaseFile == "" && l.SystemDir == "" && l.UserDir == "" {
		return nil, errors.New("config: watcher has nothing to watch")
	}
	if onChange == nil {
		onChange = func(*Set) {}
	}
	if onError == nil {
		onError = func(error) {}
	}
	l.BaseFile = cleanPath(l.BaseFile)
	l.SystemDir = cleanPath(l.SystemDir)
	l.UserDir = cleanPath(l.UserDir)

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("config: create watcher: %w", err)
	}
	w := &Watcher{
		layout:   l,
		fsw:      fsw,
		onChange: onChange,
		onError:  onError,
		pending:  map[string]string{},
		watched:  map[string]int{},
	}
	// Watch directories rather than files so atomic renames and
	// temp-file-plus-rename writes are seen.
	var dirs []string
	if l.BaseFile != "" {
		dirs = append(dirs, filepath.Dir(l.BaseFile))
	}
	dirs = append(dirs, l.SystemDir, l.UserDir)
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if err := w.watchDir(dir); err != nil {
			_ = fsw.Close()
			return nil, err
		}
	}
	return w, nil
}

func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// watchDir watches dir, or records it as pending and watches its parent when
// dir does not exist yet.
func (w *Watcher) watchDir(dir string) error {
	err := w.add(dir)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	if err := w.add(parent); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent = ""
	}
	w.pending[dir] = parent
	// dir may have appeared before its parent was watched.
	w.resolvePending(dir)
	return nil
}

// add watches path, counting a reference when it is already watched.
func (w *Watcher) add(path string) error {
	if w.watched[path] == 0 {
		if err := w.fsw.Add(path); err != nil {
			return fmt.Errorf("config: watch %s: %w", path, err)
		}
	}
	w.watched[path]++
	return nil
}

// release drops one reference to path and stops watching it at zero.
func (w *Watcher) release(path string) {
	if w.watched[path] == 0 {
		return
	}
	w.watched[path]--
	if w.watched[path] == 0 {
		delete(w.watched, path)
		_ = w.fsw.Remove(path)
	}
}

// resolvePending watches the pending directory dir if it now exists and
// reports whether it did.
func (w *Watcher) resolvePending(dir string) bool {
	parent, ok := w.pending[dir]
	if !ok {
		return false
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	if err := w.add(dir); err != nil {
		return false // removed again, or unreadable: stay pending
	}
	delete(w.pending, dir)
	if parent != "" {
		w.release(parent)
	}
	return true
}

// relevant reports whether an event on name can change the loaded set: the
// base file itself, or a name in SystemDir or UserDir that listDropIns would
// pick (.yaml or .yml, not starting with "."). Pending directories are
// handled separately.
func (w *Watcher) relevant(name string) bool {
	l := w.layout
	if l.BaseFile != "" && name == l.BaseFile {
		return true
	}
	dir := filepath.Dir(name)
	if dir != l.SystemDir && dir != l.UserDir {
		return false
	}
	base := filepath.Base(name)
	if strings.HasPrefix(base, ".") {
		return false
	}
	ext := filepath.Ext(base)
	return ext == ".yaml" || ext == ".yml"
}

// Close releases the underlying filesystem watcher. It is safe to call more
// than once and after Run has returned.
func (w *Watcher) Close() error { return w.fsw.Close() }

// Reload loads and validates the layout, then calls onChange on success or
// onError on failure, and returns the error. An empty or whitespace-only
// file is an error; a missing base file is not (see Layout.Load). Reload
// runs on the caller's goroutine.
func (w *Watcher) Reload() error {
	set, err := w.load()
	if err != nil {
		w.onError(err)
		return err
	}
	w.onChange(set)
	return nil
}

func (w *Watcher) load() (*Set, error) {
	set, err := w.layout.Load()
	if err != nil {
		return nil, err
	}
	if err := Validate(set.Config); err != nil {
		return nil, err
	}
	return set, nil
}

// Run reloads the layout after a debounced burst of relevant events. It
// blocks until ctx is done and then returns nil; if the underlying fsnotify
// watcher fails it returns that error. The fsnotify watcher is closed before
// Run returns, so a Watcher cannot be run twice.
//
// The onChange and onError callbacks run on the goroutine that called Run, so
// a slow callback delays further reloads.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fsw.Close()

	timer := time.NewTimer(debounceInterval)
	timer.Stop()
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			name := filepath.Clean(ev.Name)
			// A pending directory that now exists may already hold
			// drop-ins written before it was watched, so reload.
			if w.resolvePending(name) || w.relevant(name) {
				timer.Reset(debounceInterval)
			}
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("config: watcher: %w", err)
		case <-timer.C:
			_ = w.Reload() // outcome is delivered through the callbacks
		}
	}
}
