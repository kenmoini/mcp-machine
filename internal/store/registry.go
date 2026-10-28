package store

import (
	"sort"
	"sync"
	"time"
)

// State is the lifecycle state of a managed server.
type State string

// Lifecycle states.
const (
	StateDefined    State = "defined"
	StateResolving  State = "resolving"
	StateBuilding   State = "building"
	StateImageReady State = "image-ready"
	StateCreating   State = "creating"
	StateRunning    State = "running"
	StateConnecting State = "connecting"
	StateReady      State = "ready"
	StateStopped    State = "stopped"
	StateFailed     State = "failed"
	StateRemoving   State = "removing"
)

// subscriberBuffer is the channel capacity of each subscriber.
const subscriberBuffer = 16

// Status is the observable state of one managed server.
type Status struct {
	Name        string
	State       State
	Hash        string
	ContainerID string
	Image       string
	Error       string
	Since       time.Time
	Restarts    int
	Paused      bool
}

// Registry is a concurrency-safe in-memory map of server statuses with
// change notification.
type Registry struct {
	mu     sync.Mutex
	items  map[string]Status
	subs   map[uint64]chan Status
	nextID uint64
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		items: make(map[string]Status),
		subs:  make(map[uint64]chan Status),
	}
}

// Set stores s (by value) and notifies subscribers without blocking; a
// subscriber whose buffer is full misses the event.
func (r *Registry) Set(s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[s.Name] = s
	for _, ch := range r.subs {
		select {
		case ch <- s:
		default:
		}
	}
}

// Get returns a copy of the status for name.
func (r *Registry) Get(name string) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.items[name]
	return s, ok
}

// List returns copies of all statuses sorted by Name.
func (r *Registry) List() []Status {
	r.mu.Lock()
	out := make([]Status, 0, len(r.items))
	for _, s := range r.items {
		out = append(out, s)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete removes name; unknown names are ignored.
func (r *Registry) Delete(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, name)
}

// Subscribe returns a channel of subsequent Set events (no history) and a
// cancel func that unsubscribes and closes the channel. cancel is idempotent.
func (r *Registry) Subscribe() (<-chan Status, func()) {
	ch := make(chan Status, subscriberBuffer)
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	r.subs[id] = ch
	r.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, id)
			close(ch)
			r.mu.Unlock()
		})
	}
	return ch, cancel
}

// CountByState returns the number of servers in each state present.
func (r *Registry) CountByState() map[State]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[State]int)
	for _, s := range r.items {
		out[s.State]++
	}
	return out
}
