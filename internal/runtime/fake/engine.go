// Package fake is an in-memory runtime.Engine for unit tests.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

// exitState is one run's exit signal. code is written before done is closed
// and never changes afterwards, so waiters can read it without the mutex.
type exitState struct {
	done chan struct{}
	code int
}

func newExitState() *exitState { return &exitState{done: make(chan struct{})} }

func (x *exitState) isDone() bool {
	select {
	case <-x.done:
		return true
	default:
		return false
	}
}

type container struct {
	info  runtime.ContainerInfo
	spec  runtime.ContainerSpec
	logs  []runtime.LogLine
	exit  *exitState // replaced on restart
	local net.Conn   // end handed out by Attach
	peer  net.Conn   // far end, retrievable via Peer
}

// Engine is an in-memory runtime.Engine. All state is guarded by one mutex.
type Engine struct {
	mu         sync.Mutex
	containers map[string]*container // by id
	order      []string              // ids in creation order
	nextID     int
	nextBuild  int
	images     map[string]bool
	imageCfgs  map[string]runtime.ImageConfig
	builds     []runtime.BuildRequest
	pulls      []string
	pullAuths  []*runtime.RegistryAuth
	networks   []string
	conns      [][2]string
	calls      []string
	failures   map[string][]error
	execs      []ExecCall
	execExit   int
	onStart    func(id string)
}

// ExecCall records one Exec: the container, the command and everything read
// from its stdin.
type ExecCall struct {
	ID    string
	Cmd   []string
	Stdin []byte
}

var _ runtime.Engine = (*Engine)(nil)

// New returns an empty fake engine.
func New() *Engine {
	return &Engine{
		containers: map[string]*container{},
		images:     map[string]bool{},
		imageCfgs:  map[string]runtime.ImageConfig{},
		failures:   map[string][]error{},
	}
}

// begin records the call and pops a queued failure, if any. Callers hold e.mu.
func (e *Engine) begin(method string) error {
	e.calls = append(e.calls, method)
	if q := e.failures[method]; len(q) > 0 {
		err := q[0]
		e.failures[method] = q[1:]
		return err
	}
	return nil
}

// find resolves an id or name. Callers hold e.mu.
func (e *Engine) find(idOrName string) (*container, bool) {
	if c, ok := e.containers[idOrName]; ok {
		return c, true
	}
	for _, id := range e.order {
		if c := e.containers[id]; c.info.Name == idOrName {
			return c, true
		}
	}
	return nil, false
}

// markExited transitions c to exited, closes and clears the attach pipe (so
// a fresh Attach can create a new one) and wakes waiters with code. It is a
// no-op if c already exited. Callers hold e.mu.
func (e *Engine) markExited(c *container, code int) {
	if c.exit.isDone() {
		return
	}
	c.info.State = "exited"
	c.info.ExitCode = code
	if c.local != nil {
		_ = c.local.Close()
	}
	c.local, c.peer = nil, nil
	c.exit.code = code
	close(c.exit.done)
}

// Ping returns nil unless a failure was queued with FailNext("Ping", err).
func (e *Engine) Ping(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.begin("Ping")
}

func (e *Engine) EnsureNetwork(_ context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("EnsureNetwork"); err != nil {
		return err
	}
	e.networks = append(e.networks, name)
	return nil
}

func (e *Engine) ConnectNetwork(_ context.Context, containerID, network string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("ConnectNetwork"); err != nil {
		return err
	}
	e.conns = append(e.conns, [2]string{containerID, network})
	if c, ok := e.find(containerID); ok {
		c.info.Networks = addNetwork(c.info.Networks, network)
	}
	return nil
}

func (e *Engine) ImageExists(_ context.Context, ref string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("ImageExists"); err != nil {
		return false, err
	}
	return e.images[ref], nil
}

