package manager_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// hungRunner applies the config with no backend served, so the runner stays
// busy in a dial that never completes, and returns it with the container it
// is dialing.
func hungRunner(t *testing.T, h *harness) (*manager.Runner, string) {
	t.Helper()
	h.apply()
	st := waitState(t, h.reg, "a", store.StateConnecting)
	r, err := h.m.Runner("a")
	if err != nil {
		t.Fatal(err)
	}
	return r, st.ContainerID
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// A preempting command whose send is withdrawn (queue full, ctx over) has
// already cancelled the runner's attempt; the runner must resume it.
func TestWithdrawnPreemptingCommandResumesInterruptedAttempt(t *testing.T) {
	h := newHarness(t, manager.Options{})
	r, firstID := hungRunner(t, h)

	if n := r.FillQueue(); n == 0 {
		t.Fatal("could not queue anything behind the busy runner")
	}
	err := h.m.Stop(cancelledCtx(), "a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop with a cancelled ctx and a full queue = %v, want context.Canceled", err)
	}
	// The withdrawn command no longer counts as in flight.
	if pending, _ := r.Counters(); pending != 0 {
		t.Fatalf("pending = %d after withdraw, want 0", pending)
	}
	if r.Interrupted() {
		t.Fatal("runner still reports an interrupt after withdraw")
	}

	// The attempt it cancelled is re-run on a fresh container; the server is
	// neither stopped nor paused.
	serveNext(t, h, firstID)
	got := waitFor(t, h.reg, "a", "ready on a new container", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateReady && s.ContainerID != firstID
	})
	if got.Paused {
		t.Errorf("server paused by a withdrawn Stop: %+v", got)
	}
	if n := h.count("CreateContainer"); n != 2 {
		t.Errorf("CreateContainer called %d times, want 2 (original attempt + resumed attempt)", n)
	}
	if n := h.count("StopContainer"); n != 0 {
		t.Errorf("withdrawn Stop stopped a container (%d StopContainer calls)", n)
	}
}

func TestWithdrawnRemoveReleasesRemovalAndKeepsServer(t *testing.T) {
	h := newHarness(t, manager.Options{})
	r, firstID := hungRunner(t, h)

	r.FillQueue()
	if err := h.m.Remove(cancelledCtx(), "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove with a cancelled ctx and a full queue = %v, want context.Canceled", err)
	}
	pending, removals := r.Counters()
	if pending != 0 || removals != 0 {
		t.Fatalf("pending=%d removals=%d after withdraw, want 0, 0", pending, removals)
	}
	if _, ok := r.RemovalPending(); ok {
		t.Fatal("withdrawn Remove still reported as pending")
	}

	// Apply must not wait for a removal that was never queued.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.m.Apply(ctx, h.cfg); err != nil {
		t.Fatalf("Apply after a withdrawn Remove: %v", err)
	}
	serveNext(t, h, firstID)
	waitFor(t, h.reg, "a", "ready on a new container", func(s store.Status, ok bool) bool {
		return ok && s.State == store.StateReady && s.ContainerID != firstID
	})
	if n := h.count("RemoveContainer"); n != 1 {
		// only the resumed attempt replacing the abandoned container
		t.Errorf("RemoveContainer called %d times, want 1", n)
	}
}

func TestCommandToEndedRunnerReportsUnknownServer(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.ready()
	r, err := h.m.Runner("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.Remove(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	ctx := context.Background()

	// Room in the queue: the command is queued, then the ended runner is noticed.
	if err := r.Send(ctx, manager.KindStart); !errors.Is(err, manager.ErrUnknownServer) {
		t.Fatalf("Send to an ended runner = %v, want ErrUnknownServer", err)
	}
	if want := r.Gone().Error(); !strings.Contains(want, `"a"`) {
		t.Errorf("gone error %q does not name the server", want)
	}

	// Queue full: never queued, so the interrupt and removal it announced are undone.
	r.FillQueue()
	pendingBefore, _ := r.Counters()
	if err := r.Send(ctx, manager.KindStop); !errors.Is(err, manager.ErrUnknownServer) {
		t.Fatalf("Stop to an ended runner with a full queue = %v, want ErrUnknownServer", err)
	}
	if pending, _ := r.Counters(); pending != pendingBefore {
		t.Errorf("pending = %d, want %d: the withdrawn Stop leaked its interrupt", pending, pendingBefore)
	}
	if err := r.Send(ctx, manager.KindStart); !errors.Is(err, manager.ErrUnknownServer) {
		t.Fatalf("Start to an ended runner with a full queue = %v, want ErrUnknownServer", err)
	}
	if pending, _ := r.Counters(); pending != pendingBefore {
		t.Errorf("pending = %d, want %d: a non-preempting Start must not touch it", pending, pendingBefore)
	}
	r.BeginRemoval()
	_, removalsBefore := r.Counters()
	if err := r.Send(ctx, manager.KindRemove); !errors.Is(err, manager.ErrUnknownServer) {
		t.Fatalf("Remove to an ended runner with a full queue = %v, want ErrUnknownServer", err)
	}
	if _, removals := r.Counters(); removals != removalsBefore-1 {
		t.Errorf("removals = %d, want %d: the withdrawn Remove did not release its slot", removals, removalsBefore-1)
	}
}

func TestFailedRemoveReleasesRemovalSlot(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.ready()
	r, err := h.m.Runner("a")
	if err != nil {
		t.Fatal(err)
	}

	h.eng.FailNext("RemoveContainer", errors.New("device busy"))
	err = h.m.Remove(context.Background(), "a")
	if err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("Remove = %v, want the engine error", err)
	}
	if _, removals := r.Counters(); removals != 0 {
		t.Fatalf("removals = %d after a failed Remove, want 0", removals)
	}
	if _, ok := r.RemovalPending(); ok {
		t.Fatal("failed Remove still reported as pending")
	}
	if st, _ := h.reg.Get("a"); st.State != store.StateFailed {
		t.Fatalf("state after failed Remove = %s, want failed", st.State)
	}

	// The runner lives on and can still be removed.
	if err := h.m.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	<-r.Done()
	if _, ok := h.reg.Get("a"); ok {
		t.Error("server still in the registry after Remove")
	}
}

