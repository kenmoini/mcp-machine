package proxy_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/proxy"
)

func TestDialIOListsTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	near, far := net.Pipe()
	srvDone := make(chan error, 1)
	go func() { srvDone <- pingBackend().Run(ctx, &mcp.IOTransport{Reader: far, Writer: far}) }()

	cs, err := proxy.DialIO(ctx, near, nil)
	if err != nil {
		t.Fatalf("DialIO: %v", err)
	}
	listPing(t, cs)

	// The session owns the stream: closing it closes the connection, which
	// ends the backend's run loop.
	if err := cs.Close(); err != nil {
		t.Logf("session close: %v", err)
	}
	if _, err := near.Write([]byte("x")); err == nil {
		t.Error("stream still open after the session closed")
	}
	select {
	case err := <-srvDone:
		t.Logf("backend run ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("backend still running after the session closed")
	}
}

func TestDialIOFailsWhenStreamEndsBeforeInitialize(t *testing.T) {
	near, far := net.Pipe()
	_ = far.Close() // the backend is gone before it answers

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := proxy.DialIO(ctx, near, nil); err == nil {
		t.Fatal("DialIO over a dead stream should fail")
	}
}
