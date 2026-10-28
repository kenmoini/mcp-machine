package cli

import (
	"fmt"
	"os"
	"sync"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// configState is the serve process's view of its configuration: the config
// currently in effect, the outcome of the latest reload, and the reload
// counters. It is shared by the config watcher, the readiness probe, the
// metrics snapshot, and (later) the web layer.
type configState struct {
	mu         sync.RWMutex
	current    *config.Config
	lastErr    error
	reloads    uint64
	reloadErrs uint64
	applied    bool // the manager has applied a config at least once
	watcher    *config.Watcher
	path       string // the config file, re-read by Raw
}

// Current returns the configuration most recently loaded.
func (s *configState) Current() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// LastError returns the error of the latest failed reload, or nil when the
// latest reload succeeded (or none has happened).
func (s *configState) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// Reload re-reads the config file now, through the watcher's own path, so the
// outcome is recorded and applied exactly as a file change would be.
func (s *configState) Reload() error { return s.watcher.Reload() }

// Raw re-reads the config file with every ${VAR} left as written, so the
// result still carries the placeholders. It is the source for editing forms:
// Current holds secrets already expanded and must never be written back.
func (s *configState) Raw() (*config.Config, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", s.path, err)
	}
	return config.Parse(data, func(name string) (string, bool) { return "${" + name + "}", true })
}

// loaded records a successfully reloaded config.
func (s *configState) loaded(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = cfg
	s.lastErr = nil
	s.reloads++
}

// failed records a failed reload.
func (s *configState) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err
	s.reloadErrs++
}

// markApplied records that the manager has applied a config.
func (s *configState) markApplied() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = true
}

// isApplied reports whether the manager has applied a config.
func (s *configState) isApplied() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.applied
}

// counters returns the reload counters.
func (s *configState) counters() (reloads, errs uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reloads, s.reloadErrs
}
