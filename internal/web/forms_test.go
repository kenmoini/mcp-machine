package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/web"
)

type updatedCall struct {
	Name string
	Spec config.ServerSpec
}

// fakeWriter records the edits the forms make.
type fakeWriter struct {
	Added   []config.ServerSpec
	Updated []updatedCall
	Deleted []string
	err     error
}

func (w *fakeWriter) AddServer(s config.ServerSpec) error {
	if w.err != nil {
		return w.err
	}
	w.Added = append(w.Added, s)
	return nil
}

func (w *fakeWriter) UpdateServer(name string, s config.ServerSpec) error {
	if w.err != nil {
		return w.err
	}
	w.Updated = append(w.Updated, updatedCall{name, s})
	return nil
}

func (w *fakeWriter) DeleteServer(name string) error {
	if w.err != nil {
		return w.err
	}
	w.Deleted = append(w.Deleted, name)
	return nil
}

func (e *env) post(target string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestNewFormRendersPerType(t *testing.T) {
	e := newEnv(t)
	body := e.do("GET", "/servers/new?type=build").Body.String()
	mustContain(t, body, `name="build.repo"`, `name="build.auth.token"`, `name="name"`, `name="env"`, `name="container.image"`)
	for _, absent := range []string{`name="remote.url"`, `name="command.command"`} {
		if strings.Contains(body, absent) {
			t.Errorf("build form contains %s", absent)
		}
	}
	mustContain(t, e.do("GET", "/servers/new?type=remote").Body.String(), `name="remote.url"`, `name="remote.headers"`)
	mustContain(t, e.do("GET", "/servers/new?type=command").Body.String(), `name="command.command"`, `name="command.run"`)
	picker := e.do("GET", "/servers/new").Body.String()
	mustContain(t, picker, "/servers/new?type=remote", "/servers/new?type=command", "/servers/new?type=build")
	if rec := e.do("GET", "/servers/new?type=bogus"); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus type: status %d", rec.Code)
	}
}

func TestNewRouteNotShadowedByServerPage(t *testing.T) {
	e := newEnv(t)
	// Even a registered server called "new" or "import" must not capture the
	// literal routes.
	e.reg.Set(store.Status{Name: "new", State: store.StateReady})
	e.reg.Set(store.Status{Name: "import", State: store.StateReady})
	for path, want := range map[string]string{
		"/servers/new?type=remote": `name="remote.url"`,
		"/servers/import":          `name="text"`,
	} {
		rec := e.do("GET", path)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		body := rec.Body.String()
		mustContain(t, body, want)
		if strings.Contains(body, "Capabilities") {
			t.Errorf("%s served the server detail page", path)
		}
	}
}

func remoteForm() url.Values {
	return url.Values{
		"name":             {"x"},
		"type":             {"remote"},
		"enabled":          {"0", "1"},
		"remote.url":       {"https://x.example/mcp"},
		"remote.transport": {"http"},
		"remote.headers":   {"Authorization: Bearer ${TOK}"},
	}
}

func TestCreateRemoteServerWritesAndRedirects(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/servers/new", remoteForm())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/servers/x" {
		t.Errorf("Location = %q", loc)
	}
	if len(e.wr.Added) != 1 {
		t.Fatalf("Added = %+v", e.wr.Added)
	}
	got := e.wr.Added[0]
	if got.Name != "x" || got.Type != "remote" || got.Remote.URL != "https://x.example/mcp" ||
		got.Remote.Transport != "http" || got.Remote.Headers["Authorization"] != "Bearer ${TOK}" {
		t.Errorf("spec = %+v / %+v", got, got.Remote)
	}
	if got.Enabled != nil {
		t.Errorf("Enabled = %v, want nil (default)", *got.Enabled)
	}
}

func TestCreateInvalidRendersErrors(t *testing.T) {
	e := newEnv(t)
	f := remoteForm()
	f.Set("name", "alpha") // already configured
	rec := e.post("/servers/new", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "duplicate", `name="remote.url"`, `value="https://x.example/mcp"`)
	if len(e.wr.Added) != 0 {
		t.Errorf("writer called: %+v", e.wr.Added)
	}

	// Parse problems name the offending line.
	f = remoteForm()
	f.Set("remote.headers", "Good: yes\nbroken line")
	rec = e.post("/servers/new", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "line 2")

	// Required fields the config validation does not cover.
	f = remoteForm()
	f.Set("remote.url", "")
	if rec = e.post("/servers/new", f); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty url: status %d", rec.Code)
	}
	if len(e.wr.Added) != 0 {
		t.Errorf("writer called: %+v", e.wr.Added)
	}
}

