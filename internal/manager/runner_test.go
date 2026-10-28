package manager_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/fake"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// --- helpers ---------------------------------------------------------------

// stubResolver returns a copy of res (or err) for every server.
type stubResolver struct {
	mu    sync.Mutex
	res   sources.Resolved
	err   error
	calls int
}

func (s *stubResolver) Resolve(_ context.Context, _ *config.Config, _ config.ServerSpec) (sources.Resolved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return sources.Resolved{}, s.err
	}
	out := s.res
	if out.Container != nil {
		c := *out.Container
		out.Container = &c
	}
	if out.Image.Build != nil {
		b := *out.Image.Build
		out.Image.Build = &b
	}
	return out, nil
}

func stdioResolved() sources.Resolved {
	return sources.Resolved{
		Kind: sources.KindContainer,
		Container: &runtime.ContainerSpec{
			Name:    store.ContainerName("a"),
			Image:   "img:1",
			Command: []string{"npx", "-y", "server"},
			Stdin:   true,
			Network: "mcp-machine",
		},
		Image:     sources.ImageSource{Ref: "img:1"},
		Transport: "stdio",
	}
}

type buildCall struct {
	plan  sources.BuildPlan
	force bool
}

// fakeBuilder records Build calls and returns ref.
type fakeBuilder struct {
	mu    sync.Mutex
	ref   string
	calls []buildCall
}

func (b *fakeBuilder) Build(_ context.Context, plan sources.BuildPlan, force bool) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, buildCall{plan, force})
	return b.ref, nil
}

func (b *fakeBuilder) Calls() []buildCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]buildCall(nil), b.calls...)
}

// fakeClock is a manual clock: timers fire only when Advance moves past them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock forward and fires every timer whose deadline passed.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// Pending reports the number of timers that have not fired yet.
func (c *fakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

type harness struct {
	t   *testing.T
	eng *fake.Engine
	res *stubResolver
	bld *fakeBuilder
	clk *fakeClock
	reg *store.Registry
	cfg *config.Config
	m   *manager.Manager
}

func newHarness(t *testing.T, opts manager.Options) *harness {
	t.Helper()
	h := &harness{
		t:   t,
		eng: fake.New(),
		res: &stubResolver{res: stdioResolved()},
		bld: &fakeBuilder{ref: "localhost/mcp-machine/a:abc1234"},
		clk: newFakeClock(),
		reg: store.NewRegistry(),
		cfg: &config.Config{
			Podman: config.PodmanConfig{Network: "mcp-machine", StopTimeout: time.Second},
			Servers: []config.ServerSpec{{
				Name:    "a",
				Type:    "command",
				Command: &config.CommandSpec{Command: "npx", Args: []string{"-y", "server"}},
			}},
		},
	}
	opts.Clock = h.clk
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h.m = manager.New(h.eng, h.res, h.bld, h.reg, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.m.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h
}

func (h *harness) apply() {
	h.t.Helper()
	if err := h.m.Apply(context.Background(), h.cfg); err != nil {
		h.t.Fatalf("Apply: %v", err)
	}
}

// ready applies the config, serves a backend and waits for Ready.
func (h *harness) ready() (store.Status, *mcp.Server) {
	h.t.Helper()
	h.apply()
	backend := serveBackend(h.t, h.eng, store.ContainerName("a"))
	return waitState(h.t, h.reg, "a", store.StateReady), backend
}

func (h *harness) count(method string) int {
	n := 0
	for _, c := range h.eng.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

// serveBackend waits for the runner to attach to container id, then serves a
// real MCP server with a "ping" tool on the far end of the attach pipe. The
// backend does not advertise list changes, so additions need a Resync.
func serveBackend(t *testing.T, eng *fake.Engine, id string) *mcp.Server {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var peer interface {
		Read([]byte) (int, error)
		Write([]byte) (int, error)
		Close() error
	}
	for {
		if p, ok := eng.Peer(id); ok {
			peer = p
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no attach peer for %s within 3s", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
	backend := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: false}},
	})
	backend.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = backend.Run(ctx, &mcp.IOTransport{Reader: peer, Writer: peer}) }()
	return backend
}