func TestRemovalIdleChannelClosesWhenLastRemovalEnds(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.ready()
	r, err := h.m.Runner("a")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := r.RemovalPending(); ok {
		t.Fatal("RemovalPending before any Remove")
	}
	r.BeginRemoval()
	r.BeginRemoval()
	idle, ok := r.RemovalPending()
	if !ok {
		t.Fatal("RemovalPending false with removals queued")
	}
	r.EndRemoval()
	select {
	case <-idle:
		t.Fatal("idle closed while a removal is still pending")
	default:
	}
	r.EndRemoval()
	select {
	case <-idle:
	default:
		t.Fatal("idle not closed after the last removal ended")
	}
	if _, ok := r.RemovalPending(); ok {
		t.Fatal("RemovalPending true after all removals ended")
	}
}

// Rebuild preempts a runner stuck in its first Resolve. The Rebuild is then
// rejected because the second Resolve fails; nothing is left to settle the
// interrupted attempt, so the runner re-runs it.
func TestRejectedRebuildResumesAbandonedAttempt(t *testing.T) {
	h := newHarness(t, manager.Options{})
	h.useBuild()
	entered := make(chan struct{})
	h.res.hook = func(ctx context.Context, n int) error {
		switch n {
		case 1:
			close(entered)
			<-ctx.Done() // hang until the Rebuild interrupts the attempt
			return ctx.Err()
		case 2:
			return errors.New("resolver down")
		}
		return nil
	}
	h.apply()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first Resolve never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := h.m.Rebuild(ctx, "a")
	if err == nil || !strings.Contains(err.Error(), "resolve: resolver down") {
		t.Fatalf("Rebuild = %v, want the resolve failure", err)
	}

	// Third Resolve succeeds: the abandoned launch ran again, unforced.
	waitFor(t, h.reg, "a", "a container", func(s store.Status, ok bool) bool { return ok && s.ContainerID != "" })
	serveBackend(t, h.eng, store.ContainerName("a"))
	got := waitState(t, h.reg, "a", store.StateReady)
	if got.Paused {
		t.Errorf("server paused by a rejected Rebuild: %+v", got)
	}
	h.res.mu.Lock()
	resolves := h.res.calls
	h.res.mu.Unlock()
	if resolves != 3 {
		t.Errorf("Resolve called %d times, want 3 (hung, failed rebuild plan, resumed launch)", resolves)
	}
	calls := h.bld.Calls()
	if len(calls) != 1 || calls[0].force {
		t.Errorf("build calls = %+v, want one unforced build from the resumed launch", calls)
	}
}

func TestRebuildPlanErrors(t *testing.T) {
	t.Run("build-type server whose resolve has no build plan", func(t *testing.T) {
		h := newHarness(t, manager.Options{})
		h.useBuild()
		h.res.res = stdioResolved() // pulled image: nothing to rebuild
		st, _ := h.ready()

		before := len(h.eng.Calls())
		err := h.m.Rebuild(context.Background(), "a")
		if err == nil || !strings.Contains(err.Error(), "nothing to rebuild") {
			t.Fatalf("Rebuild = %v, want a 'nothing to rebuild' error", err)
		}
		if got, _ := h.reg.Get("a"); got.State != store.StateReady || got.ContainerID != st.ContainerID {
			t.Errorf("status after rejected Rebuild = %+v, want Ready on %s", got, st.ContainerID)
		}
		if calls := h.eng.Calls()[before:]; len(calls) != 0 {
			t.Errorf("rejected Rebuild made engine calls: %v", calls)
		}
		if n := len(h.bld.Calls()); n != 0 {
			t.Errorf("builder called %d times", n)
		}
	})

	t.Run("resolve failure", func(t *testing.T) {
		h := newHarness(t, manager.Options{})
		h.useBuild()
		h.res.err = errors.New("repo unreachable")
		h.apply()
		waitState(t, h.reg, "a", store.StateFailed)

		err := h.m.Rebuild(context.Background(), "a")
		if err == nil || !strings.Contains(err.Error(), "resolve: repo unreachable") {
			t.Fatalf("Rebuild = %v, want the resolve failure", err)
		}
		if got, _ := h.reg.Get("a"); got.State != store.StateFailed || got.ContainerID != "" {
			t.Errorf("status after rejected Rebuild = %+v, want still Failed without a container", got)
		}
		if n := h.count("CreateContainer"); n != 0 {
			t.Errorf("CreateContainer called %d times", n)
		}
	})
}
