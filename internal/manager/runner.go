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

type evKind int

const (
	evStart evKind = iota
	evStop
	evRestart
	evRemove
	evRebuild
	evShutdown    // manager closing: drop the session, leave the container
	evExit        // the container of generation gen exited with code
	evSessionLost // the session of generation gen ended
)

// preempts reports whether a command must interrupt whatever the runner is
// doing (a dial to an unresponsive backend, a long build) instead of queueing
// behind it.
func (k evKind) preempts() bool {
	switch k {
	case evStop, evRestart, evRemove, evRebuild, evShutdown:
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

	// Owned by the runner goroutine.
	status   store.Status
	resolved *sources.Resolved // last successful resolve
	image    string            // image ref of the last successful image step
	gen      uint64            // bumped when a container starts, is stopped or is discarded
	sess     *mcp.ClientSession
	rwc      io.ReadWriteCloser
	retries  int              // consecutive automatic retries; reset by Start
	retryC   <-chan time.Time // pending retry timer; nil when none
	// awaitExit: the last dial failed before the container's exit was
	// visible; an exit of the same generation replaces the error.
	awaitExit bool
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

// post delivers an event without ever blocking the runner: it waits for room
// in the channel or for the runner to end, so nothing is dropped or leaked.
func (r *runner) post(ev event) {
	select {
	case r.events <- ev:
	case <-r.ctx.Done():
	}
}

// send delivers a command and waits for the runner's answer.
func (r *runner) send(ctx context.Context, kind evKind) error {
	if kind.preempts() {
		r.interrupt()
	}
	ev := event{kind: kind, reply: make(chan error, 1)}
	select {
	case r.events <- ev:
	case <-r.done:
		if kind.preempts() {
			r.uninterrupt()
		}
		return r.gone()
	case <-ctx.Done():
		if kind.preempts() {
			r.uninterrupt()
		}
		return ctx.Err()
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
		}
	case evRemove:
		err := r.remove()
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
	}
	return false
}

// set records a state transition in the registry.
func (r *runner) set(state store.State, errMsg string) {
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
// schedules a retry. The timer is armed before the status is published so an
// observer of Failed can already advance a manual clock past it.
func (r *runner) fail(err error) {
	r.teardown()
	r.awaitExit = false
	r.retryC = nil
	if r.retries < r.m.opts.MaxRetries {
		r.retryC = r.m.opts.Clock.After(backoff(r.retries, r.m.opts.MaxBackoff))
	}
	r.set(store.StateFailed, err.Error())
}

// failAttempt is fail for errors inside a start pipeline. An interrupted
// attempt is abandoned quietly: the preempting command decides the state.
func (r *runner) failAttempt(ctx context.Context, err error) {
	if ctx.Err() != nil {
		r.teardown()
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
		if r.canResume() {
			ctx := r.beginAttempt()
			r.set(store.StateCreating, "")
			r.attachAndStart(ctx)
			return
		}
	}
	r.launch(false)
}

// canResume reports whether the stopped container still exists and was
// created for the current spec.
func (r *runner) canResume() bool {
	if r.status.ContainerID == "" {
		return false
	}
	info, err := r.m.engine.InspectContainer(r.ctx, r.status.ContainerID)
	if err != nil {
		return false
	}
	return info.Labels[store.LabelHash] == r.hash
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

// launch runs the whole pipeline: Resolve → image → create → attach → start
// → dial → bind.
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

// provision makes the image available and creates the container.
func (r *runner) provision(ctx context.Context, res sources.Resolved, forceBuild bool) {
	// Task 27 adds the http-mode and remote paths here.
	if res.Kind != sources.KindContainer || res.Container == nil || res.Transport != "stdio" {
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
	maps.Copy(labels, store.Labels(r.name, r.hash, "stdio"))
	spec.Labels = labels
	spec.Stdin = true
	id, err := r.m.engine.CreateContainer(ctx, spec)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("create container: %w", err))
		return
	}
	r.status.ContainerID = id
	r.attachAndStart(ctx)
}

// attachAndStart attaches to the container's stdio (before starting it, so
// no output is lost), starts it, and connects the facade to it.
func (r *runner) attachAndStart(ctx context.Context) {
	id := r.status.ContainerID
	rwc, err := r.m.engine.Attach(ctx, id)
	if err != nil {
		r.failAttempt(ctx, fmt.Errorf("attach: %w", err))
		return
	}
	if err := r.m.engine.StartContainer(ctx, id); err != nil {
		_ = rwc.Close()
		r.failAttempt(ctx, fmt.Errorf("start container: %w", err))
		return
	}
	// Cancelling the attempt must unblock a dial stuck writing to a backend
	// that never reads; context cancellation alone does not.
	context.AfterFunc(ctx, func() { _ = rwc.Close() })
	r.gen++
	gen := r.gen
	go r.waitExit(gen, id)
	r.set(store.StateRunning, "")

	r.set(store.StateConnecting, "")
	// The session lives on ctx, the attempt's long-lived context, not a
	// dial timeout: it must outlive the dial.
	sess, err := proxy.DialIO(ctx, rwc, r.facade.ClientOptions())
	if err != nil {
		_ = rwc.Close()
		if exitErr := r.exitedError(); exitErr != nil {
			r.failAttempt(ctx, exitErr)
			return
		}
		r.failAttempt(ctx, fmt.Errorf("connect: %w", err))
		r.awaitExit = ctx.Err() == nil
		return
	}
	r.sess, r.rwc = sess, rwc
	go func() {
		_ = sess.Wait()
		r.post(event{kind: evSessionLost, gen: gen})
	}()
	if err := r.facade.Bind(ctx, sess); err != nil {
		if exitErr := r.exitedError(); exitErr != nil {
			err = exitErr
		} else {
			err = fmt.Errorf("bind: %w", err)
		}
		r.failAttempt(ctx, err)
		return
	}
	r.set(store.StateReady, "")
}

// waitExit reports the exit of container id, started as generation gen. It
// lives as long as the runner (not the attempt) so an exit that races a
// failed dial is still seen and can explain it.
func (r *runner) waitExit(gen uint64, id string) {
	code, err := r.m.engine.Wait(r.ctx, id)
	if err != nil {
		if r.ctx.Err() == nil {
			r.m.log.Debug("manager: wait failed", "server", r.name, "container", id, "err", err)
		}
		return
	}
	r.post(event{kind: evExit, gen: gen, code: code})
}

// exitedError returns the exit error if the container has exited, else nil.
func (r *runner) exitedError() error {
	if info, err := r.m.engine.InspectContainer(r.ctx, r.status.ContainerID); err == nil && info.State == "exited" {
		return r.exitError(info.ExitCode)
	}
	return nil
}

// exitError describes a container exit with its last log lines.
func (r *runner) exitError(code int) error {
	tail := "(none)"
	if lines, err := r.m.engine.Logs(r.ctx, r.status.ContainerID, exitLogLines); err == nil && len(lines) > 0 {
		texts := make([]string, len(lines))
		for i, l := range lines {
			texts[i] = l.Text
		}
		tail = strings.Join(texts, " | ")
	}
	return fmt.Errorf("container exited with code %d; last log lines: %s", code, tail)
}

func (r *runner) onExit(ev event) {
	if ev.gen != r.gen || r.interrupted() {
		return // a previous container, or a command is about to take over
	}
	switch r.status.State {
	case store.StateRunning, store.StateConnecting, store.StateReady:
		r.fail(r.exitError(ev.code))
	case store.StateFailed:
		// The dial hit EOF before the exit was visible; the exit code and
		// logs explain the failure better.
		if r.awaitExit {
			r.awaitExit = false
			r.status.Error = r.exitError(ev.code).Error()
			r.m.reg.Set(r.status)
		}
	}
}

func (r *runner) onSessionLost(ev event) {
	if ev.gen != r.gen || r.interrupted() || r.status.State != store.StateReady {
		return
	}
	err := r.exitedError()
	if err == nil {
		err = errors.New("backend session closed")
	}
	r.fail(err)
}

// stop drops the session and stops the container, keeping it.
func (r *runner) stop(paused bool) error {
	r.retryC = nil
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
	r.m.reg.Delete(r.name)
	return nil
}

// rebuildPlan resolves the server and checks that it is built from source.
func (r *runner) rebuildPlan() (sources.Resolved, error) {
	res, err := r.m.resolver.Resolve(r.ctx, r.m.config(), r.spec)
	if err != nil {
		return sources.Resolved{}, fmt.Errorf("resolve: %w", err)
	}
	if res.Image.Build == nil {
		return sources.Resolved{}, fmt.Errorf("manager: server %q is not built from source; nothing to rebuild", r.name)
	}
	return res, nil
}