// serveNext waits until the server runs on a container other than oldID and
// serves a backend on it.
func serveNext(t *testing.T, h *harness, oldID string) *mcp.Server {
	t.Helper()
	st := waitFor(t, h.reg, "a", "a new container", func(s store.Status, ok bool) bool {
		return ok && s.ContainerID != "" && s.ContainerID != oldID
	})
	return serveBackend(t, h.eng, st.ContainerID)
}

// waitFor polls the registry until cond holds for name, or fails after 3s.
func waitFor(t *testing.T, reg *store.Registry, name, what string, cond func(store.Status, bool) bool) store.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, ok := reg.Get(name)
		if cond(st, ok) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out waiting for %s; last status %+v (present %v)", name, what, st, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitState(t *testing.T, reg *store.Registry, name string, want store.State) store.Status {
	t.Helper()
	return waitFor(t, reg, name, string(want), func(st store.Status, ok bool) bool { return ok && st.State == want })
}

func listTools(t *testing.T, srv *mcp.Server) []string {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

// inOrder reports whether want appears in calls as a subsequence.
func inOrder(calls, want []string) bool {
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	return i == len(want)
}

// --- tests -----------------------------------------------------------------

func TestRunnerReachesReady(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	f, ok := h.m.Facade("a")
	if !ok {
		t.Fatal("Facade(a) not found")
	}
	if got := listTools(t, f.Server()); !slices.Contains(got, "ping") {
		t.Errorf("facade tools = %v, want ping", got)
	}
	if _, ok := h.m.Lookup("a"); !ok {
		t.Error("Lookup(a) = false while Ready")
	}
	if calls := h.eng.Calls(); !inOrder(calls, []string{"CreateContainer", "Attach", "StartContainer"}) {
		t.Errorf("calls %v lack CreateContainer, Attach, StartContainer in order", calls)
	}

	wantHash := config.Hash(h.cfg.Servers[0], h.cfg.Servers[0].RuntimeImage(h.cfg))
	if st.Hash != wantHash {
		t.Errorf("status hash = %q, want %q", st.Hash, wantHash)
	}
	if st.Image != "img:1" || st.ContainerID == "" || st.Error != "" || st.Paused {
		t.Errorf("status = %+v", st)
	}
	info, err := h.eng.InspectContainer(context.Background(), st.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if lbl, ok := store.ParseLabels(info.Labels); !ok || lbl.Name != "a" || lbl.Hash != wantHash || lbl.Transport != "stdio" {
		t.Errorf("labels = %v", info.Labels)
	}
	if info.Name != "mcpm-a" || info.State != "running" {
		t.Errorf("container = %+v", info)
	}
	if nets := h.eng.Networks(); !slices.Contains(nets, "mcp-machine") {
		t.Errorf("EnsureNetwork calls = %v", nets)
	}

	// A second Apply with the same config creates no second runner.
	h.apply()
	if n := h.count("CreateContainer"); n != 1 {
		t.Errorf("CreateContainer called %d times, want 1", n)
	}
	if got := len(h.reg.List()); got != 1 {
		t.Errorf("registry has %d entries, want 1", got)
	}
}

func TestRunnerStopAndStart(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	ctx := context.Background()

	if err := h.m.Stop(ctx, "a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got, _ := h.reg.Get("a")
	if got.State != store.StateStopped || !got.Paused {
		t.Errorf("after Stop status = %+v, want Stopped and Paused", got)
	}
	info, err := h.eng.InspectContainer(ctx, st.ContainerID)
	if err != nil {
		t.Fatalf("container gone after Stop: %v", err)
	}
	if info.State != "exited" {
		t.Errorf("container state = %q, want exited", info.State)
	}
	if _, ok := h.m.Lookup("a"); ok {
		t.Error("Lookup(a) = true while Stopped")
	}
	if _, ok := h.m.Facade("a"); !ok {
		t.Error("Facade(a) missing while Stopped")
	}

	if err := h.m.Start(ctx, "a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	serveBackend(t, h.eng, st.ContainerID)
	got = waitState(t, h.reg, "a", store.StateReady)
	if got.Paused {
		t.Error("Paused still set after Start")
	}
	if got.ContainerID != st.ContainerID {
		t.Errorf("container changed: %s -> %s", st.ContainerID, got.ContainerID)
	}
	if n := h.count("CreateContainer"); n != 1 {
		t.Errorf("CreateContainer called %d times, want 1", n)
	}
	if _, ok := h.m.Lookup("a"); !ok {
		t.Error("Lookup(a) = false after Start")
	}
}

func TestRunnerRemove(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	ctx := context.Background()

	if err := h.m.Remove(ctx, "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, ok := h.reg.Get("a"); ok {
		t.Errorf("registry still has a: %+v", got)
	}
	if _, err := h.eng.InspectContainer(ctx, st.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Inspect after Remove err = %v, want ErrNotFound", err)
	}
	if _, ok := h.m.Facade("a"); ok {
		t.Error("Facade(a) still present after Remove")
	}
	if err := h.m.Start(ctx, "a"); !errors.Is(err, manager.ErrUnknownServer) {
		t.Errorf("Start after Remove err = %v, want ErrUnknownServer", err)
	}
}

func TestApplyRemovesServerDroppedFromConfig(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	h.cfg = &config.Config{Podman: h.cfg.Podman}
	h.apply()
	if _, ok := h.reg.Get("a"); ok {
		t.Error("registry still has a after it left the config")
	}
	if _, err := h.eng.InspectContainer(context.Background(), st.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("container not removed: %v", err)
	}
}

func TestRunnerRetriesAfterExit(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	h.eng.SetLogs(st.ContainerID, []runtime.LogLine{
		{Stream: "stderr", Text: "npm ERR! 404 Not Found"},
		{Stream: "stderr", Text: "npm ERR! no such package"},
	})
	h.eng.Exit(st.ContainerID, 1)
	failed := waitFor(t, h.reg, "a", "exit failure", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "exited with code 1")
	})
	if !strings.Contains(failed.Error, "npm ERR! no such package") {
		t.Errorf("error %q lacks the last log lines", failed.Error)
	}
	if !strings.HasPrefix(failed.Error, "container exited with code 1; last log lines: ") {
		t.Errorf("error = %q", failed.Error)
	}
	if _, ok := h.m.Lookup("a"); ok {
		t.Error("Lookup(a) = true while Failed")
	}

	h.clk.Advance(time.Second)
	serveNext(t, h, st.ContainerID)
	got := waitState(t, h.reg, "a", store.StateReady)
	if got.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", got.Restarts)
	}
	if got.ContainerID == st.ContainerID {
		t.Error("retry reused the exited container; want a fresh one")
	}
	if _, err := h.eng.InspectContainer(context.Background(), st.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("old container not removed: %v", err)
	}
}

func TestRunnerRetriesAfterSessionLoss(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	peer, ok := h.eng.Peer(st.ContainerID)
	if !ok {
		t.Fatal("no peer")
	}
	_ = peer.Close()
	failed := waitState(t, h.reg, "a", store.StateFailed)
	if failed.Error != "backend session closed" {
		t.Errorf("error = %q, want backend session closed", failed.Error)
	}

	h.clk.Advance(time.Second)
	serveNext(t, h, st.ContainerID)
	if got := waitState(t, h.reg, "a", store.StateReady); got.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", got.Restarts)
	}
}