func TestCreateWriterErrorIs500(t *testing.T) {
	e := newEnv(t)
	e.wr.err = errors.New("disk <full>")
	rec := e.post("/servers/new", remoteForm())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "disk &lt;full&gt;", `name="remote.url"`)
}

func TestEditPrefillsAndKeepsVars(t *testing.T) {
	e := newEnv(t)
	e.cfg.raw = &config.Config{
		Podman: e.cfg.cfg.Podman,
		Servers: []config.ServerSpec{
			{Name: "x", Type: "remote", Remote: &config.RemoteSpec{
				URL: "https://x.example/mcp", Transport: "auto",
				Headers: map[string]string{"Authorization": "Bearer ${TOK}"},
			}},
		},
	}
	rec := e.do("GET", "/servers/x/edit")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "Authorization: Bearer ${TOK}", `value="https://x.example/mcp"`, "/servers/x/delete")

	// Post back what the form shows, unchanged.
	form := url.Values{
		"name": {"x"}, "type": {"remote"}, "enabled": {"0", "1"},
		"remote.url": {"https://x.example/mcp"}, "remote.transport": {"auto"},
		"remote.headers": {"Authorization: Bearer ${TOK}"},
	}
	rec = e.post("/servers/x/edit", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/servers/x" {
		t.Fatalf("status %d loc %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if len(e.wr.Updated) != 1 || e.wr.Updated[0].Name != "x" {
		t.Fatalf("Updated = %+v", e.wr.Updated)
	}
	if h := e.wr.Updated[0].Spec.Remote.Headers["Authorization"]; h != "Bearer ${TOK}" {
		t.Errorf("header = %q", h)
	}
	if rec := e.do("GET", "/servers/nope/edit"); rec.Code != 404 {
		t.Errorf("unknown edit: status %d", rec.Code)
	}
}

func TestEditPrefillsFromRawNotExpanded(t *testing.T) {
	e := newEnv(t)
	e.cfg.raw = &config.Config{Servers: []config.ServerSpec{
		{Name: "x", Type: "remote", Remote: &config.RemoteSpec{
			URL: "https://x.example/mcp", Transport: "auto",
			Headers: map[string]string{"Authorization": "Bearer ${TOK}"},
		}},
	}}
	e.cfg.cfg = &config.Config{Servers: []config.ServerSpec{
		{Name: "x", Type: "remote", Remote: &config.RemoteSpec{
			URL: "https://x.example/mcp", Transport: "auto",
			Headers: map[string]string{"Authorization": "Bearer s3cret"},
		}},
	}}
	body := e.do("GET", "/servers/x/edit").Body.String()
	mustContain(t, body, "${TOK}")
	if strings.Contains(body, "s3cret") {
		t.Error("edit page shows the expanded secret")
	}
}

func TestDeleteCallsWriter(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/servers/beta/delete", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d loc %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(e.wr.Deleted) != 1 || e.wr.Deleted[0] != "beta" {
		t.Errorf("Deleted = %v", e.wr.Deleted)
	}
	if rec := e.post("/servers/nope/delete", nil); rec.Code != 404 {
		t.Errorf("unknown: status %d", rec.Code)
	}
	e.wr.err = errors.New("read-only")
	rec = e.post("/servers/beta/delete", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("writer error: status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "read-only")
}

func TestFormsUnavailableWithoutWriter(t *testing.T) {
	h := web.New(web.Deps{Registry: store.NewRegistry()})
	for _, tc := range [][2]string{{"GET", "/servers/new"}, {"POST", "/servers/new"}, {"GET", "/servers/import"}, {"POST", "/servers/x/delete"}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc[0], tc[1], nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status %d", tc[0], tc[1], rec.Code)
		}
	}
}

func TestFormCrossOriginRefused(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/servers/new", remoteForm(), "Origin", "http://evil.example")
	if rec.Code != http.StatusForbidden || len(e.wr.Added) != 0 {
		t.Errorf("status %d added %v", rec.Code, e.wr.Added)
	}
}