func (e *Engine) PullImage(_ context.Context, ref string, auth *runtime.RegistryAuth, progress io.Writer) error {
	e.mu.Lock()
	if err := e.begin("PullImage"); err != nil {
		e.mu.Unlock()
		return err
	}
	e.pulls = append(e.pulls, ref)
	if auth != nil {
		a := *auth
		auth = &a
	}
	e.pullAuths = append(e.pullAuths, auth)
	e.images[ref] = true
	e.mu.Unlock()
	// Write outside the lock: the writer may block or call back into e.
	if progress != nil {
		_, _ = fmt.Fprintf(progress, "pulled %s\n", ref)
	}
	return nil
}

func (e *Engine) BuildImage(_ context.Context, req runtime.BuildRequest) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("BuildImage"); err != nil {
		return "", err
	}
	e.builds = append(e.builds, req)
	e.nextBuild++
	if req.Tag != "" {
		e.images[req.Tag] = true
	}
	return fmt.Sprintf("sha256:fake-%d", e.nextBuild), nil
}

func (e *Engine) CreateContainer(_ context.Context, spec runtime.ContainerSpec) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("CreateContainer"); err != nil {
		return "", err
	}
	if len(spec.SecretEnv) > 0 {
		return "", errors.New("runtime: SecretEnv must be resolved before create")
	}
	for _, c := range e.containers {
		if c.info.Name == spec.Name {
			return "", errors.New("runtime: container name in use")
		}
	}
	e.nextID++
	id := fmt.Sprintf("fake-%d", e.nextID)
	e.containers[id] = &container{
		spec: spec,
		info: runtime.ContainerInfo{
			ID:     id,
			Name:   spec.Name,
			Image:  spec.Image,
			State:  "created",
			Labels: copyLabels(spec.Labels),
			Ports:  append([]runtime.PortMapping(nil), spec.Ports...),
		},
		exit: newExitState(),
	}
	if spec.Network != "" {
		e.containers[id].info.Networks = []string{spec.Network}
	}
	e.order = append(e.order, id)
	return id, nil
}

func (e *Engine) StartContainer(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("StartContainer"); err != nil {
		return err
	}
	c, ok := e.find(id)
	if !ok {
		return runtime.ErrNotFound
	}
	if c.exit.isDone() { // restarting an exited container
		c.exit = newExitState()
		c.info.ExitCode = 0
	}
	c.info.State = "running"
	if hook := e.onStart; hook != nil {
		e.mu.Unlock()
		hook(c.info.ID)
		e.mu.Lock()
	}
	return nil
}