func TestRunnerStopsRetryingAfterMax(t *testing.T) {
	h := newHarness(t, manager.Options{MaxRetries: 2})
	st, _ := h.ready()

	for i := 0; i < 2; i++ {
		h.eng.Exit(st.ContainerID, 1)
		waitState(t, h.reg, "a", store.StateFailed)
		h.clk.Advance(time.Second << i) // backoff: 1s, then 2s
		serveNext(t, h, st.ContainerID)
		st = waitState(t, h.reg, "a", store.StateReady)
		if st.Restarts != i+1 {
			t.Fatalf("Restarts = %d, want %d", st.Restarts, i+1)
		}
	}

	// Third exit: the retry cap is reached, so the runner stays Failed.
	h.eng.Exit(st.ContainerID, 7)
	failed := waitFor(t, h.reg, "a", "sticky failure", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "exited with code 7")
	})
	if n := h.clk.Pending(); n != 0 {
		t.Errorf("%d retry timers pending after the cap, want 0", n)
	}
	creates := h.count("CreateContainer")
	h.clk.Advance(time.Hour)
	if got, _ := h.reg.Get("a"); got.State != store.StateFailed || got.Restarts != 2 {
		t.Errorf("after the cap status = %+v, want Failed with 2 restarts", got)
	}
	if n := h.count("CreateContainer"); n != creates {
		t.Errorf("CreateContainer called after the cap (%d -> %d)", creates, n)
	}
	if failed.Restarts != 2 {
		t.Errorf("Restarts = %d, want 2", failed.Restarts)
	}

	// Start resets the retry counter.
	if err := h.m.Start(context.Background(), "a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	serveNext(t, h, st.ContainerID)
	st = waitState(t, h.reg, "a", store.StateReady)
	h.eng.Exit(st.ContainerID, 1)
	waitState(t, h.reg, "a", store.StateFailed)
	if n := h.clk.Pending(); n != 1 {
		t.Errorf("after Start and a new exit, %d retry timers pending, want 1", n)
	}
}