func TestSpecFormRoundTrip(t *testing.T) {
	forms := []url.Values{
		{
			"name": {"r"}, "type": {"remote"}, "enabled": {"1"},
			"remote.url": {"https://x.example/mcp"}, "remote.transport": {"sse"},
			"remote.headers":  {"Authorization: Bearer ${TOK}\nX-A: b"},
			"env":             {"A=1\nB=${B}"},
			"container.image": {"img:1"}, "container.volumes": {"/a:/b\n/c:/d"},
		},
		{
			"name": {"c"}, "type": {"command"}, "enabled": {"0"},
			"command.command": {"npx"}, "command.args": {"-y\n@x/y"}, "command.env": {"K=V\nT=${T}"},
			"serve.mode": {"http"}, "serve.port": {"3000"}, "serve.path": {"/mcp"}, "serve.transport": {"http"},
			"container.env": {"Z=1"},
		},
		{
			"name": {"c2"}, "type": {"command"}, "enabled": {"1"},
			"command.run": {"docker run -i --rm img"},
		},
		{
			"name": {"b"}, "type": {"build"}, "enabled": {"1"},
			"build.repo": {"https://git.example/r.git"}, "build.ref": {"main"}, "build.subdir": {"sub"},
			"build.containerfile": {"Containerfile"}, "build.args": {"A=1\nB=${B}"},
			"build.auth.token": {"${GIT_TOKEN}"}, "build.auth.ssh_key_file": {"/keys/id"},
			"serve.mode": {"stdio"},
		},
	}
	for _, v := range forms {
		spec, err := web.SpecFromForm(v)
		if err != nil {
			t.Fatalf("%s: %v", v.Get("name"), err)
		}
		if got := web.FormFromSpec(spec); !reflect.DeepEqual(got, v) {
			t.Errorf("%s round trip:\n got %v\nwant %v", v.Get("name"), got, v)
		}
	}
}

