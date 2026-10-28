package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// eventBuffer is the capacity of a runner's event channel.
const eventBuffer = 16

// exitLogLines is how many trailing log lines a container-exit error quotes.
const exitLogLines = 5

// stableReady is how long a server must have been Ready for its next failure
// to start a fresh retry budget.
const stableReady = 60 * time.Second

type evKind int

const (
	evStart evKind = iota
	evStop
	evRestart
	evRemove
	evRebuild
	evDisable     // enabled: false in the config: stop without pausing
	evShutdown    // manager closing: drop the session, leave the container
	evExit        // the container of generation gen exited with code
	evSessionLost // the session of generation gen ended
	evRecover     // a preempting command was withdrawn: resume abandoned work
)

// preempts reports whether a command must interrupt whatever the runner is
// doing (a dial to an unresponsive backend, a long build) instead of queueing
// behind it.
func (k evKind) preempts() bool {
	switch k {
	case evStop, evRestart, evRemove, evRebuild, evDisable, evShutdown:
		return true
	}
	return false
}

type event struct {
	kind  evKind
	gen   uint64     // evExit, evSessionLost: the container/session generation
	code  int        // evExit
	reply chan error // commands: buffered(1); nil when nobody waits
}

// runner drives one server through its lifecycle. Everything below "owned by
// the runner goroutine" is touched only by run and the methods it calls.
type runner struct {
	m      *Manager
	name   string
	spec   config.ServerSpec
	hash   string
	facade *proxy.Facade

	events chan event
	ctx    context.Context // runner lifetime; cancelled when run returns or Close gives up
	cancel context.CancelFunc
	done   chan struct{} // closed when run returns

	// Interrupt bookkeeping shared with the manager's goroutines.
	mu            sync.Mutex
	pending       int                // preempting commands sent but not yet handled
	busy          bool               // an event or retry is being handled (a pipeline may be running)
	attemptCancel context.CancelFunc // cancels the current attempt's context
	removals      int                // Remove calls queued or being handled
	removalIdle   chan struct{}      // closed when removals drops to 0; nil until someone waits

	// Owned by the runner goroutine.
	status   store.Status
	resolved *sources.Resolved // last successful resolve
	image    string            // image ref of the last successful image step
	gen      uint64            // bumped when a container starts, is stopped or is discarded
	sess     *mcp.ClientSession
	rwc      io.ReadWriteCloser
	retries  int // consecutive automatic retries; reset by Start and by a stable Ready (see fail)
	// readySince is when the server last became Ready.
	readySince time.Time
	retryC     <-chan time.Time // pending retry timer; nil when none
	// awaitExit: the last failure happened before the container's exit was
	// visible; an exit of the same generation replaces the error.
	awaitExit bool
	// abandoned: an attempt was cancelled by an interrupt and left the state
	// mid-pipeline. The preempting command that caused it settles the state;
	// if that command turns out to be a no-op, the attempt is re-run.
	abandoned bool
}

func newRunner(m *Manager, cfg *config.Config, spec config.ServerSpec) *runner {
	ctx, cancel := context.WithCancel(context.Background())
	hash := config.Hash(spec, spec.RuntimeImage(cfg))
	r := &runner{
		m:      m,
		name:   spec.Name,
		spec:   spec,
		hash:   hash,
		facade: proxy.New(spec.Name, m.opts.Version),
		events: make(chan event, eventBuffer),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		status: store.Status{Name: spec.Name, Hash: hash},
	}
	r.set(store.StateDefined, "")
	return r
}

// --- cross-goroutine API -----------------------------------------------------

// interrupt marks a preempting command as in flight. If the runner is busy
// (possibly blocked in a dial or build) the current attempt is cancelled;
// until the command is handled, any attempt the runner begins is born
// cancelled, so the command is never stuck behind a blocking operation. An
// idle runner's attempt (the live session of a Ready server) is left alone:
// the command itself decides whether to tear it down.
func (r *runner) interrupt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending++
	if r.busy && r.attemptCancel != nil {
		r.attemptCancel()
	}
}

// setBusy records whether the runner goroutine is handling something.
func (r *runner) setBusy(b bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = b
}

func (r *runner) uninterrupt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending--
}

func (r *runner) interrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending > 0
}

// beginRemoval records a Remove about to be sent. It is ended by the runner
// exiting (a successful removal), or by endRemoval when the removal fails or
// is never queued.
func (r *runner) beginRemoval() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removals++
}