func TestRunnerSurfacesStartError(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.eng.FailNext("StartContainer", errors.New("boom"))
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "boom") {
		t.Errorf("error = %q, want it to contain boom", st.Error)
	}
	if _, ok := h.m.Lookup("a"); ok {
		t.Error("Lookup(a) = true while Failed")
	}
}

func TestRunnerSurfacesResolveError(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.res.err = errors.New("bad spec")
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "bad spec") {
		t.Errorf("error = %q", st.Error)
	}
	if n := h.count("CreateContainer"); n != 0 {
		t.Errorf("CreateContainer called %d times after a resolve error", n)
	}
}

func TestRunnerPullsMissingImage(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		h := newHarness(t, manager.Options{})
		h.ready()
		if got := h.eng.Pulls(); !slices.Equal(got, []string{"img:1"}) {
			t.Errorf("Pulls = %v, want [img:1]", got)
		}
	})
	t.Run("present", func(t *testing.T) {
		h := newHarness(t, manager.Options{})
		h.eng.MarkImage("img:1")
		h.ready()
		if got := h.eng.Pulls(); len(got) != 0 {
			t.Errorf("Pulls = %v, want none", got)
		}
	})
}

// useBuild turns server a into a build-type server whose resolve yields a
// build plan.
func (h *harness) useBuild() {
	h.cfg.Servers[0] = config.ServerSpec{
		Name:  "a",
		Type:  "build",
		Build: &config.BuildSpec{Repo: "https://example.com/a.git", Ref: "main"},
	}
	h.res.res = buildResolved()
}

func buildResolved() sources.Resolved {
	res := stdioResolved()
	res.Container.Image = ""
	res.Image = sources.ImageSource{Build: &sources.BuildPlan{Name: "a", Repo: "https://example.com/a.git", Ref: "main"}}
	return res
}

