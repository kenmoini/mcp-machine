package web_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/catalog"
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
	setHeaders(req, hdr)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestNewFormRendersPerType(t *testing.T) {
	e := newEnv(t)
	body := e.do("GET", "/servers/new?type=build").Body.String()
	mustContain(t, body, `name="build.repo"`, `name="build.auth.token"`, `name="build.auth.known_hosts_file"`, `name="name"`, `name="env"`, `name="container.image"`)
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
	e.cfg.origins["x"] = userOrigin("x")
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
		req := httptest.NewRequest(tc[0], tc[1], nil)
		req.Host = "127.0.0.1:8080"
		h.ServeHTTP(rec, req)
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
		{
			"name": {"b2"}, "type": {"build"}, "enabled": {"1"},
			"build.repo":              {"git@git.example:r.git"},
			"build.auth.ssh_key_file": {"/keys/id"}, "build.auth.known_hosts_file": {"/keys/known_hosts"},
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
		if end < 0 {
			t.Fatalf("unterminated hidden input in %q", part)
		}
		tag := part[:end]
		get := func(attr string) string {
			i := strings.Index(tag, attr+`="`)
			if i < 0 {
				return ""
			}
			rest := tag[i+len(attr)+2:]
			q := strings.Index(rest, `"`)
			if q < 0 {
				return ""
			}
			return strings.NewReplacer("&#34;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&", "&#39;", "'", "&#43;", "+").
				Replace(rest[:q])
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

func TestEditPreservesMultilineRun(t *testing.T) {
	const run = "docker run -i --rm \\\n  -e A=1 \\\n  ghcr.io/acme/server:1"
	e := newEnv(t)
	cur := config.ServerSpec{Name: "x", Type: "command", Command: &config.CommandSpec{Run: run}}
	e.cfg.raw = &config.Config{Podman: e.cfg.cfg.Podman, Servers: []config.ServerSpec{cur}}

	rec := e.do("GET", "/servers/x/edit")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	mustContain(t, body, `<textarea id="f-command-run" name="command.run"`, ">"+run+"</textarea>")

	// A browser submits textarea newlines as CRLF; the stored value stays LF.
	form := web.FormFromSpec(cur)
	form.Set("command.run", strings.ReplaceAll(run, "\n", "\r\n"))
	rec = e.post("/servers/x/edit", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(e.wr.Updated) != 1 || e.wr.Updated[0].Spec.Command.Run != run {
		t.Errorf("Updated = %+v", e.wr.Updated)
	}
}

func TestWritesReloadBeforeRedirect(t *testing.T) {
	// The new server's page exists only once the config has been applied:
	// the form reloads before redirecting instead of waiting for the watcher.
	e := newEnv(t)
	e.cfg.onReload = func() { e.reg.Set(store.Status{Name: "x", State: store.StateDefined}) }
	rec := e.post("/servers/new", remoteForm())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !e.cfg.reloaded {
		t.Fatal("create did not reload the configuration")
	}
	if got := e.do("GET", rec.Header().Get("Location")); got.Code != 200 {
		t.Fatalf("redirect target: status %d", got.Code)
	}

	for _, tc := range []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"update", func() *httptest.ResponseRecorder {
			return e.post("/servers/beta/edit", url.Values{"remote.url": {"https://y.example/mcp"}, "enabled": {"1"}})
		}},
		{"delete", func() *httptest.ResponseRecorder { return e.post("/servers/beta/delete", nil) }},
		{"import", func() *httptest.ResponseRecorder {
			form := hiddenFields(t, e.post("/servers/import", url.Values{"text": {twoServersJSON}}).Body.String())
			form.Set("confirm", "1")
			e.cfg.reloaded = false
			return e.post("/servers/import", form)
		}},
	} {
		e.cfg.reloaded = false
		e.cfg.onReload = nil
		if rec := tc.do(); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: status %d: %s", tc.name, rec.Code, rec.Body)
		}
		if !e.cfg.reloaded {
			t.Errorf("%s did not reload the configuration", tc.name)
		}
	}
}

func TestFormRejectsUnsetVariables(t *testing.T) {
	// A spec referencing an unset ${VAR} would be written, fail to reload,
	// and stop the next start: the form refuses it instead.
	e := newEnv(t)
	f := remoteForm()
	f.Set("remote.headers", "Authorization: Bearer ${UNSET_TOKEN}")
	f.Set("env", "A=${UNSET_A}\nB=${TOK}")
	rec := e.post("/servers/new", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "environment variable UNSET_A is not set", "environment variable UNSET_TOKEN is not set")
	if strings.Contains(rec.Body.String(), "environment variable TOK") {
		t.Error("a set variable was reported")
	}
	if len(e.wr.Added) != 0 || e.cfg.reloaded {
		t.Errorf("written anyway: %+v", e.wr.Added)
	}

	// Editing: the same check applies to the changed spec.
	rec = e.post("/servers/alpha/edit", url.Values{"command.command": {"npx"}, "command.args": {"--key\n${UNSET_KEY}"}, "enabled": {"1"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("edit: status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "environment variable UNSET_KEY is not set")
	if len(e.wr.Updated) != 0 {
		t.Errorf("updated anyway: %+v", e.wr.Updated)
	}
}

func TestEditPrefillOmitsDefaults(t *testing.T) {
	// Raw() parses the file, which applies defaults; the edit form must not
	// turn them into explicit keys on the first save.
	raw, err := config.Parse([]byte(`
servers:
  - {name: r, type: remote, remote: {url: "https://r.example/mcp"}}
  - {name: rs, type: remote, remote: {url: "https://r.example/sse", transport: sse}}
  - {name: b, type: build, build: {repo: "https://git.example/b.git"}}
  - {name: bc, type: build, build: {repo: "https://git.example/b.git", containerfile: build/Dockerfile}, serve: {mode: http, port: 3000}}
  - {name: c, type: command, command: {command: npx, args: [x]}}
`), func(string) (string, bool) { return "x", true })
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]config.ServerSpec{}
	for _, s := range raw.Servers {
		byName[s.Name] = web.StripDefaults(s)
	}
	if r := byName["r"]; r.Remote == nil || r.Remote.Transport != "" || r.Remote.URL != "https://r.example/mcp" {
		t.Errorf("r = %+v", r.Remote)
	}
	if rs := byName["rs"]; rs.Remote.Transport != "sse" {
		t.Errorf("rs transport = %q", rs.Remote.Transport)
	}
	if b := byName["b"]; b.Build == nil || b.Build.Containerfile != "" || b.Serve != nil {
		t.Errorf("b = %+v serve %+v", b.Build, b.Serve)
	}
	if bc := byName["bc"]; bc.Build.Containerfile != "build/Dockerfile" || bc.Serve == nil || bc.Serve.Mode != "http" || bc.Serve.Port != 3000 {
		t.Errorf("bc = %+v serve %+v", bc.Build, bc.Serve)
	}
	if c := byName["c"]; c.Serve != nil || c.Command.Command != "npx" {
		t.Errorf("c = %+v serve %+v", c.Command, c.Serve)
	}
	if raw.Servers[0].Remote.Transport != "auto" || raw.Servers[2].Serve == nil {
		t.Error("StripDefaults modified its argument")
	}

	// End to end: an unchanged edit posts no default-valued fields.
	e := newEnv(t)
	e.cfg.raw = raw
	for name, absent := range map[string][]string{
		"r": {`<option value="auto" selected>`},
		"b": {`value="Containerfile"`, `<option value="stdio" selected>`},
		"c": {`<option value="stdio" selected>`},
	} {
		body := e.do("GET", "/servers/"+name+"/edit").Body.String()
		for _, a := range absent {
			if strings.Contains(body, a) {
				t.Errorf("edit %s prefills %s", name, a)
			}
		}
	}
}

func TestEditFormDeleteVariantUser(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/servers/alpha/edit")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	mustContain(t, body, `action="/servers/alpha/delete"`, `hx-confirm="Delete alpha?"`, ">Delete alpha</button>",
		"Removes "+userFile("alpha")+". Its container is stopped and removed.")
	if strings.Contains(body, "Revert to admin definition") {
		t.Error("revert offered for a UI-owned server")
	}
}

func TestEditFormDeleteVariantRevert(t *testing.T) {
	e := newEnv(t)
	e.cfg.origins["alpha"] = overriddenOrigin("alpha")
	body := e.do("GET", "/servers/alpha/edit").Body.String()
	mustContain(t, body, `action="/servers/alpha/delete"`, ">Revert to admin definition</button>",
		"Removes "+userFile("alpha")+"; alpha goes back to the administrator&#39;s definition.")
	if strings.Contains(body, ">Delete alpha</button>") {
		t.Error("delete offered for an admin-defined server")
	}
}

func TestEditFormDeleteVariantNone(t *testing.T) {
	e := newEnv(t)
	e.cfg.origins["alpha"] = adminOrigin()
	body := e.do("GET", "/servers/alpha/edit").Body.String()
	mustContain(t, body, `<p class="muted">Defined by the administrator in `+baseFile+", "+systemFile+
		"; it cannot be deleted here, only overridden by saving changes.</p>")
	if strings.Contains(body, "/servers/alpha/delete") || strings.Contains(body, "Revert to admin definition") {
		t.Error("delete form offered for an admin-only server")
	}

	// Origin unknown: nothing at all.
	delete(e.cfg.origins, "alpha")
	body = e.do("GET", "/servers/alpha/edit").Body.String()
	if strings.Contains(body, "/servers/alpha/delete") || strings.Contains(body, "danger-zone") {
		t.Error("delete section shown for an unknown origin")
	}
}

func TestDeleteActionFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     config.Origin
		found bool
		want  web.DeleteAction
	}{
		{"user", userOrigin("alpha"), true, web.DeleteAction{Mode: "delete",
			Note: "Removes /data/config.d/alpha.yaml. Its container is stopped and removed."}},
		{"revert", overriddenOrigin("alpha"), true, web.DeleteAction{Mode: "revert",
			Note: "Removes /data/config.d/alpha.yaml; alpha goes back to the administrator's definition."}},
		{"none", adminOrigin(), true, web.DeleteAction{Mode: "none",
			Note: "Defined by the administrator in " + baseFile + ", " + systemFile +
				"; it cannot be deleted here, only overridden by saving changes."}},
		{"unknown", config.Origin{}, false, web.DeleteAction{Mode: "none"}},
		{"foreign", foreignOrigin(false), true, web.DeleteAction{Mode: "none",
			Note: "Defined in /data/config.d/zzz.yaml, a file the UI does not manage; edit or remove it by hand."}},
		{"foreign over admin", foreignOrigin(true), true, web.DeleteAction{Mode: "none",
			Note: "Defined in /data/config.d/zzz.yaml, a file the UI does not manage; edit or remove it by hand."}},
		{"user with foreign", withCanonical(foreignOrigin(false)), true, web.DeleteAction{Mode: "delete",
			Note: "Removes /data/config.d/alpha.yaml. The definition in /data/config.d/zzz.yaml stays in effect."}},
		{"revert with foreign", withCanonical(foreignOrigin(true)), true, web.DeleteAction{Mode: "revert",
			Note: "Removes /data/config.d/alpha.yaml. The definition in /data/config.d/zzz.yaml stays in effect."}},
	} {
		if got := web.DeleteActionFor("alpha", tc.o, tc.found); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// foreignOrigin is alpha defined in the hand-placed user file zzz.yaml,
// over the admin definition when admin is true.
func foreignOrigin(admin bool) config.Origin {
	var o config.Origin
	if admin {
		o = adminOrigin()
	}
	o.Files = append(o.Files, config.OriginFile{Path: "/data/config.d/zzz.yaml", Layer: config.LayerUser})
	return o
}

// withCanonical adds alpha's own user file, which the loader applies last.
func withCanonical(o config.Origin) config.Origin {
	o.Files = append(o.Files, config.OriginFile{Path: userFile("alpha"), Layer: config.LayerUser})
	return o
}

func TestDeleteForeignOriginIs409(t *testing.T) {
	e := newEnv(t)
	e.cfg.origins["alpha"] = foreignOrigin(false)
	rec := e.post("/servers/alpha/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "a file the UI does not manage")
	if len(e.wr.Deleted) != 0 || e.cfg.reloads != 0 {
		t.Errorf("deleted %v, reloads %d", e.wr.Deleted, e.cfg.reloads)
	}
}

func TestDeleteAdminOwnedIs409(t *testing.T) {
	e := newEnv(t)
	e.cfg.origins["alpha"] = adminOrigin()
	rec := e.post("/servers/alpha/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "cannot be deleted here")
	if len(e.wr.Deleted) != 0 || e.cfg.reloads != 0 {
		t.Errorf("deleted %v, reloads %d", e.wr.Deleted, e.cfg.reloads)
	}

	// An unknown origin is refused the same way.
	delete(e.cfg.origins, "alpha")
	if rec := e.post("/servers/alpha/delete", nil); rec.Code != http.StatusConflict {
		t.Errorf("unknown origin: status %d", rec.Code)
	}
	if len(e.wr.Deleted) != 0 || e.cfg.reloads != 0 {
		t.Errorf("deleted %v, reloads %d", e.wr.Deleted, e.cfg.reloads)
	}
}

func TestDeleteWriterAdminErrorIs409(t *testing.T) {
	e := newEnv(t)
	e.wr.err = config.ErrAdminOwned
	rec := e.post("/servers/alpha/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "defined by the administrator", `name="command.command"`)
	if e.cfg.reloads != 0 {
		t.Errorf("reloads = %d", e.cfg.reloads)
	}
}

func TestDeleteForeignFileIs409WithPath(t *testing.T) {
	e := newEnv(t)
	e.wr.err = fmt.Errorf("%w: /data/config.d/zzz.yaml", config.ErrForeignFile)
	rec := e.post("/servers/alpha/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "zzz.yaml", `name="command.command"`)
	if e.cfg.reloads != 0 {
		t.Errorf("reloads = %d", e.cfg.reloads)
	}
}

func TestUpdateForeignFileIs409(t *testing.T) {
	e := newEnv(t)
	e.wr.err = fmt.Errorf("%w: /data/config.d/beta.yaml", config.ErrForeignFile)
	rec := e.post("/servers/beta/edit", url.Values{"remote.url": {"https://y.example/mcp"}, "enabled": {"1"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// The form is shown again with the message and what was posted.
	mustContain(t, rec.Body.String(), "beta.yaml", `name="remote.url"`, "https://y.example/mcp")
	if e.cfg.reloads != 0 {
		t.Errorf("reloads = %d", e.cfg.reloads)
	}
}

func TestDeleteUserFileSucceeds(t *testing.T) {
	for name, o := range map[string]config.Origin{"user": userOrigin("alpha"), "revert": overriddenOrigin("alpha")} {
		e := newEnv(t)
		e.cfg.origins["alpha"] = o
		rec := e.post("/servers/alpha/delete", nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
			t.Fatalf("%s: status %d loc %q: %s", name, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
		if len(e.wr.Deleted) != 1 || e.wr.Deleted[0] != "alpha" || e.cfg.reloads != 1 {
			t.Errorf("%s: deleted %v, reloads %d", name, e.wr.Deleted, e.cfg.reloads)
		}
	}
}

func TestImportReloadsOnce(t *testing.T) {
	e := newEnv(t)
	form := hiddenFields(t, e.post("/servers/import", url.Values{"text": {twoServersJSON}}).Body.String())
	form.Set("confirm", "1")
	if rec := e.post("/servers/import", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(e.wr.Added) != 2 || e.cfg.reloads != 1 {
		t.Errorf("added %d, reloads %d", len(e.wr.Added), e.cfg.reloads)
	}
}

func TestFormVaultReferences(t *testing.T) {
	// Vault configured in the file: references are accepted in env and
	// headers, refused in args and when malformed.
	e := newEnv(t)
	e.cfg.cfg.Vault = config.VaultConfig{Address: "https://vault.example:8200", Token: "t"}
	f := remoteForm()
	f.Set("remote.headers", "Authorization: Bearer ${vault:kv/data/gh#token}")
	f.Set("env", "A=${vault:kv/data/gh#token}")
	rec := e.post("/servers/new", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(e.wr.Added) != 1 || e.wr.Added[0].Remote.Headers["Authorization"] != "Bearer ${vault:kv/data/gh#token}" {
		t.Errorf("written spec = %+v, want the reference kept verbatim", e.wr.Added)
	}

	rec = e.post("/servers/alpha/edit", url.Values{"command.command": {"npx"}, "command.args": {"--token\n${vault:kv/data/gh#token}"}, "enabled": {"1"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("args: status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), `server &#34;alpha&#34;: ${vault:...} is not allowed in command.args`)

	f = remoteForm()
	f.Set("env", "A=${vault:kv/data/gh}")
	rec = e.post("/servers/new", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed: status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), `invalid reference &#34;${vault:kv/data/gh}&#34;`)

	// No Vault configured: a reference is refused with a clear message.
	e2 := newEnv(t)
	f = remoteForm()
	f.Set("env", "A=${vault:kv/data/gh#token}")
	rec = e2.post("/servers/new", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no vault: status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "vault is not configured (no vault.address)")
	if len(e2.wr.Added) != 0 {
		t.Errorf("written anyway: %+v", e2.wr.Added)
	}
}

// catalogEntries builds n catalog entries named cat-00.. of the given type.
func catalogEntries(n int) []catalog.Entry {
	out := make([]catalog.Entry, 0, n)
	for i := range n {
		name := fmt.Sprintf("cat-%02d", i)
		out = append(out, catalog.Entry{
			Name: name, Type: "remote", Category: "development", File: name + ".yaml",
			Description: "Entry " + name + ".", Upstream: "https://example.com/" + name,
			Spec: config.ServerSpec{Name: name, Type: "remote", Remote: &config.RemoteSpec{URL: "https://" + name + ".example/mcp"}},
		})
	}
	return out
}

func githubEntry() catalog.Entry {
	return catalog.Entry{
		Name: "github", Type: "command", Category: "development", File: "github.yaml",
		Description: "GitHub's official MCP server, run locally from its image.",
		Upstream:    "https://github.com/github/github-mcp-server",
		Secret:      "vault kv put kv/mcp-machine/github token=ghp_...",
		Spec: config.ServerSpec{Name: "github", Type: "command", Command: &config.CommandSpec{
			Run: "docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server",
			Env: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${vault:kv/data/mcp-machine/github#token}"},
		}},
	}
}

func TestPickPageWithoutCatalog(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/servers/new")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "Add a server", "/servers/new?type=remote")
	if strings.Contains(body, "Catalog") || strings.Contains(body, "pager") {
		t.Error("a catalog section was rendered without a catalog")
	}
}

func TestPickPageCatalogPager(t *testing.T) {
	e := newEnv(t)
	e.catalog = catalogEntries(8)
	rec := e.get("/servers/new")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, `id="catalog-heading"`, "Page 1 of 2 (8 servers)", `href="/servers/new?page=2"`,
		`href="/servers/new?type=remote&amp;catalog=cat-00"`, "Entry cat-05.", `<span class="tag">development</span>`)
	if strings.Contains(body, "cat-06") {
		t.Error("page 1 shows an entry of page 2")
	}
	if strings.Contains(body, `href="/servers/new?page=0"`) {
		t.Error("page 1 links to a previous page")
	}

	rec = e.get("/servers/new?page=2")
	body = rec.Body.String()
	mustContain(t, body, "Page 2 of 2", `href="/servers/new?page=1"`, "cat-06", "cat-07")
	if strings.Contains(body, "cat-05.") || strings.Contains(body, `href="/servers/new?page=3"`) {
		t.Error("page 2 shows page 1 entries or a next link")
	}

	// Out of range pages clamp.
	if body := e.get("/servers/new?page=9").Body.String(); !strings.Contains(body, "Page 2 of 2") {
		t.Error("page 9 did not clamp to the last page")
	}
	if body := e.get("/servers/new?page=-3").Body.String(); !strings.Contains(body, "Page 1 of 2") {
		t.Error("page -3 did not clamp to the first page")
	}

	// A card for an entry with a Vault secret says so.
	e.catalog = []catalog.Entry{githubEntry()}
	body = e.get("/servers/new").Body.String()
	mustContain(t, body, "Needs a Vault secret", "vault kv put kv/mcp-machine/github token=ghp_...", "Page 1 of 1 (1 servers)")
}

func TestNewFormPrefilledFromCatalog(t *testing.T) {
	e := newEnv(t)
	e.catalog = []catalog.Entry{githubEntry()}
	rec := e.get("/servers/new?type=command&catalog=github")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body,
		`name="name" value="github"`,
		"docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server",
		"GITHUB_PERSONAL_ACCESS_TOKEN=${vault:kv/data/mcp-machine/github#token}",
		`<div class="info banner">From the catalog: GitHub&#39;s official MCP server, run locally from its image. Check the host paths`,
		// No Vault in the test config: the heads-up is shown.
		"no vault.address is configured")
	if !strings.Contains(body, `value="1" checked`) {
		t.Error("enabled is not checked")
	}

	// With Vault configured the heads-up goes away.
	e.cfg.cfg.Vault = config.VaultConfig{Address: "https://vault.example:8200", Token: "t"}
	if body := e.get("/servers/new?type=command&catalog=github").Body.String(); strings.Contains(body, "no vault.address") {
		t.Error("the Vault heads-up is shown although vault.address is set")
	}

	// Unknown entry, and an entry opened under the wrong type.
	if rec := e.get("/servers/new?type=command&catalog=nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown entry: status %d", rec.Code)
	}
	if rec := e.get("/servers/new?type=remote&catalog=github"); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong type: status %d", rec.Code)
	}
	// The plain form is unaffected.
	if body := e.get("/servers/new?type=command").Body.String(); strings.Contains(body, "info banner") {
		t.Error("the plain form shows a catalog notice")
	}
}
