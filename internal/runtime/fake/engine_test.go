package fake_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/fake"
)

var _ runtime.Engine = (*fake.Engine)(nil)

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, err := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a", Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "fake-1" {
		t.Fatalf("id = %q, want fake-1", id)
	}
	if _, err := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"}); err == nil {
		t.Fatal("duplicate name should fail")
	}
	state := func() string {
		t.Helper()
		info, err := e.InspectContainer(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return info.State
	}
	if s := state(); s != "created" {
		t.Fatalf("state = %q, want created", s)
	}
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != "running" {
		t.Fatalf("state = %q, want running", s)
	}
	if err := e.RemoveContainer(ctx, id, false); err == nil {
		t.Fatal("removing running container without force should fail")
	}
	if err := e.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != "exited" {
		t.Fatalf("state = %q, want exited", s)
	}
	// look-up by name works too
	if info, err := e.InspectContainer(ctx, "a"); err != nil || info.ID != id {
		t.Fatalf("inspect by name = %+v, %v", info, err)
	}
	if err := e.RemoveContainer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.InspectContainer(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("inspect after remove err = %v, want ErrNotFound", err)
	}
	if err := e.RemoveContainer(ctx, id, true); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("remove missing err = %v, want ErrNotFound", err)
	}
}

func TestFakeForceRemoveRunning(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	_ = e.StartContainer(ctx, id)
	if err := e.RemoveContainer(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.InspectContainer(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFakeListFiltersByLabels(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	_, _ = e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a", Labels: map[string]string{"k": "1", "x": "y"}})
	_, _ = e.CreateContainer(ctx, runtime.ContainerSpec{Name: "b", Labels: map[string]string{"k": "2"}})
	_, _ = e.CreateContainer(ctx, runtime.ContainerSpec{Name: "c"})

	all, err := e.ListContainers(ctx, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d, %v", len(all), err)
	}
	got, _ := e.ListContainers(ctx, map[string]string{"k": "1"})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("k=1 -> %+v", got)
	}
	got, _ = e.ListContainers(ctx, map[string]string{"k": "1", "x": "z"})
	if len(got) != 0 {
		t.Fatalf("k=1,x=z -> %+v", got)
	}
	got, _ = e.ListContainers(ctx, map[string]string{})
	if len(got) != 3 {
		t.Fatalf("empty filter -> %d", len(got))
	}
}

func TestFakeAttachPipe(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a", Stdin: true})
	conn, err := e.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := e.Peer(id)
	if !ok {
		t.Fatal("Peer not found")
	}
	if _, err := e.Attach(ctx, id); err == nil {
		t.Fatal("second Attach should fail")
	}

	go func() { _, _ = peer.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read = %q, %v", buf, err)
	}
	go func() { _, _ = conn.Write([]byte("pong")) }()
	if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("read = %q, %v", buf, err)
	}

	// Stop closes the pipe: far end sees EOF.
	_ = e.StartContainer(ctx, id)
	if err := e.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after stop err = %v, want EOF", err)
	}
}

func TestFakeAttachErrors(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	if _, err := e.Attach(ctx, "nope"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	if _, ok := e.Peer(id); ok {
		t.Fatal("Peer should not exist before Attach")
	}
	_ = e.StartContainer(ctx, id)
	_ = e.StopContainer(ctx, id, time.Second)
	if _, ok := e.Peer(id); ok {
		t.Fatal("Peer should not exist")
	}
}

func TestFakeReattachAfterStop(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a", Stdin: true})
	if _, err := e.Attach(ctx, id); err != nil {
		t.Fatal(err)
	}
	oldPeer, _ := e.Peer(id)
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Peer(id); ok {
		t.Fatal("Peer should be gone after stop")
	}
	buf := make([]byte, 4)
	if _, err := oldPeer.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("old peer err = %v, want EOF", err)
	}
	conn, err := e.Attach(ctx, id)
	if err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	peer, ok := e.Peer(id)
	if !ok {
		t.Fatal("Peer missing after re-Attach")
	}
	go func() { _, _ = peer.Write([]byte("ping")) }()
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read = %q, %v", buf, err)
	}
	go func() { _, _ = conn.Write([]byte("pong")) }()
	if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("read = %q, %v", buf, err)
	}

	// Exit also clears the pipe for the next Attach.
	e.Exit(id, 1)
	if _, ok := e.Peer(id); ok {
		t.Fatal("Peer should be gone after Exit")
	}
	if _, err := e.Attach(ctx, id); err != nil {
		t.Fatalf("Attach after Exit: %v", err)
	}
}