func TestRunnerBuildsWhenPlanned(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.useBuild()
	st, _ := h.ready()

	calls := h.bld.Calls()
	if len(calls) != 1 || calls[0].force || calls[0].plan.Repo != "https://example.com/a.git" {
		t.Fatalf("build calls = %+v, want one unforced build of the repo", calls)
	}
	if st.Image != h.bld.ref {
		t.Errorf("status image = %q, want %q", st.Image, h.bld.ref)
	}
	info, err := h.eng.InspectContainer(context.Background(), st.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Image != h.bld.ref {
		t.Errorf("container image = %q, want %q", info.Image, h.bld.ref)
	}
	if got := h.eng.Pulls(); len(got) != 0 {
		t.Errorf("Pulls = %v, want none for a built image", got)
	}
}

func TestRunnerRebuild(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.useBuild()
	st, _ := h.ready()

	h.bld.mu.Lock()
	h.bld.ref = "localhost/mcp-machine/a:def5678"
	h.bld.mu.Unlock()
	if err := h.m.Rebuild(context.Background(), "a"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	serveNext(t, h, st.ContainerID)
	got := waitFor(t, h.reg, "a", "ready on the rebuilt image", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateReady && s.Image == "localhost/mcp-machine/a:def5678"
	})
	calls := h.bld.Calls()
	if len(calls) != 2 || !calls[1].force {
		t.Errorf("build calls = %+v, want a second forced build", calls)
	}
	if got.ContainerID == st.ContainerID {
		t.Error("Rebuild reused the old container")
	}
	if got.Paused {
		t.Error("Rebuild left the server paused")
	}
	if n := h.count("CreateContainer"); n != 2 {
		t.Errorf("CreateContainer called %d times, want 2", n)
	}
}

func TestRebuildOnNonBuildServerDoesNotDisturbIt(t *testing.T) {
	h := newHarness(t, manager.Options{})
	ctx := context.Background()

	// During Connecting: the runner is busy dialing a backend that has not
	// answered yet. The rejected Rebuild must not cancel that dial.
	h.apply()
	waitState(t, h.reg, "a", store.StateConnecting)
	if err := h.m.Rebuild(ctx, "a"); err == nil {
		t.Fatal("Rebuild of a command server succeeded during Connecting, want an error")
	}
	serveBackend(t, h.eng, store.ContainerName("a"))
	st := waitState(t, h.reg, "a", store.StateReady)

	// During Ready: rejected without touching the container or the session.
	before := len(h.eng.Calls())
	if err := h.m.Rebuild(ctx, "a"); err == nil {
		t.Fatal("Rebuild of a command server succeeded during Ready, want an error")
	}
	if got, _ := h.reg.Get("a"); got.State != store.StateReady || got.ContainerID != st.ContainerID {
		t.Errorf("after rejected Rebuild status = %+v, want Ready on %s", got, st.ContainerID)
	}
	if calls := h.eng.Calls()[before:]; len(calls) != 0 {
		t.Errorf("rejected Rebuild made engine calls: %v", calls)
	}
	f, _ := h.m.Facade("a")
	if got := listTools(t, f.Server()); !slices.Contains(got, "ping") {
		t.Errorf("facade tools after rejected Rebuild = %v", got)
	}
	if n := len(h.bld.Calls()); n != 0 {
		t.Errorf("builder called %d times", n)
	}
}

func TestExitDuringPendingPreemptIsNotLost(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	// The exit and a command that turns out to be a no-op arrive together;
	// the exit must still be handled.
	h.eng.Exit(st.ContainerID, 1)
	if err := h.m.Rebuild(context.Background(), "a"); err == nil {
		t.Fatal("Rebuild of a command server succeeded, want an error")
	}
	waitFor(t, h.reg, "a", "exit failure", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "exited with code 1")
	})
	h.clk.Advance(time.Second)
	serveNext(t, h, st.ContainerID)
	if got := waitState(t, h.reg, "a", store.StateReady); got.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", got.Restarts)
	}
}

func TestSessionLossBeforeExitKeepsExitCode(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	h.eng.SetLogs(st.ContainerID, []runtime.LogLine{{Stream: "stderr", Text: "fatal: out of memory"}})

	// A real engine may close the attach stream before it records the exit.
	peer, ok := h.eng.Peer(st.ContainerID)
	if !ok {
		t.Fatal("no peer")
	}
	_ = peer.Close()
	waitFor(t, h.reg, "a", "session loss", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && s.Error == "backend session closed"
	})
	h.eng.Exit(st.ContainerID, 2)
	got := waitFor(t, h.reg, "a", "exit error", func(s store.Status, ok bool) bool {
		return ok && strings.HasPrefix(s.Error, "container exited")
	})
	if want := "container exited with code 2; last log lines: fatal: out of memory"; got.Error != want {
		t.Errorf("error = %q, want %q", got.Error, want)
	}
	if got.State != store.StateFailed {
		t.Errorf("state = %s, want failed", got.State)
	}
}

