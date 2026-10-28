package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/web"
)

type fakeManager struct {
	mu      sync.Mutex
	calls   []string // "<action>:<name>"
	err     error
	facade  *proxy.Facade
	lines   []runtime.LogLine
	tails   []int
	logsErr error
	// onRemove, if set, runs on Remove (the real manager forgets the server).
	onRemove func(name string)
}

func (f *fakeManager) record(action, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action+":"+name)
	return f.err
}

func (f *fakeManager) Start(_ context.Context, n string) error   { return f.record("start", n) }
func (f *fakeManager) Stop(_ context.Context, n string) error    { return f.record("stop", n) }
func (f *fakeManager) Restart(_ context.Context, n string) error { return f.record("restart", n) }
func (f *fakeManager) Remove(_ context.Context, n string) error {
	err := f.record("remove", n)
	if f.onRemove != nil {
		f.onRemove(n)
	}
	return err
}
func (f *fakeManager) Rebuild(_ context.Context, n string) error { return f.record("rebuild", n) }
func (f *fakeManager) Resync(_ context.Context, n string) error  { return f.record("resync", n) }

func (f *fakeManager) Facade(name string) (*proxy.Facade, bool) {
	if f.facade != nil && name == "alpha" {
		return f.facade, true
	}
	return nil, false
}

func (f *fakeManager) Logs(_ context.Context, _ string, tail int) ([]runtime.LogLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tails = append(f.tails, tail)
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	n := min(tail, len(f.lines))
	return f.lines[len(f.lines)-n:], nil
}

type fakeConfig struct {
	cfg      *config.Config
	raw      *config.Config // what Raw returns; cfg when nil
	rawErr   error
	lastErr  error
	reloaded bool
	// onReload, if set, runs on Reload (the real one applies the config,
	// which registers new servers).
	onReload func()
}

func (c *fakeConfig) Current() *config.Config { return c.cfg }
func (c *fakeConfig) LastError() error        { return c.lastErr }
func (c *fakeConfig) Reload() error {
	c.reloaded = true
	if c.onReload != nil {
		c.onReload()
	}
	return nil
}
func (c *fakeConfig) Raw() (*config.Config, error) {
	if c.rawErr != nil {
		return nil, c.rawErr
	}
	if c.raw != nil {
		return c.raw, nil
	}
	return c.cfg, nil
}

type env struct {
	h   http.Handler
	mgr *fakeManager
	cfg *fakeConfig
	reg *store.Registry
	wr  *fakeWriter
}

func newEnv(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	reg.Set(store.Status{Name: "alpha", State: store.StateReady, Image: "img:1", Since: time.Now().Add(-3 * time.Minute), Restarts: 2})
	reg.Set(store.Status{Name: "beta", State: store.StateFailed, Error: "boom <b>", Since: time.Now().Add(-time.Hour)})

	srv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
	type in struct{}
	mcp.AddTool(srv, &mcp.Tool{Name: "ping", Description: "ping the thing"},
		func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "facade", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	fac := proxy.New("alpha", "1")
	if err := fac.Bind(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	mgr := &fakeManager{facade: fac}
	cfg := &fakeConfig{cfg: &config.Config{
		Listen:    ":8080",
		PublicURL: "http://mcp.lan:8080",
		Podman:    config.PodmanConfig{Socket: "unix:///run/podman.sock", Network: "mcp", StopTimeout: 10 * time.Second},
		Registries: []config.Registry{
			{Registry: "ghcr.io", Username: "ken", Password: "s3cret"},
		},
		Runtimes: map[string]string{"npx": "node:22"},
		Servers: []config.ServerSpec{
			{Name: "alpha", Type: "command", Command: &config.CommandSpec{Command: "npx"}},
			{Name: "beta", Type: "remote", Remote: &config.RemoteSpec{URL: "https://x.example/mcp", Transport: "auto", Headers: map[string]string{"Authorization": "Bearer hdr-secret"}}},
		},
	}}
	wr := &fakeWriter{}
	h := web.New(web.Deps{
		Manager: mgr, Registry: reg, Config: cfg, Writer: wr,
		PublicURL: "http://mcp.lan:8080", Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Every variable is set except those named UNSET*.
		LookupEnv: func(name string) (string, bool) { return "x", !strings.HasPrefix(name, "UNSET") },
	})
	return &env{h: h, mgr: mgr, cfg: cfg, reg: reg, wr: wr}
}

func (e *env) do(method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	setHeaders(req, hdr)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// testHost is the Host of test requests: the newEnv public_url host, which
// the host guard accepts. A "Host" pair in hdr overrides it.
const testHost = "mcp.lan:8080"

func setHeaders(req *http.Request, hdr []string) {
	req.Host = testHost
	for i := 0; i+1 < len(hdr); i += 2 {
		if http.CanonicalHeaderKey(hdr[i]) == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
}

func mustContain(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q:\n%s", s, body)
		}
	}
}

func TestIndexListsServersWithStates(t *testing.T) {
	rec := newEnv(t).do("GET", "/")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "alpha", "beta", `class="badge ready"`, `class="badge failed"`, "3m ago", `id="row-alpha"`,
		"/static/htmx.min.js", "boom &lt;b&gt;")
	if strings.Contains(body, "boom <b>") {
		t.Error("error text not escaped")
	}
}

func TestStatusPartialIsFragment(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/servers/alpha/status", "HX-Request", "true")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("fragment contains layout: %s", rec.Body)
	}
	mustContain(t, rec.Body.String(), "ready")
	full := e.do("GET", "/servers/alpha/status").Body.String()
	mustContain(t, full, "<html", "ready")
}

