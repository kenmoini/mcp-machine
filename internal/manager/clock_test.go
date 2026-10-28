package manager

import (
	"testing"
	"time"
)

func TestRealClock(t *testing.T) {
	var c Clock = realClock{}
	before := time.Now()
	if now := c.Now(); now.Before(before) || time.Since(now) > time.Minute {
		t.Fatalf("Now() = %v, not the wall clock (started %v)", now, before)
	}
	select {
	case at := <-c.After(time.Millisecond):
		if at.Before(before) {
			t.Fatalf("After fired with %v, before it was requested at %v", at, before)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("After(1ms) never fired")
	}
}
