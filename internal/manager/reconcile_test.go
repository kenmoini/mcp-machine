package manager_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// edit replaces h.cfg with a copy whose first server fn has modified, so the
// config the manager holds is never mutated in place.
func (h *harness) edit(fn func(*config.ServerSpec)) {
	cfg := *h.cfg
	cfg.Servers = slices.Clone(h.cfg.Servers)
	fn(&cfg.Servers[0])
	h.cfg = &cfg
}

// hash is the desired hash of server a under the current config.
func (h *harness) hash() string {
	return config.Hash(h.cfg.Servers[0], h.cfg.Servers[0].RuntimeImage(h.cfg))
}

func boolPtr(b bool) *bool { return &b }

func TestApplyRecreatesOnHashChange(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	oldHash := st.Hash
	before := len(h.eng.Calls())

	h.edit(func(s *config.ServerSpec) { s.Env = map[string]string{"DEBUG": "1"} })
	h.apply()
	serveNext(t, h, st.ContainerID)
	got := waitState(t, h.reg, "a", store.StateReady)

	if calls := h.eng.Calls()[before:]; !inOrder(calls, []string{"RemoveContainer", "CreateContainer"}) {
		t.Errorf("calls after the change = %v, want RemoveContainer then CreateContainer", calls)
	}
	if got.Hash != h.hash() || got.Hash == oldHash {
		t.Errorf("status hash = %q, want the new hash %q (old %q)", got.Hash, h.hash(), oldHash)
	}
	info, err := h.eng.InspectContainer(context.Background(), got.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if lbl, ok := store.ParseLabels(info.Labels); !ok || lbl.Hash != h.hash() {
		t.Errorf("new container labels = %v, want hash %s", info.Labels, h.hash())
	}
	if _, err := h.eng.InspectContainer(context.Background(), st.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("old container not removed: %v", err)
	}
}

func TestApplyRemovesDroppedServer(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.cfg.Servers = append(h.cfg.Servers, config.ServerSpec{
		Name:    "b",
		Type:    "command",
		Command: &config.CommandSpec{Command: "npx", Args: []string{"-y", "other"}},
	})
	h.apply()
	serveBackend(t, h.eng, store.ContainerName("a"))
	serveBackend(t, h.eng, store.ContainerName("b"))
	stA := waitState(t, h.reg, "a", store.StateReady)
	stB := waitState(t, h.reg, "b", store.StateReady)
	before := len(h.eng.Calls())

	cfg := *h.cfg
	cfg.Servers = h.cfg.Servers[:1]
	h.cfg = &cfg
	h.apply()

	if got, ok := h.reg.Get("b"); ok {
		t.Errorf("registry still has b: %+v", got)
	}
	if _, ok := h.m.Facade("b"); ok {
		t.Error("Facade(b) still present")
	}
	if _, err := h.eng.InspectContainer(context.Background(), stB.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("b's container not removed: %v", err)
	}
	if got, _ := h.reg.Get("a"); got.State != store.StateReady || got.ContainerID != stA.ContainerID {
		t.Errorf("a disturbed: %+v", got)
	}
	if calls := h.eng.Calls()[before:]; slices.Contains(calls, "CreateContainer") {
		t.Errorf("dropping b created a container: %v", calls)
	}
}

func TestApplyDisabledStops(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()
	ctx := context.Background()

	h.edit(func(s *config.ServerSpec) { s.Enabled = boolPtr(false) })
	h.apply()
	got, ok := h.reg.Get("a")
	if !ok || got.State != store.StateStopped || got.Paused {
		t.Fatalf("after disable status = %+v (present %v), want Stopped and not Paused", got, ok)
	}
	if info, err := h.eng.InspectContainer(ctx, st.ContainerID); err != nil || info.State != "exited" {
		t.Errorf("container after disable = %+v, err %v; want it kept and exited", info, err)
	}
	if _, ok := h.m.Lookup("a"); ok {
		t.Error("Lookup(a) = true while disabled")
	}

	// Enabling it again resumes the same container.
	h.edit(func(s *config.ServerSpec) { s.Enabled = nil })
	h.apply()
	serveBackend(t, h.eng, st.ContainerID)
	got = waitState(t, h.reg, "a", store.StateReady)
	if got.ContainerID != st.ContainerID {
		t.Errorf("re-enable changed the container: %s -> %s", st.ContainerID, got.ContainerID)
	}
	if n := h.count("CreateContainer"); n != 1 {
		t.Errorf("CreateContainer called %d times, want 1", n)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.ready()
	ctx := context.Background()

	assertNoCalls := func(what string) {
		t.Helper()
		before := h.eng.Calls()
		h.apply()
		if after := h.eng.Calls(); !slices.Equal(before, after) {
			t.Errorf("%s: second Apply made engine calls %v", what, after[len(before):])
		}
	}
	assertNoCalls("ready")

	if err := h.m.Stop(ctx, "a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertNoCalls("paused")
	if got, _ := h.reg.Get("a"); got.State != store.StateStopped || !got.Paused {
		t.Errorf("paused server after Apply = %+v, want Stopped and Paused", got)
	}

	// Disabling a paused server clears the flag without touching the engine.
	h.edit(func(s *config.ServerSpec) { s.Enabled = boolPtr(false) })
	assertNoCalls("disable a paused server")
	if got, _ := h.reg.Get("a"); got.State != store.StateStopped || got.Paused {
		t.Errorf("disabled server = %+v, want Stopped and not Paused", got)
	}
	assertNoCalls("disabled")

	if n := h.count("ListContainers"); n != 1 {
		t.Errorf("ListContainers called %d times, want 1 (adoption runs on the first Apply only)", n)
	}
}

func TestAdoptHttpContainerInPlace(t *testing.T) {
	var (
		mu       sync.Mutex
		reg      *store.Registry
		gotInfo  []runtime.ContainerInfo
		gotState []store.State
	)
	h := newHarness(t, manager.Options{
		EndpointFunc: func(info runtime.ContainerInfo, _ sources.HTTPEndpoint) (string, error) {
			st, _ := reg.Get("a")
			mu.Lock()
			defer mu.Unlock()
			gotInfo = append(gotInfo, info)
			gotState = append(gotState, st.State)
			return "", errors.New("test: no endpoint")
		},
	})
	reg = h.reg
	h.res.res = httpResolved()
	h.eng.Seed(runtime.ContainerInfo{
		ID:     "seed-1",
		Name:   store.ContainerName("a"),
		Image:  "img:1",
		State:  "running",
		Labels: store.Labels("a", h.hash(), "http"),
		Ports:  []runtime.PortMapping{{HostIP: "127.0.0.1", HostPort: 41234, ContainerPort: 3000, Protocol: "tcp"}},
	})
	h.apply()

	st := waitFor(t, h.reg, "a", "endpoint failure", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateFailed && strings.Contains(s.Error, "test: no endpoint")
	})
	if st.ContainerID != "seed-1" {
		t.Errorf("ContainerID = %q, want seed-1", st.ContainerID)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotInfo) != 1 || gotInfo[0].ID != "seed-1" {
		t.Fatalf("EndpointFunc calls = %+v, want one for seed-1", gotInfo)
	}
	if gotState[0] != store.StateConnecting {
		t.Errorf("state while resolving the endpoint = %s, want connecting", gotState[0])
	}
	for _, m := range []string{"CreateContainer", "StartContainer", "StopContainer", "RemoveContainer", "Attach"} {
		if n := h.count(m); n != 0 {
			t.Errorf("%s called %d times adopting a running http container", m, n)
		}
	}
}

func TestAdoptStdioContainerRestarts(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.eng.Seed(runtime.ContainerInfo{
		ID:     "seed-1",
		Name:   store.ContainerName("a"),
		Image:  "img:1",
		State:  "exited",
		Labels: store.Labels("a", h.hash(), "stdio"),
	})
	h.apply()
	serveBackend(t, h.eng, "seed-1")
	st := waitState(t, h.reg, "a", store.StateReady)

	if st.ContainerID != "seed-1" {
		t.Errorf("ContainerID = %q, want seed-1", st.ContainerID)
	}
	calls := h.eng.Calls()
	if !inOrder(calls, []string{"ListContainers", "Attach", "StartContainer"}) {
		t.Errorf("calls = %v, want ListContainers, Attach, StartContainer", calls)
	}
	for _, m := range []string{"CreateContainer", "StopContainer", "RemoveContainer"} {
		if slices.Contains(calls, m) {
			t.Errorf("adopting an exited stdio container called %s: %v", m, calls)
		}
	}
}

func TestAdoptStdioRunningIsStoppedFirst(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.eng.Seed(runtime.ContainerInfo{
		ID:     "seed-1",
		Name:   store.ContainerName("a"),
		Image:  "img:1",
		State:  "running",
		Labels: store.Labels("a", h.hash(), "stdio"),
	})
	h.apply()
	serveBackend(t, h.eng, "seed-1")
	st := waitState(t, h.reg, "a", store.StateReady)

	if st.ContainerID != "seed-1" {
		t.Errorf("ContainerID = %q, want seed-1", st.ContainerID)
	}
	calls := h.eng.Calls()
	if !inOrder(calls, []string{"ListContainers", "StopContainer", "Attach", "StartContainer"}) {
		t.Errorf("calls = %v, want ListContainers, StopContainer, Attach, StartContainer", calls)
	}
	if slices.Contains(calls, "CreateContainer") || slices.Contains(calls, "RemoveContainer") {
		t.Errorf("adoption replaced the container: %v", calls)
	}
}

func TestAdoptStaleHashRecreates(t *testing.T) {
	h := newHarness(t, manager.Options{})
	ctx := context.Background()
	h.eng.Seed(runtime.ContainerInfo{ // server a, but created for another spec
		ID: "seed-1", Name: store.ContainerName("a"), Image: "img:0", State: "running",
		Labels: store.Labels("a", "stale", "stdio"),
	})
	h.eng.Seed(runtime.ContainerInfo{ // a server no longer configured
		ID: "seed-2", Name: store.ContainerName("gone"), Image: "img:1", State: "exited",
		Labels: store.Labels("gone", "whatever", "stdio"),
	})
	h.eng.Seed(runtime.ContainerInfo{ // marked managed but unparseable: not ours to touch
		ID: "seed-3", Name: "stray", Image: "img:1", State: "running",
		Labels: map[string]string{store.LabelManaged: "true"},
	})
	h.apply()
	serveNext(t, h, "seed-1")
	st := waitState(t, h.reg, "a", store.StateReady)

	if st.ContainerID == "seed-1" {
		t.Fatal("the stale container was adopted")
	}
	for _, id := range []string{"seed-1", "seed-2"} {
		if _, err := h.eng.InspectContainer(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
			t.Errorf("%s not removed: %v", id, err)
		}
	}
	if _, err := h.eng.InspectContainer(ctx, "seed-3"); err != nil {
		t.Errorf("unparseable container was removed: %v", err)
	}
	if calls := h.eng.Calls(); !inOrder(calls, []string{"ListContainers", "StopContainer", "RemoveContainer", "CreateContainer"}) {
		t.Errorf("calls = %v, want the stale container stopped and removed before CreateContainer", calls)
	}
	if st.Hash != h.hash() {
		t.Errorf("hash = %q, want %q", st.Hash, h.hash())
	}
}

func TestPausedSurvivesReload(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	if err := h.m.Stop(context.Background(), "a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	before := len(h.eng.Calls())
	h.apply()
	if got, _ := h.reg.Get("a"); got.State != store.StateStopped || !got.Paused {
		t.Errorf("after reload status = %+v, want Stopped and Paused", got)
	}
	if calls := h.eng.Calls()[before:]; len(calls) != 0 {
		t.Errorf("reload of a paused server made engine calls: %v", calls)
	}

	h.edit(func(s *config.ServerSpec) { s.Env = map[string]string{"DEBUG": "1"} })
	h.apply()
	serveNext(t, h, st.ContainerID)
	got := waitState(t, h.reg, "a", store.StateReady)
	if got.Paused {
		t.Error("recreated server still Paused")
	}
	if got.Hash != h.hash() {
		t.Errorf("hash = %q, want %q", got.Hash, h.hash())
	}
}

func TestApplyAfterRemoveRecreates(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	if err := h.m.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	h.apply()
	serveNext(t, h, st.ContainerID)
	if got := waitState(t, h.reg, "a", store.StateReady); got.Paused {
		t.Errorf("recreated server is Paused: %+v", got)
	}
}

func TestApplyDuringPendingRemoveRecreates(t *testing.T) {
	h := newHarness(t, manager.Options{})
	st, _ := h.ready()

	// The Remove is queued but its caller gives up at once; the Apply that
	// follows must wait for it rather than mistake the runner for a live one.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = h.m.Remove(ctx, "a")
	h.apply()
	serveNext(t, h, st.ContainerID)
	if got := waitState(t, h.reg, "a", store.StateReady); got.Paused {
		t.Errorf("recreated server is Paused: %+v", got)
	}
	if _, err := h.eng.InspectContainer(context.Background(), st.ContainerID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("old container not removed: %v", err)
	}
}

func TestAdoptRetriesAfterListFailure(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.eng.Seed(runtime.ContainerInfo{
		ID:     "seed-1",
		Name:   store.ContainerName("a"),
		Image:  "img:1",
		State:  "exited",
		Labels: store.Labels("a", h.hash(), "stdio"),
	})
	h.eng.FailNext("ListContainers", errors.New("socket not ready"))
	if err := h.m.Apply(context.Background(), h.cfg); err == nil || !strings.Contains(err.Error(), "socket not ready") {
		t.Fatalf("first Apply err = %v, want the list failure", err)
	}
	if _, ok := h.m.Facade("a"); ok {
		t.Error("a runner was started although the containers could not be listed")
	}
	if n := h.count("CreateContainer"); n != 0 {
		t.Errorf("CreateContainer called %d times after the failed scan", n)
	}

	h.apply()
	serveBackend(t, h.eng, "seed-1")
	st := waitState(t, h.reg, "a", store.StateReady)
	if st.ContainerID != "seed-1" {
		t.Errorf("ContainerID = %q, want the adopted seed-1", st.ContainerID)
	}
	if n := h.count("ListContainers"); n != 2 {
		t.Errorf("ListContainers called %d times, want 2", n)
	}
	if n := h.count("CreateContainer"); n != 0 {
		t.Errorf("CreateContainer called %d times, want 0", n)
	}
}

func TestCreateReplacesConflictingContainer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"unlabelled", nil},
		{"managed-without-server", map[string]string{store.LabelManaged: "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, manager.Options{})
			h.eng.Seed(runtime.ContainerInfo{
				ID: "seed-1", Name: store.ContainerName("a"), Image: "img:0", State: "running",
				Labels: tc.labels,
			})
			h.apply()
			serveNext(t, h, "seed-1")
			st := waitState(t, h.reg, "a", store.StateReady)
			if _, err := h.eng.InspectContainer(context.Background(), "seed-1"); !errors.Is(err, runtime.ErrNotFound) {
				t.Errorf("conflicting container not removed: %v", err)
			}
			if st.ContainerID == "seed-1" {
				t.Error("conflicting container was used")
			}
		})
	}
}