func (r *runner) endRemoval() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removals--
	if r.removals == 0 && r.removalIdle != nil {
		close(r.removalIdle)
		r.removalIdle = nil
	}
}

// removalPending reports whether a Remove is queued or in progress and, if
// so, returns a channel closed when none is (the runner may exit instead:
// watch done too).
func (r *runner) removalPending() (<-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removals == 0 {
		return nil, false
	}
	if r.removalIdle == nil {
		r.removalIdle = make(chan struct{})
	}
	return r.removalIdle, true
}

// post delivers an event without ever blocking the runner: it waits for room
// in the channel or for the runner to end, so nothing is dropped or leaked.
func (r *runner) post(ev event) {
	select {
	case r.events <- ev:
	case <-r.ctx.Done():
	}
}

// send delivers a command and waits for the runner's answer. Once the
// command is queued it runs to completion even if ctx ends first.
func (r *runner) send(ctx context.Context, kind evKind) error {
	if kind.preempts() {
		r.interrupt()
	}
	ev := event{kind: kind, reply: make(chan error, 1)}
	select {
	case r.events <- ev: // queue it if there is room, whatever the state of ctx
	default:
		select {
		case r.events <- ev:
		case <-r.done:
			r.withdraw(kind)
			return r.gone()
		case <-ctx.Done():
			r.withdraw(kind)
			return ctx.Err()
		}
	}
	select {
	case err := <-ev.reply:
		return err
	case <-r.done:
		select {
		case err := <-ev.reply:
			return err
		default:
			return r.gone()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// withdraw undoes the interrupt of a command that was never queued. The
// interrupt may already have cancelled an attempt, so the runner is asked to
// resume it.
func (r *runner) withdraw(kind evKind) {
	if kind == evRemove {
		r.endRemoval()
	}
	if !kind.preempts() {
		return
	}
	r.uninterrupt()
	go r.post(event{kind: evRecover})
}

func (r *runner) gone() error { return fmt.Errorf("%w %q", ErrUnknownServer, r.name) }

// --- runner goroutine ----------------------------------------------------------

func (r *runner) run() {
	defer close(r.done)
	defer r.cancel()
	for {
		select {
		case ev := <-r.events:
			r.setBusy(true)
			exit := r.handle(ev)
			r.setBusy(false)
			if exit {
				return
			}
		case <-r.retryC:
			r.setBusy(true)
			r.retryC = nil
			r.retry()
			r.setBusy(false)
		case <-r.ctx.Done():
			r.teardown()
			return
		}
	}
}

func reply(ev event, err error) {
	if ev.reply != nil {
		ev.reply <- err
	}
}

// handle processes one event and reports whether the runner should exit.
func (r *runner) handle(ev event) bool {
	if ev.kind.preempts() {
		r.uninterrupt()
	}
	switch ev.kind {
	case evStart:
		reply(ev, nil)
		r.start()
	case evStop:
		reply(ev, r.stop(true))
	case evRestart:
		err := r.stop(false)
		reply(ev, err)
		if err == nil {
			r.start()
		}
	case evRebuild:
		res, err := r.rebuildPlan()
		if err == nil {
			r.retries = 0
			err = r.stop(false)
		}
		reply(ev, err)
		if err == nil {
			r.provision(r.beginAttempt(), res, true)
		} else {
			r.recoverAbandoned() // rejected: nothing settled the interrupted attempt
		}
	case evDisable:
		if r.status.State == store.StateStopped {
			// Already stopped (paused by the user): only the flag changes.
			r.status.Paused = false
			r.m.reg.Set(r.status)
			reply(ev, nil)
			break
		}
		reply(ev, r.stop(false))
	case evRemove:
		err := r.remove()
		if err != nil {
			r.endRemoval() // the runner lives on
		}
		reply(ev, err)
		return err == nil
	case evShutdown:
		r.retryC = nil
		r.teardown()
		reply(ev, nil)
		return true
	case evExit:
		r.onExit(ev)
	case evSessionLost:
		r.onSessionLost(ev)
	case evRecover:
		r.recoverAbandoned()
	}
	return false
}

// recoverAbandoned re-runs an attempt that an interrupt cancelled when no
// preempting command is left to settle the state.
func (r *runner) recoverAbandoned() {
	if !r.abandoned || r.interrupted() {
		return
	}
	r.abandoned = false
	r.launch(false)
}

// set records a state transition in the registry.
func (r *runner) set(state store.State, errMsg string) {
	if state == store.StateReady && r.status.State != store.StateReady {
		r.readySince = r.m.opts.Clock.Now()
	}
	r.status.State = state
	r.status.Error = errMsg
	r.status.Since = r.m.opts.Clock.Now()
	r.m.reg.Set(r.status)
	if errMsg != "" {
		r.m.log.Warn("manager: server failed", "server", r.name, "err", errMsg, "restarts", r.status.Restarts)
	} else {
		r.m.log.Debug("manager: server state", "server", r.name, "state", state)
	}
}

// beginAttempt returns a context for one run of the container: it carries
// the pipeline, the dial and the session, and is cancelled by teardown or an
// interrupt. It is born cancelled while a preempting command is pending.
func (r *runner) beginAttempt() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attemptCancel != nil {
		r.attemptCancel()
	}
	ctx, cancel := context.WithCancel(r.ctx)
	if r.pending > 0 {
		cancel()
	}
	r.attemptCancel = cancel
	return ctx
}

// teardown drops the backend session and cancels the current attempt. The
// container is left alone.
func (r *runner) teardown() {
	r.mu.Lock()
	if r.attemptCancel != nil {
		r.attemptCancel()
		r.attemptCancel = nil
	}
	r.mu.Unlock()
	r.facade.Unbind()
	if r.rwc != nil {
		_ = r.rwc.Close()
		r.rwc = nil
	}
	if r.sess != nil {
		_ = r.sess.Close()
		r.sess = nil
	}
}

// fail tears down, moves to Failed and, unless the retry cap is reached,
// schedules a retry. A server that was Ready for at least stableReady fails
// as a new incident: its retry budget starts over. The timer is armed before
// the status is published so an observer of Failed can already advance a
// manual clock past it.
func (r *runner) fail(err error) {
	if r.status.State == store.StateReady && r.m.opts.Clock.Now().Sub(r.readySince) >= stableReady {
		r.retries = 0
	}
	r.teardown()
	r.awaitExit = false
	r.abandoned = false
	r.retryC = nil
	if r.retries < r.m.opts.MaxRetries {
		r.retryC = r.m.opts.Clock.After(backoff(r.retries, r.m.opts.MaxBackoff))
	}
	r.set(store.StateFailed, err.Error())
}

// failAttempt is fail for errors inside a start pipeline. An interrupted
// attempt is abandoned quietly: the preempting command decides the state
// (and recoverAbandoned re-runs it if that command is a no-op).
func (r *runner) failAttempt(ctx context.Context, err error) {
	if ctx.Err() != nil {
		r.teardown()
		r.abandoned = true
		return
	}
	r.fail(err)
}

// start handles Start: reset the retry budget and the paused flag, then
// resume the stopped container if it is still ours, else run the full
// pipeline.
func (r *runner) start() {
	r.retries = 0
	r.retryC = nil
	r.status.Paused = false
	switch r.status.State {
	case store.StateReady:
		r.m.reg.Set(r.status)
		return
	case store.StateStopped:
		if info, ok := r.resumable(); ok {
			ctx := r.beginAttempt()
			if info.Labels[store.LabelTransport] == "http" {
				r.resumeHTTP(ctx, info.State == "running")
				return
			}
			r.set(store.StateCreating, "")
			r.attachAndStart(ctx)
			return
		}
	}
	r.launch(false)
}

// resumable reports whether the stopped container still exists and was
// created for the current spec, and returns its current state. A container
// adopted by Apply is resumed the same way.
func (r *runner) resumable() (runtime.ContainerInfo, bool) {
	if r.status.ContainerID == "" {
		return runtime.ContainerInfo{}, false
	}
	info, err := r.m.engine.InspectContainer(r.ctx, r.status.ContainerID)
	if err != nil {
		return runtime.ContainerInfo{}, false
	}
	return info, info.Labels[store.LabelHash] == r.hash
}

// retry runs after a backoff: from Creating when the image is known,
// otherwise from the top.
func (r *runner) retry() {
	r.retries++
	r.status.Restarts++
	if r.resolved != nil && r.image != "" {
		r.create(r.beginAttempt())
		return
	}
	r.launch(false)
}

// launch runs the whole pipeline: Resolve → image → create → start → dial →
// bind (a remote server: Resolve → dial → bind).
func (r *runner) launch(forceBuild bool) {
	ctx := r.beginAttempt()
	r.set(store.StateResolving, "")
	res, err := r.m.resolver.Resolve(ctx, r.m.config(), r.spec)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("resolve: %w", err))
		return
	}
	r.provision(ctx, res, forceBuild)
}