func TestActionCallsManagerAndReturnsRow(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/servers/alpha/actions/stop")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := fmt.Sprint(e.mgr.calls); got != "[stop:alpha]" {
		t.Errorf("calls = %s", got)
	}
	mustContain(t, rec.Body.String(), `id="row-alpha"`)
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("row wrapped in layout")
	}
}

func TestActionRemoveReturnsEmptyRowAndRedirectsFromDetail(t *testing.T) {
	e := newEnv(t)
	e.mgr.onRemove = e.reg.Delete
	rec := e.do("POST", "/servers/beta/actions/remove", "HX-Current-URL", "http://x/servers/beta")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("status %d body %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q", got)
	}
	rec = e.do("POST", "/servers/alpha/actions/remove")
	if rec.Header().Get("HX-Redirect") != "" {
		t.Error("redirect set from the dashboard")
	}
}

func TestUnknownActionIs400(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/servers/alpha/actions/explode")
	if rec.Code != 400 {
		t.Errorf("status %d", rec.Code)
	}
	if len(e.mgr.calls) != 0 {
		t.Errorf("manager called: %v", e.mgr.calls)
	}
}

func TestUnknownServerIs404(t *testing.T) {
	e := newEnv(t)
	for _, tc := range [][2]string{
		{"POST", "/servers/nope/actions/stop"},
		{"GET", "/servers/nope"},
		{"GET", "/servers/nope/status"},
		{"GET", "/servers/nope/logs"},
		{"GET", "/servers/nope/tools"},
	} {
		if rec := e.do(tc[0], tc[1]); rec.Code != 404 {
			t.Errorf("%s %s: status %d", tc[0], tc[1], rec.Code)
		}
	}
	if len(e.mgr.calls) != 0 {
		t.Errorf("manager called: %v", e.mgr.calls)
	}
}

func TestActionManagerErrorIs500(t *testing.T) {
	e := newEnv(t)
	e.mgr.err = errors.New("kaboom <i>")
	rec := e.do("POST", "/servers/alpha/actions/restart")
	if rec.Code != 500 {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), `id="row-alpha"`, `class="err"`, "kaboom &lt;i&gt;")
}

func TestStaticServedWithCacheControl(t *testing.T) {
	rec := newEnv(t).do("GET", "/static/htmx.min.js")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if rec2 := newEnv(t).do("GET", "/static/app.css"); rec2.Code != 200 {
		t.Errorf("app.css status %d", rec2.Code)
	}
	if rec3 := newEnv(t).do("GET", "/static/missing.js"); rec3.Code != 404 {
		t.Errorf("missing status %d", rec3.Code)
	}
}

func TestAPIServersJSON(t *testing.T) {
	rec := newEnv(t).do("GET", "/api/servers")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var got []store.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].State != store.StateFailed {
		t.Errorf("got %+v", got)
	}
}

func TestTemplatesParse(_ *testing.T) {
	_ = web.New(web.Deps{Registry: store.NewRegistry()})
}

func TestServerPageShowsToolsAndSnippets(t *testing.T) {
	rec := newEnv(t).do("GET", "/servers/alpha")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "ping", "ping the thing",
		"claude mcp add --transport http alpha http://mcp.lan:8080/servers/alpha/mcp",
		"mcp-machine connect", "npx mcp-remote",
		`hx-trigger="every 2s"`, `hx-trigger="every 3s"`)
}

func TestServerPageWithoutFacade(t *testing.T) {
	rec := newEnv(t).do("GET", "/servers/beta")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "beta")
	if strings.Contains(body, "hdr-secret") {
		t.Error("header secret rendered")
	}
}

func TestLogsPartialTail(t *testing.T) {
	e := newEnv(t)
	for i := range 10 {
		e.mgr.lines = append(e.mgr.lines, runtime.LogLine{Stream: "stdout", Text: fmt.Sprintf("entry-%d", i)})
	}
	rec := e.do("GET", "/servers/alpha/logs?tail=5", "HX-Request", "true")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(e.mgr.tails) != 1 || e.mgr.tails[0] != 5 {
		t.Errorf("tails = %v", e.mgr.tails)
	}
	if n := strings.Count(rec.Body.String(), "entry-"); n != 5 {
		t.Errorf("lines = %d:\n%s", n, rec.Body)
	}
	mustContain(t, rec.Body.String(), "entry-9")
}