func TestRemoveWithExpiredContextStillDeletesRunner(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_ = h.m.Remove(ctx, "a") // may report ctx's error; the removal still completes

	deadline := time.Now().Add(3 * time.Second)
	for {
		_, inReg := h.reg.Get("a")
		_, hasFacade := h.m.Facade("a")
		if !inReg && !hasFacade {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner not forgotten: registry %v, facade %v", inReg, hasFacade)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := h.eng.InspectContainer(context.Background(), st.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("container not removed: %v", err)
	}

	h.apply()
	serveNext(t, h, st.ContainerID)
	waitState(t, h.reg, "a", store.StateReady)
}

func TestRunnerRestart(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	before := len(h.eng.Calls())

	if err := h.m.Restart(context.Background(), "a"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	serveBackend(t, h.eng, st.ContainerID)
	got := waitState(t, h.reg, "a", store.StateReady)
	if got.Paused {
		t.Error("Restart left the server paused")
	}
	calls := h.eng.Calls()[before:]
	if !inOrder(calls, []string{"StopContainer", "Attach", "StartContainer"}) {
		t.Errorf("calls after Restart = %v, want StopContainer, Attach, StartContainer", calls)
	}
	if slices.Contains(calls, "CreateContainer") {
		t.Errorf("Restart created a container: %v", calls)
	}
}

func TestManagerResync(t *testing.T) {
	h := newHarness(t, manager.Options{})
	_, backend := h.ready()
	f, _ := h.m.Facade("a")

	backend.AddTool(&mcp.Tool{Name: "pong", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	if got := listTools(t, f.Server()); slices.Contains(got, "pong") {
		t.Fatalf("pong mirrored before Resync: %v", got)
	}
	if err := h.m.Resync(context.Background(), "a"); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if got := listTools(t, f.Server()); !slices.Equal(got, []string{"ping", "pong"}) {
		t.Errorf("tools after Resync = %v, want [ping pong]", got)
	}
}

func TestManagerLogs(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	h.eng.SetLogs(st.ContainerID, []runtime.LogLine{{Stream: "stdout", Text: "one"}, {Stream: "stdout", Text: "two"}})

	lines, err := h.m.Logs(context.Background(), "a", 1)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Text != "two" {
		t.Errorf("Logs = %+v", lines)
	}
	if _, err := h.m.Logs(context.Background(), "nope", 1); !errors.Is(err, manager.ErrUnknownServer) {
		t.Errorf("Logs(unknown) err = %v", err)
	}
}

func TestManagerCloseLeavesContainersRunning(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := h.eng.InspectContainer(ctx, st.ContainerID)
	if err != nil || info.State != "running" {
		t.Errorf("after Close container = %+v, err %v; want running", info, err)
	}
	if err := h.m.Apply(ctx, h.cfg); !errors.Is(err, manager.ErrClosed) {
		t.Errorf("Apply after Close err = %v, want ErrClosed", err)
	}
}

func TestRunnerStopInterruptsHungDial(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.apply() // no backend is served: the dial blocks writing to the pipe
	waitState(t, h.reg, "a", store.StateConnecting)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.m.Stop(ctx, "a"); err != nil {
		t.Fatalf("Stop during a hung dial: %v", err)
	}
	if got, _ := h.reg.Get("a"); got.State != store.StateStopped || !got.Paused || got.Error != "" {
		t.Errorf("status = %+v, want Stopped and Paused without error", got)
	}
	if n := h.clk.Pending(); n != 0 {
		t.Errorf("%d retry timers pending after an interrupted dial, want 0", n)
	}
}
