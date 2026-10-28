package store_test

import (
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/store"
)

func TestRegistrySetGetListSorted(t *testing.T) {
	r := store.NewRegistry()
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get on empty registry ok = true")
	}
	r.Set(store.Status{Name: "b", State: store.StateReady})
	r.Set(store.Status{Name: "c", State: store.StateFailed, Error: "boom"})
	r.Set(store.Status{Name: "a", State: store.StateRunning, Restarts: 2})
	r.Set(store.Status{Name: "b", State: store.StateStopped}) // replace

	got, ok := r.Get("b")
	if !ok || got.State != store.StateStopped {
		t.Errorf("Get(b) = %+v ok=%v", got, ok)
	}
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("len(List) = %d, want 3", len(list))
	}
	for i, n := range []string{"a", "b", "c"} {
		if list[i].Name != n {
			t.Errorf("List[%d].Name = %q, want %q", i, list[i].Name, n)
		}
	}

	r.Delete("b")
	r.Delete("unknown") // no-op
	if _, ok := r.Get("b"); ok {
		t.Error("b still present after Delete")
	}
	if len(r.List()) != 2 {
		t.Errorf("len(List) = %d, want 2", len(r.List()))
	}
}

func TestRegistryCopiesOnSetAndGet(t *testing.T) {
	r := store.NewRegistry()
	s := store.Status{Name: "a", State: store.StateReady, Image: "img1"}
	r.Set(s)
	s.Image = "mutated"

	got, _ := r.Get("a")
	if got.Image != "img1" {
		t.Errorf("Set aliased caller value: Image = %q", got.Image)
	}
	got.Image = "mutated-get"
	again, _ := r.Get("a")
	if again.Image != "img1" {
		t.Errorf("Get aliased stored value: Image = %q", again.Image)
	}
	l := r.List()
	l[0].Image = "mutated-list"
	again, _ = r.Get("a")
	if again.Image != "img1" {
		t.Errorf("List aliased stored value: Image = %q", again.Image)
	}
}

func TestRegistrySubscribeReceivesAndUnsubscribes(t *testing.T) {
	r := store.NewRegistry()
	r.Set(store.Status{Name: "old", State: store.StateReady})

	ch1, cancel1 := r.Subscribe()
	ch2, cancel2 := r.Subscribe()
	defer cancel2()

	select {
	case s := <-ch1:
		t.Fatalf("received history: %+v", s)
	default:
	}

	r.Set(store.Status{Name: "a", State: store.StateBuilding})
	for i, ch := range []<-chan store.Status{ch1, ch2} {
		select {
		case s := <-ch:
			if s.Name != "a" || s.State != store.StateBuilding {
				t.Errorf("sub %d got %+v", i, s)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d timed out", i)
		}
	}

	cancel1()
	cancel1() // idempotent
	if _, ok := <-ch1; ok {
		t.Error("ch1 not closed after cancel")
	}
	r.Set(store.Status{Name: "b", State: store.StateReady})
	select {
	case s := <-ch2:
		if s.Name != "b" {
			t.Errorf("ch2 got %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 timed out after ch1 cancel")
	}
}

func TestRegistrySubscribeDropsWhenFull(t *testing.T) {
	r := store.NewRegistry()
	ch, cancel := r.Subscribe()
	defer cancel()
	for i := 0; i < 16; i++ {
		r.Set(store.Status{Name: "a", Restarts: i})
	}
	done := make(chan struct{})
	go func() {
		r.Set(store.Status{Name: "a", Restarts: 99})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Set blocked on a full subscriber")
	}
	n := 0
	for len(ch) > 0 {
		s := <-ch
		if s.Restarts == 99 {
			t.Error("overflow event should have been dropped")
		}
		n++
	}
	if n != 16 {
		t.Errorf("drained %d, want 16", n)
	}
}

func TestRegistryCountByState(t *testing.T) {
	r := store.NewRegistry()
	r.Set(store.Status{Name: "a", State: store.StateReady})
	r.Set(store.Status{Name: "b", State: store.StateReady})
	r.Set(store.Status{Name: "c", State: store.StateFailed})
	r.Set(store.Status{Name: "d", State: store.StateRunning})
	r.Set(store.Status{Name: "d", State: store.StateReady})
	r.Delete("c")
	got := r.CountByState()
	if got[store.StateReady] != 3 || got[store.StateFailed] != 0 || len(got) != 1 {
		t.Errorf("CountByState = %v", got)
	}
}
