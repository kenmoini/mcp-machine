package cli

import (
	"slices"
	"sync"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// configState is the serve process's view of its configuration: the set of
// files currently in effect, the outcome of the latest reload, and the
// reload counters. It is shared by the config watcher, the readiness probe,
// the metrics snapshot, and the web layer.
type configState struct {
	mu         sync.RWMutex
	layout     config.Layout // where the configuration is read from; re-read by Raw
	set        *config.Set   // the last set loaded successfully
	lastErr    error
	reloads    uint64
	reloadErrs uint64
	applied    bool // the manager has applied a config at least once
	watcher    *config.Watcher
	applyMu    sync.Mutex // pairs the manager's keep-unknown flag with the Apply that reads it
}

// Current returns the configuration most recently loaded.
func (s *configState) Current() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.set == nil {
		return nil
	}
	return s.set.Config
}

// currentSet returns the set most recently loaded.
func (s *configState) currentSet() *config.Set {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.set
}

// Files returns the files the current configuration was read from, in load
// order.
func (s *configState) Files() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.set == nil {
		return nil
	}
	return slices.Clone(s.set.Files)
}

// Origin reports which files define the named server in the current
// configuration.
func (s *configState) Origin(name string) (config.Origin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.set == nil {
		return config.Origin{}, false
	}
	o, ok := s.set.Origins[name]
	return o, ok
}

// Warning returns the notice about where the current configuration came
// from (the deprecated location, or nowhere), or "".
func (s *configState) Warning() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return configWarning(s.set)
}

// LastError returns the error of the latest failed reload, or nil when the
// latest reload succeeded (or none has happened).
func (s *configState) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// Reload re-reads the configuration now, through the watcher, so the
// outcome is recorded and applied exactly as a file change would be.
func (s *configState) Reload() error { return s.watcher.Reload() }

// Raw re-reads every layer with each ${VAR} left as written, so the result
// still carries the placeholders. It is the source for editing forms:
// Current holds secrets already expanded and must never be written back.
func (s *configState) Raw() (*config.Config, error) {
	set, err := s.layout.LoadRaw()
	if err != nil {
		return nil, err
	}
	return set.Config, nil
}

// loaded records a successfully reloaded set.
func (s *configState) loaded(set *config.Set) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = set
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