func TestFakePullProgressDoesNotHoldLock(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	pr, pw := io.Pipe()
	got := make(chan []string, 1)
	go func() {
		// PullImage is blocked in pw.Write until we read; calling Calls()
		// before draining would deadlock if the lock were still held.
		time.Sleep(50 * time.Millisecond)
		got <- e.Calls()
		_, _ = io.Copy(io.Discard, pr)
	}()
	done := make(chan error, 1)
	go func() { done <- e.PullImage(ctx, "r/i:1", nil, pw); _ = pw.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PullImage deadlocked")
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("Calls deadlocked")
	}
}

func TestFakeWaitReturnsCodeAtExit(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	_ = e.StartContainer(ctx, id)
	ch := make(chan int, 1)
	go func() { c, _ := e.Wait(ctx, id); ch <- c }()
	time.Sleep(20 * time.Millisecond)
	e.Exit(id, 3)
	e.Exit(id, 9) // already exited: no-op
	if c := <-ch; c != 3 {
		t.Fatalf("waiter code = %d, want 3", c)
	}
	info, _ := e.InspectContainer(ctx, id)
	if info.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", info.ExitCode)
	}
	// Restart then exit with another code; a Wait on the new run sees it.
	_ = e.StartContainer(ctx, id)
	go func() { c, _ := e.Wait(ctx, id); ch <- c }()
	time.Sleep(20 * time.Millisecond)
	e.Exit(id, 5)
	if c := <-ch; c != 5 {
		t.Fatalf("second run code = %d, want 5", c)
	}
}

func TestFakeExitClosesPipe(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	_, _ = e.Attach(ctx, id)
	peer, _ := e.Peer(id)
	_ = e.StartContainer(ctx, id)
	e.Exit(id, 7)
	buf := make([]byte, 1)
	if _, err := peer.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	info, _ := e.InspectContainer(ctx, id)
	if info.State != "exited" || info.ExitCode != 7 {
		t.Fatalf("info = %+v", info)
	}
}