func TestLogsTailCapped(t *testing.T) {
	e := newEnv(t)
	e.do("GET", "/servers/alpha/logs")
	e.do("GET", "/servers/alpha/logs?tail=999999")
	e.do("GET", "/servers/alpha/logs?tail=junk")
	if got := fmt.Sprint(e.mgr.tails); got != "[200 2000 200]" {
		t.Errorf("tails = %s", got)
	}
}

func TestLogsErrorIsShown(t *testing.T) {
	e := newEnv(t)
	e.mgr.logsErr = errors.New("remote server has no logs")
	rec := e.do("GET", "/servers/alpha/logs")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "remote server has no logs")
}

func TestConfigPageShowsReloadError(t *testing.T) {
	e := newEnv(t)
	e.cfg.lastErr = errors.New(`server "a": duplicate`)
	rec := e.do("GET", "/config")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "server &#34;a&#34;: duplicate", "ghcr.io", "ken", "unix:///run/podman.sock", "node:22", "http://mcp.lan:8080")
}

func TestConfigPageNeverRendersPassword(t *testing.T) {
	body := newEnv(t).do("GET", "/config").Body.String()
	for _, secret := range []string{"s3cret", "hdr-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("config page leaks %q", secret)
		}
	}
}

func TestConfigReloadPost(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/config/reload")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/config" {
		t.Errorf("Location = %q", loc)
	}
	if !e.cfg.reloaded {
		t.Error("Reload not called")
	}
}

func TestAdapterPathsNotClaimed(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/servers/alpha/mcp", "/servers/alpha/sse"} {
		if rec := e.do("GET", p); rec.Code != 404 {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/servers/alpha/actions/stop", "Origin", "http://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d", rec.Code)
	}
	rec = e.do("POST", "/servers/alpha/actions/stop", "Host", "192.168.1.5:8080", "Origin", "http://192.168.1.5:8080") // same origin
	if rec.Code != 200 {
		t.Errorf("same-origin status %d", rec.Code)
	}
	rec = e.do("POST", "/servers/alpha/actions/stop", "Origin", "http://mcp.lan:8080") // public URL
	if rec.Code != 200 {
		t.Errorf("public-url origin status %d", rec.Code)
	}
	if got := fmt.Sprint(e.mgr.calls); got != "[stop:alpha stop:alpha]" {
		t.Errorf("calls = %s", got)
	}
}

func TestActionFromDetailPageRendersFlash(t *testing.T) {
	e := newEnv(t)
	e.mgr.err = errors.New("nope")
	rec := e.do("POST", "/servers/alpha/actions/stop", "HX-Target", "flash")
	if rec.Code != 500 {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "stop failed: nope")
}

func TestPollingGoneServerDoesNotSwapErrorBody(t *testing.T) {
	e := newEnv(t)
	hx := []string{"HX-Request", "true"}

	rec := e.do("GET", "/servers/nope/status?view=row", hx...)
	if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("HX-Reswap") != "outerHTML" {
		t.Errorf("row: status %d body %q reswap %q", rec.Code, rec.Body, rec.Header().Get("HX-Reswap"))
	}
	for _, p := range []string{"/servers/nope/status", "/servers/nope/tools", "/servers/nope/logs"} {
		rec := e.do("GET", p, hx...)
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("HX-Refresh") != "true" {
			t.Errorf("%s: status %d body %q refresh %q", p, rec.Code, rec.Body, rec.Header().Get("HX-Refresh"))
		}
	}
	if rec := e.do("GET", "/servers/nope/status?view=row"); rec.Code != 404 {
		t.Errorf("non-htmx row: status %d", rec.Code)
	}
}

func TestReloadWithoutConfigIs503(t *testing.T) {
	h := web.New(web.Deps{Registry: store.NewRegistry()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/config/reload", nil)
	req.Host = "127.0.0.1:8080"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d", rec.Code)
	}
}

// The web package renders what the proxy exposes; it must not depend on the
// MCP SDK directly.
func TestNoSDKImportInWeb(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if strings.Contains(imp.Path.Value, "go-sdk/mcp") {
				t.Errorf("%s imports %s", f, imp.Path.Value)
			}
		}
	}
}

