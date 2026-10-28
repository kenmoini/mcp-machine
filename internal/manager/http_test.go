package manager_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// httpResolved is server a as a container serving Streamable HTTP on 3000.
func httpResolved() sources.Resolved {
	res := stdioResolved()
	res.Container.Stdin = false
	res.Container.Ports = []runtime.PortMapping{{ContainerPort: 3000, Protocol: "tcp"}}
	res.Transport = "http"
	res.HTTP = &sources.HTTPEndpoint{Port: 3000, Path: "/mcp", Transport: "http"}
	return res
}

// pingServer is an MCP server with one tool, "ping".
func pingServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
		})
	return srv
}

// streamableHandler serves srv over Streamable HTTP.
func streamableHandler(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

// handlerTransport is an http.RoundTripper that serves every request with an
// in-process handler, streaming the response body through a pipe so
// long-lived SSE responses work. It lets the remote path be tested without a
// listener.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sreq := req.Clone(req.Context())
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	sreq.RequestURI = req.URL.RequestURI()
	if sreq.Host == "" {
		sreq.Host = req.URL.Host
	}
	pr, pw := io.Pipe()
	w := &pipeResponse{header: http.Header{}, body: pw, ready: make(chan struct{})}
	go func() {
		defer func() { _ = sreq.Body.Close() }()
		t.h.ServeHTTP(w, sreq)
		w.WriteHeader(http.StatusOK) // no-op unless the handler wrote nothing
		_ = pw.Close()
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		_ = pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{
		Status:        http.StatusText(w.code),
		StatusCode:    w.code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}, nil
}

type pipeResponse struct {
	header, sent http.Header
	body         *io.PipeWriter
	code         int
	once         sync.Once
	ready        chan struct{}
}

func (w *pipeResponse) Header() http.Header { return w.header }

func (w *pipeResponse) WriteHeader(code int) {
	w.once.Do(func() {
		w.code = code
		w.sent = w.header.Clone()
		close(w.ready)
	})
}

func (w *pipeResponse) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

func (w *pipeResponse) Flush() {}

// useInProcessHTTP routes http.DefaultTransport (which proxy.DialHTTP uses)
// to h for the rest of the test. Call it before newHarness so the transport
// is restored only after the manager is closed.
func useInProcessHTTP(t *testing.T, h http.Handler) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = handlerTransport{h: h}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// --- tests -----------------------------------------------------------------

// Needs a listener (httptest.NewServer).
func TestHttpModeUsesEndpointFuncAndProbe(t *testing.T) {
	srv := httptest.NewServer(streamableHandler(pingServer()))
	t.Cleanup(srv.Close)

	var (
		mu      sync.Mutex
		gotInfo []runtime.ContainerInfo
		gotEP   []sources.HTTPEndpoint
	)
	h := newHarness(t, manager.Options{
		EndpointFunc: func(info runtime.ContainerInfo, ep sources.HTTPEndpoint) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			gotInfo = append(gotInfo, info)
			gotEP = append(gotEP, ep)
			return srv.URL + ep.Path, nil
		},
	})
	h.res.res = httpResolved()
	h.apply()
	st := waitState(t, h.reg, "a", store.StateReady)

	calls := h.eng.Calls()
	if slices.Contains(calls, "Attach") {
		t.Errorf("http mode attached: %v", calls)
	}
	if !inOrder(calls, []string{"CreateContainer", "StartContainer", "InspectContainer"}) {
		t.Errorf("calls = %v, want CreateContainer, StartContainer, InspectContainer", calls)
	}
	mu.Lock()
	if len(gotInfo) != 1 || gotInfo[0].ID != st.ContainerID {
		t.Errorf("EndpointFunc infos = %+v, want one for %s", gotInfo, st.ContainerID)
	}
	if len(gotEP) != 1 || gotEP[0] != (sources.HTTPEndpoint{Port: 3000, Path: "/mcp", Transport: "http"}) {
		t.Errorf("EndpointFunc endpoints = %+v", gotEP)
	}
	mu.Unlock()

	f, _ := h.m.Facade("a")
	if got := listTools(t, f.Server()); !slices.Equal(got, []string{"ping"}) {
		t.Errorf("facade tools = %v, want [ping]", got)
	}
	if _, ok := h.m.Lookup("a"); !ok {
		t.Error("Lookup(a) = false while Ready")
	}
}

