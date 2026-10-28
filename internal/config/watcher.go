package config

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceInterval is the quiet period after the last matching filesystem
// event before the config is reloaded. It is a variable (not a constant) so
// in-package tests can adjust it; the default is used otherwise.
var debounceInterval = 250 * time.Millisecond

// Watcher reloads a config file when it changes on disk.
type Watcher struct {
	path     string
	fsw      *fsnotify.Watcher
	onChange func(*Config)
	onError  func(error)
}

// NewWatcher returns a Watcher for the config file at path. After each
// successful reload (Load followed by Validate) onChange is called with the
// new config; after a failed one onError is called and the previous config
// should stay in effect. A nil callback is treated as a no-op.
//
// NewWatcher starts watching the config file's directory immediately, so
// changes made after it returns are not missed even if Run has not yet been
// scheduled. The caller must either call Run (which closes the watcher when
// it returns) or Close.
func NewWatcher(path string, onChange func(*Config), onError func(error)) (*Watcher, error) {
	if path == "" {
		return nil, errors.New("config: watcher path is empty")
	}
	if onChange == nil {
		onChange = func(*Config) {}
	}
	if onError == nil {
		onError = func(error) {}
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("config: create watcher: %w", err)
	}
	// Watch the directory rather than the file so atomic renames and
	// temp-file-plus-rename writes are seen.
	dir := filepath.Dir(path)
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("config: watch %s: %w", dir, err)
	}
	return &Watcher{path: path, fsw: fsw, onChange: onChange, onError: onError}, nil
}

// Close releases the underlying filesystem watcher. It is safe to call more
// than once and after Run has returned.
func (w *Watcher) Close() error { return w.fsw.Close() }

// Reload loads and validates the config file, then calls onChange on success
// or onError on failure, and returns the error. An empty or whitespace-only
// file is an error (see Load). Reload runs on the caller's
// goroutine.
func (w *Watcher) Reload() error {
	cfg, err := w.load()
	if err != nil {
		w.onError(err)
		return err
	}
	w.onChange(cfg)
	return nil
}

func (w *Watcher) load() (*Config, error) {
	cfg, err := Load(w.path)
	if err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Run reloads the config after a debounced burst of events touching the
// config file. It blocks until ctx is done and then returns nil; if the
// underlying fsnotify watcher fails it returns that error. The fsnotify
// watcher is closed before Run returns, so a Watcher cannot be run twice.
//
// The onChange and onError callbacks run on the goroutine that called Run, so
// a slow callback delays further reloads.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fsw.Close()

	base := filepath.Base(w.path)
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
			if filepath.Base(ev.Name) == base {
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