func TestDashboardPollsAllRowsAtOnce(t *testing.T) {
	e := newEnv(t)
	body := e.do("GET", "/").Body.String()
	mustContain(t, body, `<tbody id="server-rows"`, `hx-get="/servers?view=rows"`)
	if strings.Contains(body, "/status?view=row") {
		t.Error("rows still poll one by one")
	}

	// A server registered after the page was rendered shows up in the next
	// poll of the table body.
	e.reg.Set(store.Status{Name: "gamma", State: store.StateDefined, Since: time.Now()})
	rec := e.do("GET", "/servers?view=rows", "HX-Request", "true")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	rows := rec.Body.String()
	mustContain(t, rows, `<tbody id="server-rows"`, `id="row-alpha"`, `id="row-beta"`, `id="row-gamma"`)
	if strings.Contains(rows, "<html") {
		t.Error("rows fragment wrapped in layout")
	}

	// Without servers the body says so.
	for _, n := range []string{"alpha", "beta", "gamma"} {
		e.reg.Delete(n)
	}
	mustContain(t, e.do("GET", "/servers?view=rows", "HX-Request", "true").Body.String(), "No servers configured.")

	// /servers itself is not a page.
	if rec := e.do("GET", "/servers"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("GET /servers: status %d loc %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestStateChangesRefuseUnknownHost(t *testing.T) {
	// A DNS-rebinding page posts with Origin == Host == its own name, which
	// the origin check accepts; the host guard does not.
	e := newEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/servers/alpha/actions/stop"},
		{"POST", "/config/reload"},
		{"POST", "/servers/new"},
		{"POST", "/servers/beta/delete"},
	} {
		rec := e.do(tc.method, tc.path, "Host", "rebind.evil.example:8080", "Origin", "http://rebind.evil.example:8080")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "host not allowed") {
			t.Errorf("%s %s: status %d body %q", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
	if len(e.mgr.calls) != 0 || len(e.wr.Deleted) != 0 || e.cfg.reloaded {
		t.Errorf("state changed: calls %v deleted %v reloaded %v", e.mgr.calls, e.wr.Deleted, e.cfg.reloaded)
	}

	// Fetch metadata alone marks a browser request too.
	rec := e.do("POST", "/servers/alpha/actions/stop", "Host", "rebind.evil.example:8080", "Sec-Fetch-Site", "same-origin")
	if rec.Code != http.StatusForbidden {
		t.Errorf("browser POST without Origin: status %d", rec.Code)
	}

	// Reads are not guarded; browser writes to IP literals, localhost and the
	// public_url host are accepted.
	if rec := e.do("GET", "/", "Host", "rebind.evil.example:8080", "Sec-Fetch-Site", "same-origin"); rec.Code != 200 {
		t.Errorf("GET with unknown host: status %d", rec.Code)
	}
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "mcp.lan:8080", "[::1]:8080"} {
		if rec := e.do("POST", "/servers/alpha/actions/stop", "Host", host, "Sec-Fetch-Site", "same-origin"); rec.Code != 200 {
			t.Errorf("browser POST with Host %s: status %d", host, rec.Code)
		}
	}

	// Without Origin or fetch metadata (curl, scripts) Host is not checked.
	if rec := e.do("POST", "/servers/alpha/actions/stop", "Host", "mcp-machine:8080"); rec.Code != 200 {
		t.Errorf("non-browser POST to a container name: status %d", rec.Code)
	}
}

func TestStateChangesAcceptAllowedOriginHost(t *testing.T) {
	h := web.New(web.Deps{Registry: store.NewRegistry(), AllowedOrigins: []string{"http://tools.lan:9000"}})
	req := httptest.NewRequest("POST", "/config/reload", nil)
	req.Host = "tools.lan:8080"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable { // past the guard: no config
		t.Errorf("status %d", rec.Code)
	}
}

func TestConfigPageShowsRawRegistryUsername(t *testing.T) {
	e := newEnv(t)
	e.cfg.cfg.Registries = []config.Registry{{Registry: "ghcr.io", Username: "expanded-user", Password: "s3cret"}}
	e.cfg.raw = &config.Config{Registries: []config.Registry{{Registry: "ghcr.io", Username: "${GHCR_USER}", Password: "${GHCR_TOKEN}"}}}
	body := e.do("GET", "/config").Body.String()
	mustContain(t, body, "ghcr.io", "${GHCR_USER}")
	for _, leak := range []string{"expanded-user", "s3cret", "GHCR_TOKEN"} {
		if strings.Contains(body, leak) {
			t.Errorf("config page shows %q", leak)
		}
	}
}

func TestServerPageShowsRunLine(t *testing.T) {
	e := newEnv(t)
	e.reg.Set(store.Status{Name: "gh", State: store.StateReady})
	e.cfg.cfg.Servers = append(e.cfg.cfg.Servers, config.ServerSpec{Name: "gh", Type: "command", Command: &config.CommandSpec{
		Run: "docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server",
	}})
	body := e.do("GET", "/servers/gh").Body.String()
	mustContain(t, body, "<dt>Source</dt>", "docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server")
}