// Needs a listener (to find a closed port).
func TestHttpModeProbeTimeoutFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + ln.Addr().String() + "/mcp"
	_ = ln.Close()

	h := newHarness(t, manager.Options{
		ProbeTimeout: 300 * time.Millisecond,
		EndpointFunc: func(runtime.ContainerInfo, sources.HTTPEndpoint) (string, error) { return url, nil },
	})
	h.res.res = httpResolved()
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	if !strings.Contains(st.Error, "probe") {
		t.Errorf("error = %q, want a probe failure", st.Error)
	}
	if want := "probe " + url + ": timed out after 300ms"; !strings.Contains(st.Error, want) {
		t.Errorf("error = %q, want it to contain %q", st.Error, want)
	}
}

func TestHttpContainerSpec(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inContainer bool
		wantPorts   []runtime.PortMapping
	}{
		{"host", false, []runtime.PortMapping{{HostIP: "127.0.0.1", HostPort: 0, ContainerPort: 3000, Protocol: "tcp"}}},
		{"in-container", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				infos []runtime.ContainerInfo
			)
			h := newHarness(t, manager.Options{
				InContainer: tc.inContainer,
				EndpointFunc: func(info runtime.ContainerInfo, _ sources.HTTPEndpoint) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					infos = append(infos, info)
					return "", errors.New("test: no endpoint")
				},
			})
			h.res.res = httpResolved()
			h.apply()
			st := waitFor(t, h.reg, "a", "endpoint failure", func(s store.Status, ok bool) bool {
				return ok && s.State == store.StateFailed && strings.Contains(s.Error, "test: no endpoint")
			})

			info, err := h.eng.InspectContainer(context.Background(), st.ContainerID)
			if err != nil {
				t.Fatal(err)
			}
			if lbl, ok := store.ParseLabels(info.Labels); !ok || lbl.Transport != "http" || lbl.Hash != st.Hash {
				t.Errorf("labels = %v, want transport http", info.Labels)
			}
			if !slices.Equal(info.Ports, tc.wantPorts) {
				t.Errorf("ports = %+v, want %+v", info.Ports, tc.wantPorts)
			}
			if info.State != "running" {
				t.Errorf("container state = %q, want running", info.State)
			}
			if slices.Contains(h.eng.Calls(), "Attach") {
				t.Errorf("http mode attached: %v", h.eng.Calls())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(infos) != 1 || infos[0].ID != st.ContainerID {
				t.Errorf("EndpointFunc infos = %+v", infos)
			}
		})
	}
}

func TestHttpResumeStartsWithoutAttach(t *testing.T) {
	var calls int
	var mu sync.Mutex
	h := newHarness(t, manager.Options{
		EndpointFunc: func(runtime.ContainerInfo, sources.HTTPEndpoint) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return "", errors.New("test: no endpoint")
		},
	})
	h.res.res = httpResolved()
	h.apply()
	st := waitState(t, h.reg, "a", store.StateFailed)
	ctx := context.Background()

	if err := h.m.Stop(ctx, "a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if info, _ := h.eng.InspectContainer(ctx, st.ContainerID); info.State != "exited" {
		t.Errorf("container after Stop = %q, want exited", info.State)
	}
	before := len(h.eng.Calls())
	if err := h.m.Start(ctx, "a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := waitFor(t, h.reg, "a", "second endpoint failure", func(s store.Status, ok bool) bool {
		mu.Lock()
		defer mu.Unlock()
		return ok && s.State == store.StateFailed && calls == 2
	})
	if got.ContainerID != st.ContainerID {
		t.Errorf("resume replaced the container: %s -> %s", st.ContainerID, got.ContainerID)
	}
	resumed := h.eng.Calls()[before:]
	if !inOrder(resumed, []string{"StartContainer", "InspectContainer"}) {
		t.Errorf("calls on resume = %v, want StartContainer, InspectContainer", resumed)
	}
	for _, m := range []string{"Attach", "CreateContainer"} {
		if slices.Contains(resumed, m) {
			t.Errorf("resume called %s: %v", m, resumed)
		}
	}
}

func TestRemoteServerReady(t *testing.T) {
	var (
		mu    sync.Mutex
		auths []string
	)
	backend := streamableHandler(pingServer())
	useInProcessHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		backend.ServeHTTP(w, r)
	}))

	h := newHarness(t, manager.Options{})
	h.cfg.Servers[0] = config.ServerSpec{
		Name:   "a",
		Type:   "remote",
		Remote: &config.RemoteSpec{URL: "http://remote.example/mcp", Transport: "http"},
	}
	h.res.res = sources.Resolved{
		Kind: sources.KindRemote,
		Remote: &sources.RemoteEndpoint{
			URL:       "http://remote.example/mcp",
			Transport: "http",
			Headers:   map[string]string{"Authorization": "Bearer t0k"},
		},
	}
	h.apply()
	st := waitState(t, h.reg, "a", store.StateReady)

	// Only the reconcile's one-time adoption scan touches the engine.
	if calls := h.eng.Calls(); !slices.Equal(calls, []string{"ListContainers"}) {
		t.Errorf("engine calls = %v, want only the adoption scan [ListContainers]", calls)
	}
	if st.ContainerID != "" {
		t.Errorf("remote server has a container: %q", st.ContainerID)
	}
	f, _ := h.m.Facade("a")
	if got := listTools(t, f.Server()); !slices.Equal(got, []string{"ping"}) {
		t.Errorf("facade tools = %v, want [ping]", got)
	}
	mu.Lock()
	if len(auths) == 0 || auths[0] != "Bearer t0k" {
		t.Errorf("Authorization headers seen = %q, want Bearer t0k", auths)
	}
	mu.Unlock()
}

