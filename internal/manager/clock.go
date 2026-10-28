package manager

import "time"

// Clock is the time source for status timestamps and retry backoff. Tests
// substitute a manual clock so retries fire only when they say so.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
