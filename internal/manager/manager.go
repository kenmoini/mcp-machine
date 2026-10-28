// Package manager runs each configured MCP server as a container, keeps it
// alive, and binds a proxy facade to its MCP session for the HTTP adapters.
//
// Every server has one runner: a goroutine that owns the server's container,
// session and lifecycle state and processes events (commands from the
// Manager, container exits, session loss, retry timers) one at a time. Every
// state transition is written to the store.Registry, which is what the UI and
// Lookup read.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// ErrUnknownServer is returned (wrapped with the name) for a server the
// manager has no runner for.
var ErrUnknownServer = errors.New("manager: unknown server")

// ErrClosed is returned by every method once Close has been called.
var ErrClosed = errors.New("manager: closed")

// EndpointFunc returns the URL at which an http-mode container serves MCP.
// nil means DefaultEndpoint (Task 27).
type EndpointFunc func(info runtime.ContainerInfo, ep sources.HTTPEndpoint) (string, error)

// Builder builds an image from a build plan and returns its reference.
// *build.Builder satisfies it.
type Builder interface {
	Build(ctx context.Context, plan sources.BuildPlan, force bool) (string, error)
}

// Options tune the manager. Zero values select the documented defaults.
type Options struct {
	Clock        Clock         // default: the wall clock
	MaxRetries   int           // automatic retries after a failure before it is sticky; default 10, negative means none
	MaxBackoff   time.Duration // cap on the retry delay; default 60s
	ProbeTimeout time.Duration // http-mode readiness probe budget; default 60s
	InContainer  bool          // MCP Machine itself runs in a container
	SelfID       string        // MCP Machine's own container ID, when InContainer
	EndpointFunc EndpointFunc  // default: DefaultEndpoint
	Logger       *slog.Logger  // default: slog.Default()
	Version      string        // facade server version; default "dev"
}

// Manager owns one runner per configured server.
type Manager struct {
	engine   runtime.Engine
	resolver sources.Resolver
	builder  Builder
	reg      *store.Registry
	opts     Options
	log      *slog.Logger

	mu      sync.Mutex // guards the fields below; never held while waiting on a runner
	cfg     *config.Config
	runners map[string]*runner
	closed  bool
}

// New returns a manager. builder may be nil when no server uses a build plan.
func New(engine runtime.Engine, resolver sources.Resolver, builder Builder, reg *store.Registry, opts Options) *Manager {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 10
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 60 * time.Second
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = 60 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	return &Manager{
		engine:   engine,
		resolver: resolver,
		builder:  builder,
		reg:      reg,
		opts:     opts,
		log:      opts.Logger,
		runners:  map[string]*runner{},
	}
}

// Apply makes cfg the current configuration: every enabled server without a
// runner gets one and is started, and runners whose server is no longer in
// the config are removed (container included). Runners that already exist
// are left alone, so applying the same config twice changes nothing.
//
// Task 26 replaces the body of the per-server loop with full reconciliation
// (hash changes, adoption of existing containers, enabled: false, paused).
func (m *Manager) Apply(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("manager: nil config")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	m.cfg = cfg
	inConfig := make(map[string]bool, len(cfg.Servers))
	for _, spec := range cfg.Servers {
		inConfig[spec.Name] = true
		if !spec.IsEnabled() {
			continue // Task 26: enabled: false stops an existing runner
		}
		if _, ok := m.runners[spec.Name]; ok {
			continue // Task 26: hash change → Remove then recreate
		}
		r := newRunner(m, cfg, spec)
		m.runners[spec.Name] = r
		r.events <- event{kind: evStart} // fresh buffered channel: never blocks
		go r.run()
	}
	var gone []string
	for name := range m.runners {
		if !inConfig[name] {
			gone = append(gone, name)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, name := range gone {
		if err := m.Remove(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// config returns the most recently applied configuration.
func (m *Manager) config() *config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// runner returns the runner for name.
func (m *Manager) runner(name string) (*runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	r, ok := m.runners[name]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownServer, name)
	}
	return r, nil
}

// command sends kind to name's runner and waits for its answer.
func (m *Manager) command(ctx context.Context, name string, kind evKind) error {
	r, err := m.runner(name)
	if err != nil {
		return err
	}
	return r.send(ctx, kind)
}

// Start starts a stopped or failed server and clears its paused flag and
// retry counter. It returns once the runner has accepted the command;
// progress and failures are reported through the registry.
func (m *Manager) Start(ctx context.Context, name string) error {
	return m.command(ctx, name, evStart)
}

// Stop stops a server's container (keeping it for a fast restart) and marks
// the server paused. It returns once the server is Stopped.
func (m *Manager) Stop(ctx context.Context, name string) error {
	return m.command(ctx, name, evStop)
}

// Restart stops and starts a server; the server is not left paused.
func (m *Manager) Restart(ctx context.Context, name string) error {
	return m.command(ctx, name, evRestart)
}

// Remove stops a server, removes its container and forgets it.
func (m *Manager) Remove(ctx context.Context, name string) error {
	r, err := m.runner(name)
	if err != nil {
		return err
	}
	if err := r.send(ctx, evRemove); err != nil {
		return err
	}
	m.mu.Lock()
	if m.runners[name] == r {
		delete(m.runners, name)
	}
	m.mu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Rebuild force-rebuilds a build-type server's image and restarts it on a
// fresh container. Other server types return an error.
func (m *Manager) Rebuild(ctx context.Context, name string) error {
	return m.command(ctx, name, evRebuild)
}

// Resync re-reads the backend's lists into the server's facade.
func (m *Manager) Resync(ctx context.Context, name string) error {
	r, err := m.runner(name)
	if err != nil {
		return err
	}
	return r.facade.Resync(ctx)
}

// Facade returns the server's proxy facade, whatever its state.
func (m *Manager) Facade(name string) (*proxy.Facade, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[name]
	if !ok {
		return nil, false
	}
	return r.facade, true
}

// Lookup returns the server's facade server, only while it is Ready.
func (m *Manager) Lookup(name string) (*mcp.Server, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[name]
	if !ok {
		return nil, false
	}
	if st, ok := m.reg.Get(name); !ok || st.State != store.StateReady {
		return nil, false
	}
	return r.facade.Server(), true
}

// Logs returns the last tail lines of the server's container output.
func (m *Manager) Logs(ctx context.Context, name string, tail int) ([]runtime.LogLine, error) {
	m.mu.Lock()
	_, ok := m.runners[name]
	st, _ := m.reg.Get(name)
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownServer, name)
	}
	if st.ContainerID == "" {
		return nil, fmt.Errorf("manager: server %q has no container", name)
	}
	return m.engine.Logs(ctx, st.ContainerID, tail)
}

// Close stops every runner, dropping backend sessions but leaving containers
// running so the next start can adopt them. It waits for the runners until
// ctx is done, then abandons the stragglers.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	rs := make([]*runner, 0, len(m.runners))
	for _, r := range m.runners {
		rs = append(rs, r)
	}
	m.runners = map[string]*runner{}
	m.mu.Unlock()

	for _, r := range rs {
		r.interrupt()
		go r.post(event{kind: evShutdown})
	}
	for _, r := range rs {
		select {
		case <-r.done:
		case <-ctx.Done():
			for _, r := range rs {
				r.cancel()
			}
			return fmt.Errorf("manager: close: %w", ctx.Err())
		}
	}
	return nil
}