func TestRemoteServerStopStartLogs(t *testing.T) {
	useInProcessHTTP(t, streamableHandler(pingServer()))
	h := newHarness(t, manager.Options{})
	h.cfg.Servers[0] = config.ServerSpec{
		Name:   "a",
		Type:   "remote",
		Remote: &config.RemoteSpec{URL: "http://remote.example/mcp"},
	}
	h.res.res = sources.Resolved{
		Kind:   sources.KindRemote,
		Remote: &sources.RemoteEndpoint{URL: "http://remote.example/mcp", Transport: "auto"},
	}
	h.apply()
	waitState(t, h.reg, "a", store.StateReady)
	ctx := context.Background()

	if err := h.m.Stop(ctx, "a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, _ := h.reg.Get("a"); got.State != store.StateStopped || !got.Paused {
		t.Errorf("after Stop = %+v, want Stopped and Paused", got)
	}
	if _, err := h.m.Logs(ctx, "a", 10); err == nil || !strings.Contains(err.Error(), "remote server has no logs") {
		t.Errorf("Logs err = %v, want remote server has no logs", err)
	}
	if err := h.m.Rebuild(ctx, "a"); err == nil {
		t.Error("Rebuild of a remote server succeeded")
	}
	if err := h.m.Start(ctx, "a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, h.reg, "a", store.StateReady)
	if err := h.m.Remove(ctx, "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if calls := h.eng.Calls(); !slices.Equal(calls, []string{"ListContainers"}) {
		t.Errorf("engine calls = %v, want only the adoption scan", calls)
	}
}

func TestDefaultEndpointInContainer(t *testing.T) {
	ep := manager.DefaultEndpoint(manager.Options{InContainer: true})
	got, err := ep(runtime.ContainerInfo{ID: "fake-1", Name: "mcpm-a"}, sources.HTTPEndpoint{Port: 3000, Path: "/mcp", Transport: "http"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://mcpm-a:3000/mcp" {
		t.Errorf("endpoint = %q, want http://mcpm-a:3000/mcp", got)
	}
}

func TestDefaultEndpointPublished(t *testing.T) {
	ep := manager.DefaultEndpoint(manager.Options{})
	httpEP := sources.HTTPEndpoint{Port: 3000, Path: "/mcp", Transport: "http"}
	info := runtime.ContainerInfo{ID: "fake-1", Name: "mcpm-a", Ports: []runtime.PortMapping{
		{HostIP: "127.0.0.1", HostPort: 5555, ContainerPort: 8080, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 41234, ContainerPort: 3000, Protocol: "tcp"},
	}}
	got, err := ep(info, httpEP)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:41234/mcp" {
		t.Errorf("endpoint = %q, want http://127.0.0.1:41234/mcp", got)
	}

	info.Ports = info.Ports[:1]
	if _, err := ep(info, httpEP); err == nil || !strings.Contains(err.Error(), "no published port for 3000") {
		t.Errorf("missing mapping err = %v, want no published port for 3000", err)
	}
}

func TestSelfJoinsNetworkOnce(t *testing.T) {
	h := newHarness(t, manager.Options{InContainer: true, SelfID: "self"})
	h.apply()
	h.apply()
	if n := h.count("ConnectNetwork"); n != 1 {
		t.Errorf("ConnectNetwork called %d times, want 1", n)
	}
	if got := h.eng.Connections(); !slices.Equal(got, [][2]string{{"self", "mcp-machine"}}) {
		t.Errorf("connections = %v, want [[self mcp-machine]]", got)
	}

	// A failed join is logged, not fatal.
	h2 := newHarness(t, manager.Options{InContainer: true, SelfID: "self"})
	h2.eng.FailNext("ConnectNetwork", errors.New("boom"))
	h2.apply()
	if n := h2.count("ConnectNetwork"); n != 1 {
		t.Errorf("after a failed join ConnectNetwork called %d times, want 1", n)
	}
}