// provision connects a remote server, or makes the image available and
// creates the container.
func (r *runner) provision(ctx context.Context, res sources.Resolved, forceBuild bool) {
	switch {
	case res.Kind == sources.KindRemote && res.Remote != nil:
		r.resolved = &res
		r.image = ""
		r.connectRemote(ctx)
		return
	case res.Kind == sources.KindContainer && res.Container != nil &&
		(res.Transport == "stdio" || (res.Transport == "http" && res.HTTP != nil)):
	default:
		r.failAttempt(ctx, fmt.Errorf("manager: unsupported server (kind %d, transport %q)", res.Kind, res.Transport))
		return
	}
	r.resolved = &res
	r.image = ""
	image, err := r.ensureImage(ctx, res, forceBuild)
	if err != nil {
		r.failAttempt(ctx, err)
		return
	}
	r.image = image
	r.status.Image = image
	r.set(store.StateImageReady, "")
	r.create(ctx)
}

// create replaces any previous container with a fresh one and starts it.
func (r *runner) create(ctx context.Context) {
	r.set(store.StateCreating, "")
	if err := r.discardContainer(ctx); err != nil {
		r.failAttempt(ctx, err)
		return
	}
	cfg := r.m.config()
	if cfg.Podman.Network != "" {
		if err := r.m.engine.EnsureNetwork(ctx, cfg.Podman.Network); err != nil {
			r.failAttempt(ctx, fmt.Errorf("ensure network %s: %w", cfg.Podman.Network, err))
			return
		}
	}
	transport := r.resolved.Transport
	spec := *r.resolved.Container
	if spec.Name == "" {
		spec.Name = store.ContainerName(r.name)
	}
	if spec.Network == "" {
		spec.Network = cfg.Podman.Network
	}
	spec.Image = r.image
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, store.Labels(r.name, r.hash, transport))
	spec.Labels = labels
	spec.Stdin = transport == "stdio"
	if transport == "http" {
		spec.Ports = httpPorts(spec.Ports, r.resolved.HTTP.Port, r.m.opts.InContainer)
	}
	if err := r.clearName(ctx, spec.Name); err != nil {
		r.failAttempt(ctx, err)
		return
	}
	id, err := r.m.engine.CreateContainer(ctx, spec)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("create container: %w", err))
		return
	}
	r.status.ContainerID = id
	if transport == "http" {
		r.startHTTP(ctx, false)
		return
	}
	r.attachAndStart(ctx)
}