// OnStart registers hook to run, outside the lock, each time a container
// starts, before StartContainer returns; for example to make it exit at
// once.
func (e *Engine) OnStart(hook func(id string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onStart = hook
}

func (e *Engine) StopContainer(_ context.Context, id string, _ time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("StopContainer"); err != nil {
		return err
	}
	c, ok := e.find(id)
	if !ok {
		return runtime.ErrNotFound
	}
	if c.info.State == "exited" {
		return nil // already stopped; keep its recorded exit code
	}
	e.markExited(c, 0)
	return nil
}

func (e *Engine) RemoveContainer(_ context.Context, id string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("RemoveContainer"); err != nil {
		return err
	}
	c, ok := e.find(id)
	if !ok {
		return runtime.ErrNotFound
	}
	if c.info.State == "running" {
		if !force {
			return errors.New("runtime: container is running")
		}
		e.markExited(c, 137)
	}
	delete(e.containers, c.info.ID)
	for i, oid := range e.order {
		if oid == c.info.ID {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	return nil
}

func (e *Engine) InspectContainer(_ context.Context, id string) (runtime.ContainerInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("InspectContainer"); err != nil {
		return runtime.ContainerInfo{}, err
	}
	c, ok := e.find(id)
	if !ok {
		return runtime.ContainerInfo{}, runtime.ErrNotFound
	}
	return cloneInfo(c.info), nil
}

func (e *Engine) ListContainers(_ context.Context, labels map[string]string) ([]runtime.ContainerInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("ListContainers"); err != nil {
		return nil, err
	}
	var out []runtime.ContainerInfo
	for _, id := range e.order {
		c := e.containers[id]
		match := true
		for k, v := range labels {
			if got, ok := c.info.Labels[k]; !ok || got != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, cloneInfo(c.info))
		}
	}
	return out, nil
}

func (e *Engine) Logs(_ context.Context, id string, tail int) ([]runtime.LogLine, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("Logs"); err != nil {
		return nil, err
	}
	c, ok := e.find(id)
	if !ok {
		return nil, runtime.ErrNotFound
	}
	lines := c.logs
	if tail > 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	return append([]runtime.LogLine(nil), lines...), nil
}

// Attach must be called on a created, running or exited (about to be
// restarted) container. It hands out one
// end of a net.Pipe; the other end is available via Peer.
func (e *Engine) Attach(_ context.Context, id string) (io.ReadWriteCloser, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("Attach"); err != nil {
		return nil, err
	}
	c, ok := e.find(id)
	if !ok {
		return nil, runtime.ErrNotFound
	}
	if c.info.State != "created" && c.info.State != "running" && c.info.State != "exited" {
		return nil, fmt.Errorf("runtime: cannot attach to %s container", c.info.State)
	}
	if c.local != nil {
		return nil, errors.New("runtime: already attached")
	}
	c.local, c.peer = net.Pipe()
	return c.local, nil
}

// Wait blocks until the container exits (returning its exit code) or ctx is
// done. The mutex is not held while blocking.
func (e *Engine) Wait(ctx context.Context, id string) (int, error) {
	e.mu.Lock()
	if err := e.begin("Wait"); err != nil {
		e.mu.Unlock()
		return 0, err
	}
	c, ok := e.find(id)
	if !ok {
		e.mu.Unlock()
		return 0, runtime.ErrNotFound
	}
	exit := c.exit
	e.mu.Unlock()

	select {
	case <-exit.done:
		return exit.code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// ImageConfig returns the configuration registered with SetImageConfig, or
// a zero one for an image that merely exists; an unknown image is ErrNotFound.
func (e *Engine) ImageConfig(_ context.Context, ref string) (runtime.ImageConfig, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("ImageConfig"); err != nil {
		return runtime.ImageConfig{}, err
	}
	if cfg, ok := e.imageCfgs[ref]; ok {
		return cloneImageConfig(cfg), nil
	}
	if e.images[ref] {
		return runtime.ImageConfig{}, nil
	}
	return runtime.ImageConfig{}, runtime.ErrNotFound
}

// Exec records the call (reading stdin to EOF) and returns the exit code
// set by SetExecExit (0 by default). The container must be running.
func (e *Engine) Exec(_ context.Context, id string, cmd []string, stdin io.Reader) (int, error) {
	e.mu.Lock()
	if err := e.begin("Exec"); err != nil {
		e.mu.Unlock()
		return 0, err
	}
	c, ok := e.find(id)
	if !ok {
		e.mu.Unlock()
		return 0, runtime.ErrNotFound
	}
	if c.info.State != "running" {
		e.mu.Unlock()
		return 0, fmt.Errorf("runtime: cannot exec in %s container", c.info.State)
	}
	cid, code := c.info.ID, e.execExit
	e.mu.Unlock()

	var in []byte
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		in = b
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.execs = append(e.execs, ExecCall{ID: cid, Cmd: append([]string(nil), cmd...), Stdin: in})
	return code, nil
}

// Execs returns the recorded Exec calls, in order.
func (e *Engine) Execs() []ExecCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ExecCall, len(e.execs))
	for i, c := range e.execs {
		out[i] = ExecCall{ID: c.ID, Cmd: append([]string(nil), c.Cmd...), Stdin: append([]byte(nil), c.Stdin...)}
	}
	return out
}

// SetExecExit sets the exit code every following Exec returns.
func (e *Engine) SetExecExit(code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.execExit = code
}

// SetImageConfig registers an image (marking it present) with the
// configuration ImageConfig returns for it.
func (e *Engine) SetImageConfig(ref string, cfg runtime.ImageConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.images[ref] = true
	e.imageCfgs[ref] = cloneImageConfig(cfg)
}

// PullAuths returns the credentials passed to each PullImage, in order (nil
// entries for pulls without credentials).
func (e *Engine) PullAuths() []*runtime.RegistryAuth {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*runtime.RegistryAuth, len(e.pullAuths))
	for i, a := range e.pullAuths {
		if a != nil {
			c := *a
			out[i] = &c
		}
	}
	return out
}

func cloneImageConfig(c runtime.ImageConfig) runtime.ImageConfig {
	c.Entrypoint = append([]string(nil), c.Entrypoint...)
	c.Cmd = append([]string(nil), c.Cmd...)
	return c
}

// Spec returns the spec a container was created with (mounts included, as
// given), by id or name.
func (e *Engine) Spec(idOrName string) (runtime.ContainerSpec, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.find(idOrName)
	if !ok {
		return runtime.ContainerSpec{}, false
	}
	spec := c.spec
	spec.Mounts = append([]runtime.Mount(nil), c.spec.Mounts...)
	return spec, true
}

// Peer returns the far end of the pipe handed out by Attach.
func (e *Engine) Peer(id string) (io.ReadWriteCloser, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.find(id)
	if !ok || c.peer == nil {
		return nil, false
	}
	return c.peer, true
}

// Exit simulates the container process exiting with code.
func (e *Engine) Exit(id string, code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.find(id); ok {
		e.markExited(c, code)
	}
}

// FailNext queues err to be returned by the next call to method (FIFO per
// method). Each queued error is returned once.
func (e *Engine) FailNext(method string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures[method] = append(e.failures[method], err)
}

// Calls returns the method names called so far, in order.
func (e *Engine) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// Seed registers a pre-existing container (for adoption tests).
func (e *Engine) Seed(info runtime.ContainerInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	info = cloneInfo(info)
	c := &container{info: info, exit: newExitState()}
	if info.State == "exited" {
		c.exit.code = info.ExitCode
		close(c.exit.done)
	}
	e.containers[info.ID] = c
	e.order = append(e.order, info.ID)
}

// SetLogs sets the log lines returned by Logs for the container.
func (e *Engine) SetLogs(id string, lines []runtime.LogLine) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.find(id); ok {
		c.logs = append([]runtime.LogLine(nil), lines...)
	}
}

// MarkImage pre-seeds an existing image reference.
func (e *Engine) MarkImage(ref string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.images[ref] = true
}

// Builds returns the recorded build requests.
func (e *Engine) Builds() []runtime.BuildRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]runtime.BuildRequest(nil), e.builds...)
}

// Pulls returns the recorded pulled image references.
func (e *Engine) Pulls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.pulls...)
}

// Networks returns the network names passed to EnsureNetwork.
func (e *Engine) Networks() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.networks...)
}

// Connections returns (containerID, network) pairs passed to ConnectNetwork.
func (e *Engine) Connections() [][2]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][2]string(nil), e.conns...)
}

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// addNetwork returns nets with name added, deduplicated and sorted.
func addNetwork(nets []string, name string) []string {
	if slices.Contains(nets, name) {
		return nets
	}
	nets = append(nets, name)
	slices.Sort(nets)
	return nets
}

func cloneInfo(in runtime.ContainerInfo) runtime.ContainerInfo {
	in.Networks = append([]string(nil), in.Networks...)
	in.Labels = copyLabels(in.Labels)
	in.Ports = append([]runtime.PortMapping(nil), in.Ports...)
	return in
}
