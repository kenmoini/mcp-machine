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
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/vault"
)

// ErrUnknownServer is returned (wrapped with the name) for a server the
// manager has no runner for.
var ErrUnknownServer = errors.New("manager: unknown server")

// ErrClosed is returned by every method once Close has been called.
var ErrClosed = errors.New("manager: closed")

// EndpointFunc returns the URL at which an http-mode container serves MCP,
// given the container as inspected after it started.
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
	// Vault resolves ${vault:...} references. Nil means no Vault is
	// configured: a server that references one fails to start.
	Vault vault.Reader
}

// Manager owns one runner per configured server.
type Manager struct {
	engine   runtime.Engine
	resolver sources.Resolver
	builder  Builder
	reg      *store.Registry
	opts     Options
	log      *slog.Logger
	vault    vault.Reader // nil when not configured

	// reconcileMu serializes Apply. It is held while Apply waits on runners;
	// nothing else takes it, so commands (Start, Stop, ...) never wait for it.
	reconcileMu sync.Mutex
	adopted     bool // the adoption scan has listed the containers; guarded by reconcileMu
	joined      bool // the network join has been attempted; guarded by reconcileMu

	keepUnknown atomic.Bool // leave labeled containers of unconfigured servers alone; see SetKeepUnknown

	mu      sync.Mutex // guards the fields below; never held while waiting on a runner
	cfg     *config.Config
	runners map[string]*runner
	closed  bool
}

// SetKeepUnknown makes Apply leave labeled containers of servers that are
// not in the configuration alone (logged at info) instead of removing them.
// Stale containers of known servers are still replaced. It guards against a
// missing configuration mount removing running servers, and takes effect at
// the next Apply that performs the adoption scan.
func (m *Manager) SetKeepUnknown(keep bool) { m.keepUnknown.Store(keep) }

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
	if opts.EndpointFunc == nil {
		opts.EndpointFunc = DefaultEndpoint(opts)
	}
	return &Manager{
		engine:   engine,
		resolver: resolver,
		builder:  builder,
		reg:      reg,
		opts:     opts,
		log:      opts.Logger,
		vault:    opts.Vault,
		runners:  map[string]*runner{},
	}
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
	return m.removeRunner(ctx, r)
}

// removeRunner removes r and waits for it to end. Once queued the removal
// completes even if ctx ends: the runner forgets itself (see forget), so the
// name never points at a dead runner. While it is pending Apply waits for it
// rather than mistaking the runner for a live one (see settledRunner).
func (m *Manager) removeRunner(ctx context.Context, r *runner) error {
	r.beginRemoval()
	if err := r.send(ctx, evRemove); err != nil {
		return err
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Rebuild force-rebuilds a build-type server's image and restarts it on a
// fresh container. Other server types return an error without disturbing
// the server.
func (m *Manager) Rebuild(ctx context.Context, name string) error {
	r, err := m.runner(name)
	if err != nil {
		return err
	}
	if r.spec.Type != "build" {
		return fmt.Errorf("manager: server %q is of type %q; only build servers can be rebuilt", name, r.spec.Type)
	}
	return r.send(ctx, evRebuild)
}

// forget drops a removed runner and its registry entry in one step, so a
// runner Apply creates for the same name never has its entry deleted.
func (m *Manager) forget(r *runner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.runners[r.name]; ok && cur != r {
		return // a newer runner owns the name and its entry
	}
	delete(m.runners, r.name)
	m.reg.Delete(r.name)
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
	r, ok := m.runners[name]
	st, _ := m.reg.Get(name)
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownServer, name)
	}
	if r.spec.Type == "remote" {
		return nil, fmt.Errorf("manager: server %q: remote server has no logs", name)
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