func TestFakeWaitUnblocksOnExit(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	_ = e.StartContainer(ctx, id)
	type res struct {
		code int
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := e.Wait(ctx, id)
		ch <- res{c, err}
	}()
	select {
	case <-ch:
		t.Fatal("Wait returned early")
	case <-time.After(50 * time.Millisecond):
	}
	e.Exit(id, 3)
	select {
	case r := <-ch:
		if r.err != nil || r.code != 3 {
			t.Fatalf("Wait = %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock")
	}
	// Wait on already-exited container returns immediately.
	if c, err := e.Wait(ctx, id); err != nil || c != 3 {
		t.Fatalf("Wait again = %d, %v", c, err)
	}
}

func TestFakeWaitUnblocksOnStop(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	_ = e.StartContainer(ctx, id)
	ch := make(chan int, 1)
	go func() {
		c, _ := e.Wait(ctx, id)
		ch <- c
	}()
	time.Sleep(20 * time.Millisecond)
	// StopContainer must not deadlock against a blocked Wait.
	if err := e.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-ch:
		if c != 0 {
			t.Fatalf("code = %d", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock")
	}
}

func TestFakeWaitContextCancel(t *testing.T) {
	e := fake.New()
	id, _ := e.CreateContainer(context.Background(), runtime.ContainerSpec{Name: "a"})
	_ = e.StartContainer(context.Background(), id)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { _, err := e.Wait(ctx, id); ch <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-ch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return on cancel")
	}
	if _, err := e.Wait(context.Background(), "nope"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFakeBuildRegistersTag(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	if ok, _ := e.ImageExists(ctx, "t"); ok {
		t.Fatal("image should not exist yet")
	}
	id, err := e.BuildImage(ctx, runtime.BuildRequest{Tag: "t", ContextDir: "/c"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "sha256:fake-") {
		t.Fatalf("id = %q", id)
	}
	if ok, err := e.ImageExists(ctx, "t"); err != nil || !ok {
		t.Fatalf("ImageExists = %v, %v", ok, err)
	}
	if b := e.Builds(); len(b) != 1 || b[0].Tag != "t" {
		t.Fatalf("Builds = %+v", b)
	}
}

func TestFakePullAndMarkImage(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	e.MarkImage("seeded")
	if ok, _ := e.ImageExists(ctx, "seeded"); !ok {
		t.Fatal("seeded image should exist")
	}
	var sb strings.Builder
	if err := e.PullImage(ctx, "r/i:1", nil, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.Len() == 0 {
		t.Fatal("expected progress output")
	}
	if err := e.PullImage(ctx, "r/j:1", &runtime.RegistryAuth{Username: "u"}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.ImageExists(ctx, "r/i:1"); !ok {
		t.Fatal("pulled image should exist")
	}
	if p := e.Pulls(); len(p) != 2 || p[0] != "r/i:1" || p[1] != "r/j:1" {
		t.Fatalf("Pulls = %v", p)
	}
}

func TestFakeNetworks(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	if err := e.EnsureNetwork(ctx, "net"); err != nil {
		t.Fatal(err)
	}
	if err := e.ConnectNetwork(ctx, "c1", "net"); err != nil {
		t.Fatal(err)
	}
	if n := e.Networks(); len(n) != 1 || n[0] != "net" {
		t.Fatalf("Networks = %v", n)
	}
	if c := e.Connections(); len(c) != 1 || c[0] != [2]string{"c1", "net"} {
		t.Fatalf("Connections = %v", c)
	}
	if err := e.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFakeLogs(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	lines := []runtime.LogLine{{Stream: "stdout", Text: "1"}, {Stream: "stderr", Text: "2"}, {Stream: "stdout", Text: "3"}}
	e.SetLogs(id, lines)
	got, err := e.Logs(ctx, id, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("all = %+v, %v", got, err)
	}
	got, _ = e.Logs(ctx, id, 2)
	if len(got) != 2 || got[0].Text != "2" || got[1].Text != "3" {
		t.Fatalf("tail 2 = %+v", got)
	}
	got, _ = e.Logs(ctx, id, 10)
	if len(got) != 3 {
		t.Fatalf("tail 10 = %+v", got)
	}
	if _, err := e.Logs(ctx, "nope", 0); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeSeed(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	e.Seed(runtime.ContainerInfo{ID: "pre-1", Name: "old", State: "running", Labels: map[string]string{"mcp-machine.managed": "true"}})
	got, _ := e.ListContainers(ctx, map[string]string{"mcp-machine.managed": "true"})
	if len(got) != 1 || got[0].ID != "pre-1" || got[0].State != "running" {
		t.Fatalf("got %+v", got)
	}
	if err := e.StopContainer(ctx, "old", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveContainer(ctx, "pre-1", false); err != nil {
		t.Fatal(err)
	}
}

func TestFakeFailNext(t *testing.T) {
	ctx := context.Background()
	errBoom := errors.New("boom")
	errBang := errors.New("bang")
	e := fake.New()
	id, _ := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "a"})
	e.FailNext("StartContainer", errBoom)
	e.FailNext("StartContainer", errBang)
	if err := e.StartContainer(ctx, id); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if err := e.StartContainer(ctx, id); !errors.Is(err, errBang) {
		t.Fatalf("err = %v, want bang", err)
	}
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	e.FailNext("Ping", errBoom)
	if err := e.Ping(ctx); !errors.Is(err, errBoom) {
		t.Fatalf("ping err = %v", err)
	}
	want := []string{"CreateContainer", "StartContainer", "StartContainer", "StartContainer", "Ping"}
	got := e.Calls()
	if len(got) != len(want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Calls = %v, want %v", got, want)
		}
	}
}

func TestFakeConnectNetworkShowsInInspectAndList(t *testing.T) {
	ctx := context.Background()
	e := fake.New()
	id, err := e.CreateContainer(ctx, runtime.ContainerSpec{Name: "n", Image: "img", Network: "b-net"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ConnectNetwork(ctx, id, "a-net"); err != nil {
		t.Fatal(err)
	}
	if err := e.ConnectNetwork(ctx, id, "a-net"); err != nil { // idempotent
		t.Fatal(err)
	}
	info, err := e.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a-net", "b-net"}; !slices.Equal(info.Networks, want) {
		t.Fatalf("Networks = %v, want %v", info.Networks, want)
	}
	info.Networks[0] = "mutated" // returned slice must be a copy
	again, _ := e.InspectContainer(ctx, id)
	if again.Networks[0] != "a-net" {
		t.Fatalf("Networks aliased: %v", again.Networks)
	}
	list, err := e.ListContainers(ctx, nil)
	if err != nil || len(list) != 1 || !slices.Contains(list[0].Networks, "a-net") {
		t.Fatalf("List = %+v, err %v", list, err)
	}
}

func TestFakeKeepsSpecMounts(t *testing.T) {
	e := fake.New()
	mounts := []runtime.Mount{
		{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true, Relabel: "z"},
		{Type: "volume", Source: "data", Target: "/data", Relabel: "Z"},
	}
	id, err := e.CreateContainer(context.Background(), runtime.ContainerSpec{Name: "a", Image: "img", Mounts: mounts})
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := e.Spec(id)
	if !ok || !slices.Equal(spec.Mounts, mounts) {
		t.Fatalf("Spec(%s) = %+v, %v; want mounts %+v", id, spec.Mounts, ok, mounts)
	}
	if _, ok := e.Spec("nope"); ok {
		t.Error("Spec of an unknown container")
	}
}
