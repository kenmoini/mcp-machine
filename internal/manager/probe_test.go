package manager

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Port 1 on loopback is closed (or, in a sandbox, not even dialable): either
// way every attempt fails and the probe must keep polling until it times out.
const unreachable = "http://127.0.0.1:1/mcp"

func TestProbeTimesOut(t *testing.T) {
	start := time.Now()
	err := probe(context.Background(), unreachable, 300*time.Millisecond, 50*time.Millisecond)
	if err == nil || err.Error() != "probe "+unreachable+": timed out after 300ms" {
		t.Fatalf("err = %v, want the timeout error", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 3*time.Second {
		t.Errorf("probe returned after %s, want about 300ms", d)
	}
}

func TestProbeHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err := probe(ctx, unreachable, time.Minute, 50*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("probe returned after %s, want soon after the cancel", d)
	}
}
