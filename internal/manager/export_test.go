package manager

import "context"

// White-box handles for the external tests: they reach the runner's command
// queue, which cannot be filled deterministically through the Manager API.

// Runner is the per-server runner, exposed to tests.
type Runner = runner

// Kind is a runner command kind, exposed to tests.
type Kind = evKind

const (
	KindStart   = evStart
	KindStop    = evStop
	KindRemove  = evRemove
	KindRecover = evRecover
)

// Runner returns the live runner for name.
func (m *Manager) Runner(name string) (*Runner, error) { return m.runner(name) }

// Send is the runner's command entry point.
func (r *Runner) Send(ctx context.Context, kind Kind) error { return r.send(ctx, kind) }

// Done is closed when the runner goroutine has returned.
func (r *Runner) Done() <-chan struct{} { return r.done }

// FillQueue enqueues no-op recover events until the command queue is full
// and returns how many were queued. It never blocks.
func (r *Runner) FillQueue() int {
	n := 0
	for {
		select {
		case r.events <- event{kind: evRecover}:
			n++
		default:
			return n
		}
	}
}

// Counters returns the in-flight preempting commands and queued removals.
func (r *Runner) Counters() (pending, removals int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending, r.removals
}

func (r *Runner) Interrupted() bool { return r.interrupted() }
func (r *Runner) BeginRemoval()     { r.beginRemoval() }
func (r *Runner) EndRemoval()       { r.endRemoval() }

func (r *Runner) RemovalPending() (<-chan struct{}, bool) { return r.removalPending() }

// Gone is the error a command gets when the runner has ended.
func (r *Runner) Gone() error { return r.gone() }
