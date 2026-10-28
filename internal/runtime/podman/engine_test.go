package podman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"go.podman.io/podman/v6/pkg/errorhandling"
)

func TestConnWriteFailsAfterAttachEnds(t *testing.T) {
	c := newConn(func() {}, nil)
	c.finish(nil) // the attach goroutine returned

	done := make(chan error, 1)
	go func() { _, err := c.Write([]byte("x")); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Write should fail after the attachment ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked after the attachment ended")
	}
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read = %v, want EOF", err)
	}
}

func TestConnFinishUnblocksPendingWrite(t *testing.T) {
	c := newConn(func() {}, nil)
	done := make(chan error, 1)
	go func() { _, err := c.Write([]byte("x")); done <- err }() // no reader: blocks
	time.Sleep(20 * time.Millisecond)
	c.finish(errors.New("boom"))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending Write should fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending Write not unblocked")
	}
}

func TestConnCloseIdempotent(t *testing.T) {
	cancels := 0
	c := newConn(func() { cancels++ }, &connTracker{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if cancels != 1 {
		t.Fatalf("cancel called %d times, want 1", cancels)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close should fail")
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after Close should fail")
	}
}

func TestConnTrackerCloseAll(t *testing.T) {
	tr := &connTracker{}
	a, b := net.Pipe()
	defer b.Close()
	dial := tr.wrap(func(_ context.Context, _, _ string) (net.Conn, error) { return a, nil })
	if _, err := dial(context.Background(), "unix", "x"); err != nil {
		t.Fatal(err)
	}
	tr.closeAll()
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("tracked conn should be closed")
	}
	// dials after closeAll are refused and closed
	c, d := net.Pipe()
	defer d.Close()
	dial2 := tr.wrap(func(_ context.Context, _, _ string) (net.Conn, error) { return c, nil })
	if _, err := dial2(context.Background(), "unix", "x"); err == nil {
		t.Fatal("dial after closeAll should fail")
	}
	var nilTracker *connTracker
	nilTracker.closeAll() // must not panic
}

func TestStopTimeoutClamp(t *testing.T) {
	cases := map[time.Duration]uint{
		-5 * time.Second:   0,
		0:                  0,
		time.Millisecond:   1,
		10 * time.Second:   10,
		10*time.Second + 1: 11,
	}
	for in, want := range cases {
		if got := stopSeconds(in); got != want {
			t.Errorf("stopSeconds(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestIsAlreadyExists(t *testing.T) {
	if !isAlreadyExists(&errorhandling.ErrorModel{Message: "x", ResponseCode: http.StatusConflict}) {
		t.Error("409 should count")
	}
	if !isAlreadyExists(fmt.Errorf("wrap: %w", errors.New("network mcp already exists"))) {
		t.Error("message match should count")
	}
	if isAlreadyExists(nil) || isAlreadyExists(errors.New("boom")) {
		t.Error("unrelated errors should not count")
	}
}

func TestContainerfilePath(t *testing.T) {
	ctx := filepath.Join(string(filepath.Separator), "work", "repo", "sub")
	for _, tc := range []struct{ in, want string }{
		{"", filepath.Join(ctx, "Containerfile")},
		{"Containerfile", filepath.Join(ctx, "Containerfile")},
		{"build/Dockerfile", filepath.Join(ctx, "build", "Dockerfile")},
		{filepath.Join(string(filepath.Separator), "abs", "Containerfile"), filepath.Join(string(filepath.Separator), "abs", "Containerfile")},
	} {
		if got := containerfilePath(ctx, tc.in); got != tc.want {
			t.Errorf("containerfilePath(%q, %q) = %q, want %q", ctx, tc.in, got, tc.want)
		}
	}
}
