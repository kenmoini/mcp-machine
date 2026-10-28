// Package fake is an in-memory runtime.Engine for unit tests.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

type container struct {
	info   runtime.ContainerInfo
	spec   runtime.ContainerSpec
	logs   []runtime.LogLine
	exited chan struct{} // closed when the container exits
	local  net.Conn      // end handed out by Attach
	peer   net.Conn      // far end, retrievable via Peer
}

// Engine is an in-memory runtime.Engine. All state is guarded by one mutex.
type Engine struct {
	mu         sync.Mutex
	containers map[string]*container // by id
	order      []string              // ids in creation order
	nextID     int
	nextBuild  int
	images     map[string]bool
	builds     []runtime.BuildRequest
	pulls      []string
	networks   []string
	conns      [][2]string
	calls      []string
	failures   map[string][]error
}

var _ runtime.Engine = (*Engine)(nil)

// New returns an empty fake engine.
func New() *Engine {
	return &Engine{
		containers: map[string]*container{},
		images:     map[string]bool{},
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

// markExited transitions c to exited, closes the attach pipe and wakes
// waiters. Callers hold e.mu.
func (e *Engine) markExited(c *container, code int) {
	c.info.State = "exited"
	c.info.ExitCode = code
	if c.local != nil {
		_ = c.local.Close()
	}
	select {
	case <-c.exited:
	default:
		close(c.exited)
	}
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

func (e *Engine) PullImage(_ context.Context, ref string, _ *runtime.RegistryAuth, progress io.Writer) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.begin("PullImage"); err != nil {
		return err
	}
	e.pulls = append(e.pulls, ref)
	e.images[ref] = true
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
		exited: make(chan struct{}),
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
	select {
	case <-c.exited: // restarting an exited container
		c.exited = make(chan struct{})
		c.info.ExitCode = 0
	default:
	}
	c.info.State = "running"
	return nil
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

// Attach must be called on a created or running container. It hands out one
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
	if c.info.State != "created" && c.info.State != "running" {
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
	exited := c.exited
	e.mu.Unlock()

	select {
	case <-exited:
		e.mu.Lock()
		defer e.mu.Unlock()
		return c.info.ExitCode, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
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
	c := &container{info: info, exited: make(chan struct{})}
	if info.State == "exited" {
		close(c.exited)
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

func cloneInfo(in runtime.ContainerInfo) runtime.ContainerInfo {
	in.Labels = copyLabels(in.Labels)
	in.Ports = append([]runtime.PortMapping(nil), in.Ports...)
	return in
}
