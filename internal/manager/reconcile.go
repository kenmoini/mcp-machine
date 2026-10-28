package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// Apply makes cfg the current configuration and reconciles the runners with
// it. The desired set is the enabled servers, each identified by
// config.Hash(spec, spec.RuntimeImage(cfg)):
//
//   - a desired server without a runner gets one, which is started;
//   - a runner whose hash changed is removed (container included) and
//     replaced by a fresh one;
//   - a runner of a server that is in the config with enabled: false is
//     stopped without being paused, and a stopped, unpaused runner of an
//     enabled server is started again;
//   - a runner of a server no longer in the config is removed;
//   - every other runner is left alone: in particular a server the user
//     stopped (paused) stays stopped until its hash changes.
//
// The first Apply also adopts containers left by a previous process (see
// adopt) and, when MCP Machine runs in a container, joins it to the servers'
// network. If the containers cannot be listed, Apply returns the error
// without starting anything, and the next Apply scans again. Applying the
// same config twice makes no engine calls.
//
// Calls are serialized: a concurrent Apply waits for the one in progress. It
// is safe to call alongside Start, Stop and the other commands.
func (m *Manager) Apply(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("manager: nil config")
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	m.cfg = cfg
	m.mu.Unlock()

	specs := make([]config.ServerSpec, 0, len(cfg.Servers))
	inConfig := make(map[string]bool, len(cfg.Servers))
	desired := make(map[string]string, len(cfg.Servers)) // name → hash, enabled servers only
	for _, spec := range cfg.Servers {
		if inConfig[spec.Name] {
			continue // config validation rejects duplicates; keep the first
		}
		inConfig[spec.Name] = true
		specs = append(specs, spec)
		if spec.IsEnabled() {
			desired[spec.Name] = config.Hash(spec, spec.RuntimeImage(cfg))
		}
	}

	var (
		errMu sync.Mutex
		errs  []error
	)
	report := func(err error) {
		if err != nil {
			errMu.Lock()
			errs = append(errs, err)
			errMu.Unlock()
		}
	}

	m.mu.Lock()
	var gone []*runner
	owned := make(map[string]bool, len(m.runners))
	for name, r := range m.runners {
		owned[name] = true
		if !inConfig[name] {
			gone = append(gone, r)
		}
	}
	m.mu.Unlock()

	if !m.joined {
		m.joined = true
		m.joinNetwork(ctx, cfg)
	}
	var adopted map[string]runtime.ContainerInfo
	if !m.adopted {
		listed, err := m.adopt(ctx, cfg, desired, owned)
		if errors.Is(err, ErrAdoptionScan) {
			// Starting servers now could destroy containers that the next
			// scan would adopt: start nothing, and scan again next time.
			return err
		}
		m.adopted = true
		adopted = listed
		report(err)
	}

	// Servers are independent and stopping a container can take the whole
	// stop timeout, so they are reconciled concurrently.
	var wg sync.WaitGroup
	for _, r := range gone {
		wg.Go(func() {
			if err := m.removeRunner(ctx, r); err != nil && !errors.Is(err, ErrUnknownServer) {
				report(err)
			}
		})
	}
	for _, spec := range specs {
		var adopt *runtime.ContainerInfo
		if info, ok := adopted[spec.Name]; ok {
			adopt = &info
		}
		wg.Go(func() { report(m.reconcileServer(ctx, cfg, spec, desired[spec.Name], adopt)) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// reconcileServer brings one configured server in line with its spec. hash is
// its desired hash ("" when disabled); adopt is a container adopted for it.
func (m *Manager) reconcileServer(ctx context.Context, cfg *config.Config, spec config.ServerSpec, hash string, adopt *runtime.ContainerInfo) error {
	r, err := m.settledRunner(ctx, spec.Name)
	if err != nil {
		return err
	}
	if !spec.IsEnabled() {
		if r == nil || m.stoppedUnpaused(spec.Name) {
			return nil
		}
		return r.send(ctx, evDisable)
	}
	if r != nil && r.hash != hash {
		if err := m.removeRunner(ctx, r); err != nil && !errors.Is(err, ErrUnknownServer) {
			return err
		}
		if r, err = m.settledRunner(ctx, spec.Name); err != nil {
			return err
		}
	}
	if r != nil {
		if m.stoppedUnpaused(spec.Name) { // disabled until now
			return r.send(ctx, evStart)
		}
		return nil
	}
	return m.spawn(cfg, spec, adopt)
}

// stoppedUnpaused reports whether name's runner is stopped but not by the
// user: a server that was disabled.
func (m *Manager) stoppedUnpaused(name string) bool {
	st, ok := m.reg.Get(name)
	return ok && st.State == store.StateStopped && !st.Paused
}

// settledRunner returns name's runner, or nil if there is none. If a Remove
// of the runner is queued or in progress it first waits for the outcome: a
// runner on its way out must not be mistaken for a live one, or the server
// would be left without a runner.
func (m *Manager) settledRunner(ctx context.Context, name string) (*runner, error) {
	for {
		m.mu.Lock()
		closed := m.closed
		r := m.runners[name]
		m.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		if r == nil {
			return nil, nil
		}
		idle, pending := r.removalPending()
		if !pending {
			return r, nil
		}
		select {
		case <-r.done:
			// Removed: it forgot itself before ending. Look again.
			m.mu.Lock()
			closed, still := m.closed, m.runners[name] == r
			m.mu.Unlock()
			if closed {
				return nil, ErrClosed // Close ended it
			}
			if still {
				return nil, fmt.Errorf("manager: server %q: runner ended without being forgotten", name)
			}
		case <-idle: // the removal failed; the runner lives on
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// spawn creates and starts a runner for spec. With adopt, the runner starts
// from that existing container, as if resuming it after a Stop.
func (m *Manager) spawn(cfg *config.Config, spec config.ServerSpec, adopt *runtime.ContainerInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, ok := m.runners[spec.Name]; ok {
		return nil // only Apply creates runners, and Apply is serialized
	}
	r := newRunner(m, cfg, spec)
	if adopt != nil {
		// Not yet published: the start below moves on from here at once.
		r.status.State = store.StateStopped
		r.status.ContainerID = adopt.ID
		r.status.Image = adopt.Image
	}
	m.runners[spec.Name] = r
	r.events <- event{kind: evStart} // fresh buffered channel: never blocks
	go r.run()
	return nil
}

// ErrAdoptionScan is wrapped by the error Apply returns when the managed
// containers cannot be listed: the adoption scan did not happen, nothing was
// started, and Apply must be retried. Any other error from Apply means the
// configuration was applied and describes per-server failures.
var ErrAdoptionScan = errors.New("manager: adoption scan failed")

// adopt lists the managed containers left by a previous process. Containers
// of servers in owned (which already have a runner, from an Apply whose scan
// failed) are left to their runner. Of the others, a container
// whose server is desired with the same hash is kept for that server's
// runner: an http-mode one as it is (a running one is connected to without a
// restart), a stdio one stopped first if it is running, since a running
// container's stdio cannot be re-attached safely. Every other managed
// container (stale hash, disabled or unknown server, duplicate) is stopped
// and removed.
func (m *Manager) adopt(ctx context.Context, cfg *config.Config, desired map[string]string, owned map[string]bool) (map[string]runtime.ContainerInfo, error) {
	list, err := m.engine.ListContainers(ctx, map[string]string{store.LabelManaged: "true"})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAdoptionScan, err)
	}
	timeout := cfg.Podman.StopTimeout
	adopted := map[string]runtime.ContainerInfo{}
	var errs []error
	for _, info := range list {
		lbl, ok := store.ParseLabels(info.Labels)
		if !ok || owned[lbl.Name] {
			continue
		}
		hash, want := desired[lbl.Name]
		_, taken := adopted[lbl.Name]
		if want && !taken && lbl.Hash == hash && (lbl.Transport == "stdio" || lbl.Transport == "http") {
			keep := true
			if lbl.Transport == "stdio" && info.State == "running" {
				if err := m.engine.StopContainer(ctx, info.ID, timeout); err != nil {
					m.log.Warn("manager: cannot stop container to adopt it; replacing it", "server", lbl.Name, "container", info.ID, "err", err)
					keep = false
				} else {
					info.State = "exited"
				}
			}
			if keep {
				m.log.Info("manager: adopting container", "server", lbl.Name, "container", info.ID, "transport", lbl.Transport)
				adopted[lbl.Name] = info
				continue
			}
		}
		m.log.Info("manager: removing stale container", "server", lbl.Name, "container", info.ID)
		_ = m.engine.StopContainer(ctx, info.ID, timeout)
		if err := m.engine.RemoveContainer(ctx, info.ID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			errs = append(errs, fmt.Errorf("manager: remove stale container %s: %w", info.Name, err))
		}
	}
	return adopted, errors.Join(errs...)
}

// joinNetwork connects MCP Machine's own container to the servers' network,
// so http-mode servers are reachable by container name. Failures are logged:
// the servers can still run, only http-mode endpoints may be unreachable.
func (m *Manager) joinNetwork(ctx context.Context, cfg *config.Config) {
	network := cfg.Podman.Network
	if !m.opts.InContainer || m.opts.SelfID == "" || network == "" {
		return
	}
	if err := m.engine.EnsureNetwork(ctx, network); err != nil {
		m.log.Warn("manager: ensure network", "network", network, "err", err)
	}
	err := m.engine.ConnectNetwork(ctx, m.opts.SelfID, network)
	switch {
	case err == nil:
		m.log.Info("manager: joined network", "network", network, "container", m.opts.SelfID)
	case strings.Contains(strings.ToLower(err.Error()), "already"):
		m.log.Debug("manager: already on network", "network", network, "container", m.opts.SelfID)
	default:
		m.log.Warn("manager: cannot join network; http-mode servers may be unreachable", "network", network, "container", m.opts.SelfID, "err", err)
	}
}