func TestSpecFromFormParsing(t *testing.T) {
	spec, err := web.SpecFromForm(url.Values{
		"name": {"  s  "}, "type": {"command"},
		"command.command": {" npx "},
		"command.args":    {"-y\r\n\r\n  pkg  \n"},
		"command.env":     {"A = b=c\n"},
		"enabled":         {"0"},
		"serve.port":      {"8080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "s" || spec.Command.Command != "npx" || !reflect.DeepEqual(spec.Command.Args, []string{"-y", "pkg"}) {
		t.Errorf("spec = %+v / %+v", spec, spec.Command)
	}
	if spec.Command.Env["A"] != "b=c" {
		t.Errorf("env = %v", spec.Command.Env)
	}
	if spec.Enabled == nil || *spec.Enabled {
		t.Errorf("Enabled = %v", spec.Enabled)
	}
	if spec.Serve == nil || spec.Serve.Port != 8080 {
		t.Errorf("Serve = %+v", spec.Serve)
	}
	if spec.Container != nil || spec.Build != nil || spec.Remote != nil || spec.Env != nil {
		t.Errorf("empty sections created: %+v", spec)
	}

	for _, bad := range []struct{ key, val, want string }{
		{"env", "ok=1\nnoequals", "env: line 2"},
		{"command.env", "=v", "command.env: line 1"},
		{"remote.headers", "nocolon", "remote.headers: line 1"},
		{"serve.port", "abc", "serve.port"},
	} {
		v := url.Values{"name": {"s"}, "type": {"remote"}}
		v.Set(bad.key, bad.val)
		if _, err := web.SpecFromForm(v); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s=%q: err = %v, want %q", bad.key, bad.val, err, bad.want)
		}
	}
}

const twoServersJSON = `{"mcpServers": {
  "fs": {"command": "npx", "args": ["-y", "@mcp/fs"], "env": {"ROOT": "/data"}},
  "web": {"url": "https://web.example/mcp", "headers": {"Authorization": "Bearer ${TOK}"}}
}}`

// hiddenFields extracts name/value pairs of hidden inputs from a body.
func hiddenFields(t *testing.T, body string) url.Values {
	t.Helper()
	out := url.Values{}
	for _, part := range strings.Split(body, `<input type="hidden"`)[1:] {
		end := strings.Index(part, ">")
		tag := part[:end]
		get := func(attr string) string {
			i := strings.Index(tag, attr+`="`)
			if i < 0 {
				return ""
			}
			rest := tag[i+len(attr)+2:]
			return strings.NewReplacer("&#34;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&", "&#39;", "'", "&#43;", "+").
				Replace(rest[:strings.Index(rest, `"`)])
		}
		out.Add(get("name"), get("value"))
	}
	return out
}

func TestImportPreviewAndConfirm(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/servers/import", url.Values{"text": {twoServersJSON}})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	mustContain(t, body, "fs", "web", "command", "remote", "https://web.example/mcp", `name="confirm"`)
	if len(e.wr.Added) != 0 {
		t.Fatalf("preview wrote: %+v", e.wr.Added)
	}

	form := hiddenFields(t, body)
	form.Set("confirm", "1")
	rec = e.post("/servers/import", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("confirm: status %d loc %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if len(e.wr.Added) != 2 {
		t.Fatalf("Added = %+v", e.wr.Added)
	}
	byName := map[string]config.ServerSpec{}
	for _, s := range e.wr.Added {
		byName[s.Name] = s
	}
	if byName["fs"].Command == nil || byName["fs"].Command.Command != "npx" ||
		!reflect.DeepEqual(byName["fs"].Command.Args, []string{"-y", "@mcp/fs"}) || byName["fs"].Command.Env["ROOT"] != "/data" {
		t.Errorf("fs = %+v / %+v", byName["fs"], byName["fs"].Command)
	}
	if w := byName["web"]; w.Remote == nil || w.Remote.Headers["Authorization"] != "Bearer ${TOK}" {
		t.Errorf("web = %+v", w)
	}

	// A docker run line becomes one command entry carrying the line in Run.
	const line = "docker run -i --rm ghcr.io/acme/My_Server:1.2 --flag"
	rec = e.post("/servers/import", url.Values{"text": {line}})
	if rec.Code != 200 {
		t.Fatalf("docker preview: status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "my-server")
	form = hiddenFields(t, rec.Body.String())
	form.Set("confirm", "1")
	e.wr.Added = nil
	if rec = e.post("/servers/import", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("docker confirm: status %d: %s", rec.Code, rec.Body)
	}
	if len(e.wr.Added) != 1 || e.wr.Added[0].Name != "my-server" || e.wr.Added[0].Type != "command" ||
		e.wr.Added[0].Command.Run != line {
		t.Errorf("Added = %+v", e.wr.Added)
	}
}

func TestImportErrors(t *testing.T) {
	e := newEnv(t)
	for _, text := range []string{"", "{not json", "ls -la", "docker run -it img"} {
		rec := e.post("/servers/import", url.Values{"text": {text}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%q: status %d", text, rec.Code)
		}
		mustContain(t, rec.Body.String(), `name="text"`, `class="err`)
	}
	// An entry that collides with an existing server is reported, not written.
	rec := e.post("/servers/import", url.Values{"text": {`{"mcpServers":{"alpha":{"command":"npx"}}}`}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dup preview: status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "duplicate")
	if strings.Contains(rec.Body.String(), `name="confirm"`) {
		t.Error("confirm offered for an invalid import")
	}

	// A forged confirmation is validated too.
	rec = e.post("/servers/import", url.Values{"confirm": {"1"}, "entries[0].name": {"alpha"}, "entries[0].type": {"command"}, "entries[0].command.command": {"npx"}})
	if rec.Code != http.StatusUnprocessableEntity || len(e.wr.Added) != 0 {
		t.Errorf("forged confirm: status %d added %v", rec.Code, e.wr.Added)
	}
	if rec = e.post("/servers/import", url.Values{"confirm": {"1"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty confirm: status %d", rec.Code)
	}
}

func TestImportConfirmWriterErrorKeepsFailedEntries(t *testing.T) {
	e := newEnv(t)
	form := hiddenFields(t, e.post("/servers/import", url.Values{"text": {twoServersJSON}}).Body.String())
	form.Set("confirm", "1")
	e.wr.err = errors.New("disk full")
	rec := e.post("/servers/import", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "disk full", "fs", "web")
}