// clearName removes a container that already holds name, so the create that
// follows does not fail with "name in use": a leftover managed container
// (any hash; adoption failed to remove it or could not list it), or an
// unlabelled one squatting on the reserved mcpm- name. A container carrying
// some other value of the managed label is left alone.
func (r *runner) clearName(ctx context.Context, name string) error {
	info, err := r.m.engine.InspectContainer(ctx, name)
	if err != nil {
		return nil //nolint:nilerr // not found, or unknown: CreateContainer reports any real problem
	}
	managed, labelled := info.Labels[store.LabelManaged]
	switch {
	case managed == "true":
	case !labelled && strings.HasPrefix(name, store.ContainerPrefix):
	default:
		return nil
	}
	r.m.log.Warn("manager: removing container that holds the server's name", "server", r.name, "container", info.ID, "name", name, "labels", info.Labels)
	_ = r.m.engine.StopContainer(ctx, info.ID, r.m.config().Podman.StopTimeout)
	if err := r.m.engine.RemoveContainer(ctx, info.ID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("remove container %s holding name %s: %w", info.ID, name, err)
	}
	return nil
}

// bindSession watches sess for loss and binds the facade to it: the last
// step of every start path.
func (r *runner) bindSession(ctx context.Context, sess *mcp.ClientSession, gen uint64) {
	r.sess = sess
	go func() {
		_ = sess.Wait()
		r.post(event{kind: evSessionLost, gen: gen})
	}()
	err := r.facade.Bind(ctx, sess)
	if ctx.Err() != nil {
		r.failAttempt(ctx, ctx.Err()) // interrupted: the preempting command decides
		return
	}
	if err != nil {
		if exitErr := r.exitedError(); exitErr != nil {
			err = exitErr
		} else {
			err = fmt.Errorf("bind: %w", err)
		}
		r.failAttempt(ctx, err)
		return
	}
	r.abandoned = false
	r.set(store.StateReady, "")
}

// ensureImage builds the image when the server has a build plan, otherwise
// pulls it if it is not present.
func (r *runner) ensureImage(ctx context.Context, res sources.Resolved, force bool) (string, error) {
	if plan := res.Image.Build; plan != nil {
		if r.m.builder == nil {
			return "", errors.New("build: no builder configured")
		}
		r.set(store.StateBuilding, "")
		ref, err := r.m.builder.Build(ctx, *plan, force)
		if err != nil {
			return "", fmt.Errorf("build: %w", err)
		}
		return ref, nil
	}
	ref := res.Image.Ref
	if ref == "" {
		ref = res.Container.Image
	}
	if ref == "" {
		return "", errors.New("manager: no image to run")
	}
	ok, err := r.m.engine.ImageExists(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("check image %s: %w", ref, err)
	}
	if !ok {
		if err := r.m.engine.PullImage(ctx, ref, res.Image.Auth, io.Discard); err != nil {
			return "", fmt.Errorf("pull %s: %w", ref, err)
		}
	}
	return ref, nil
}

// discardContainer force-removes the current container, if any.
func (r *runner) discardContainer(ctx context.Context) error {
	id := r.status.ContainerID
	if id == "" {
		return nil
	}
	r.gen++ // its exit is no longer news
	if err := r.m.engine.RemoveContainer(ctx, id, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("remove container: %w", err)
	}
	r.status.ContainerID = ""
	return nil
}

// onExit handles a container exit. It is processed even while a preempting
// command is queued behind it: that command may turn out to be a no-op, and
// fail's retry timer is cancelled by any command that does take over.
func (r *runner) onExit(ev event) {
	if ev.gen != r.gen {
		return // a previous container
	}
	switch r.status.State {
	case store.StateRunning, store.StateConnecting, store.StateReady:
		r.fail(r.exitError(ev.code))
	case store.StateFailed:
		// The dial or session hit EOF before the exit was visible; the exit
		// code and logs explain the failure better.
		if r.awaitExit {
			r.awaitExit = false
			r.status.Error = r.exitError(ev.code).Error()
			r.m.reg.Set(r.status)
		}
	}
}

// onSessionLost handles the end of the backend session. An engine may close
// the attach stream before it records the exit; then the exit that follows
// replaces the generic error (awaitExit).
func (r *runner) onSessionLost(ev event) {
	if ev.gen != r.gen || r.status.State != store.StateReady {
		return
	}
	if err := r.exitedError(); err != nil {
		r.fail(err)
		return
	}
	r.fail(errors.New("backend session closed"))
	r.awaitExit = r.status.ContainerID != ""
}

// stop drops the session and stops the container, keeping it.
func (r *runner) stop(paused bool) error {
	r.retryC = nil
	r.abandoned = false
	r.teardown()
	r.status.Paused = paused
	if id := r.status.ContainerID; id != "" {
		r.gen++ // the exit this stop causes is not news
		err := r.m.engine.StopContainer(r.ctx, id, r.m.config().Podman.StopTimeout)
		switch {
		case errors.Is(err, runtime.ErrNotFound):
			r.status.ContainerID = ""
		case err != nil:
			err = fmt.Errorf("stop container: %w", err)
			r.set(store.StateFailed, err.Error())
			return err
		}
	}
	r.set(store.StateStopped, "")
	return nil
}

// remove stops and removes the container and deletes the registry entry.
func (r *runner) remove() error {
	r.retryC = nil
	r.abandoned = false
	r.teardown()
	wasStopped := r.status.State == store.StateStopped
	r.set(store.StateRemoving, "")
	if id := r.status.ContainerID; id != "" {
		if !wasStopped {
			_ = r.m.engine.StopContainer(r.ctx, id, r.m.config().Podman.StopTimeout)
		}
		if err := r.discardContainer(r.ctx); err != nil {
			r.set(store.StateFailed, err.Error())
			return err
		}
	}
	r.m.forget(r)
	return nil
}

// rebuildPlan returns the server's build plan: the last resolved one, or a
// fresh resolve if the server never got that far.
func (r *runner) rebuildPlan() (sources.Resolved, error) {
	if r.resolved != nil && r.resolved.Image.Build != nil {
		return *r.resolved, nil
	}
	res, err := r.m.resolver.Resolve(r.ctx, r.m.config(), r.spec)
	if err != nil {
		return sources.Resolved{}, fmt.Errorf("resolve: %w", err)
	}
	if res.Image.Build == nil {
		return sources.Resolved{}, fmt.Errorf("manager: server %q is not built from source; nothing to rebuild", r.name)
	}
	return res, nil
}
