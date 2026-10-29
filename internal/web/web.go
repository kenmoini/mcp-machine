// Package web is the server-rendered management UI: a dashboard, per-server
// detail, logs and configuration pages, htmx partials and the actions behind
// them. The handler owns its own ServeMux; mount it at "/" after the protocol
// adapters so their more specific /servers/{name}/mcp and /sse patterns win.
package web

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/adapters/httpadapter"
	"github.com/kenmoini/mcp-machine/internal/catalog"
	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/proxy"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// The all: prefix embeds the partials, whose names start with an underscore.
//
//go:embed all:templates static
var assets embed.FS

// ManagerAPI is the part of the server manager the UI drives.
type ManagerAPI interface {
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Rebuild(ctx context.Context, name string) error
	Resync(ctx context.Context, name string) error
	Facade(name string) (*proxy.Facade, bool)
	Logs(ctx context.Context, name string, tail int) ([]runtime.LogLine, error)
}

// ConfigView is the process's view of its configuration.
type ConfigView interface {
	// Current returns the configuration in effect.
	Current() *config.Config
	// LastError returns the error of the latest failed reload, or nil.
	LastError() error
	// Reload re-reads the configuration file now.
	Reload() error
	// Raw re-reads the configuration file with ${VAR} placeholders left
	// unexpanded. The editing forms read and validate against it; Current
	// holds expanded secrets and must never be written back to the file.
	Raw() (*config.Config, error)
	// Files returns the files the configuration in effect was read from,
	// in load order.
	Files() []string
	// Origin reports which files define the named server.
	Origin(name string) (config.Origin, bool)
	// Warning returns a notice about where the configuration came from
	// (the deprecated location, or nowhere at all), or "".
	Warning() string
}

// ConfigWriter edits the configuration file (used by the forms).
type ConfigWriter interface {
	AddServer(config.ServerSpec) error
	UpdateServer(string, config.ServerSpec) error
	DeleteServer(string) error
}

// Deps are the collaborators of the UI. Writer may be nil, in which case the
// editing forms are unavailable.
type Deps struct {
	Manager  ManagerAPI
	Registry *store.Registry
	Config   ConfigView
	Writer   ConfigWriter
	// EditNotice, when Writer is nil, says why editing is disabled.
	EditNotice string
	PublicURL  string
	// AllowedOrigins are the configured allowed_origins; with PublicURL they
	// name the hosts state-changing requests may be addressed to.
	AllowedOrigins []string
	Version        string
	Logger         *slog.Logger
	// LookupEnv resolves the ${VAR} references of an edited spec, which must
	// all be set for the written file to load. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Catalog returns the ready-made servers the Add server page offers as
	// cards, read afresh on each call. Nil means no catalog.
	Catalog func() []catalog.Entry
}

const (
	defaultTail = 200
	maxTail     = 2000
	staticAge   = "public, max-age=86400"
)

// fragments are the partials that can also be requested as full pages.
var fragments = []string{"_status.html", "_logs.html", "_tools.html"}

// ui is the handler state: dependencies and the parsed templates.
type ui struct {
	Deps
	pages map[string]*template.Template // page file name -> layout + partials + page
	frags map[string]*template.Template // partial name -> layout + partials + wrapper
}

// New returns the UI handler. Templates are parsed once, here; a parse error
// is a programming error and panics.
func New(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.LookupEnv == nil {
		d.LookupEnv = os.LookupEnv
	}
	u := &ui{Deps: d, pages: map[string]*template.Template{}, frags: map[string]*template.Template{}}

	base := func() *template.Template {
		return template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(assets, "templates/layout.html", "templates/partials/*.html"))
	}
	for _, page := range []string{
		"index.html", "server.html", "config.html",
		"form_pick.html", "form_remote.html", "form_command.html", "form_build.html", "import.html",
	} {
		u.pages[page] = template.Must(base().ParseFS(assets, "templates/"+page))
	}
	for _, p := range fragments {
		u.frags[p] = template.Must(base().Parse(`{{define "content"}}{{template "` + p + `" .}}{{end}}`))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", u.index)
	mux.HandleFunc("GET /servers", u.serverRows)
	mux.HandleFunc("GET /servers/{name}", u.serverPage)
	mux.HandleFunc("GET /servers/{name}/status", u.status)
	mux.HandleFunc("GET /servers/{name}/logs", u.logs)
	mux.HandleFunc("GET /servers/{name}/tools", u.tools)
	mux.HandleFunc("POST /servers/{name}/actions/{action}", u.action)
	mux.HandleFunc("GET /config", u.configPage)
	mux.HandleFunc("POST /config/reload", u.reload)
	mux.HandleFunc("GET /api/servers", u.apiServers)
	// Literal segments outrank {name}, so these never reach serverPage.
	mux.HandleFunc("GET /servers/new", u.newForm)
	mux.HandleFunc("POST /servers/new", u.createServer)
	mux.HandleFunc("GET /servers/import", u.importForm)
	mux.HandleFunc("POST /servers/import", u.importServers)
	mux.HandleFunc("GET /servers/{name}/edit", u.editForm)
	mux.HandleFunc("POST /servers/{name}/edit", u.updateServer)
	mux.HandleFunc("POST /servers/{name}/delete", u.deleteServer)
	mux.Handle("GET /static/{file...}", staticHandler())
	return u.guardHost(u.sameOrigin(mux))
}

// guardHost applies httpadapter.HostGuard to state-changing requests: a
// DNS-rebinding page sends Origin and Host both naming its own host, which
// sameOrigin accepts.
func (u *ui) guardHost(next http.Handler) http.Handler {
	guarded := httpadapter.HostGuard(httpadapter.Options{PublicURL: u.PublicURL, AllowedOrigins: u.AllowedOrigins})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

// sameOrigin rejects state-changing requests whose Origin header names
// another site, which stops cross-site form posts from driving the UI.
// Requests without an Origin header (curl, scripts) pass.
func (u *ui) sameOrigin(next http.Handler) http.Handler {
	var public string
	if pu, err := url.Parse(u.PublicURL); err == nil {
		public = pu.Host
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				ou, err := url.Parse(o)
				if err != nil || (ou.Host != r.Host && (public == "" || ou.Host != public)) {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func staticHandler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if info, err := fs.Stat(sub, name); err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", staticAge)
		files.ServeHTTP(w, r)
	})
}

// layoutData is what layout.html receives; Data goes to the page's content.
type layoutData struct {
	Title   string
	Active  string
	Flash   string
	Version string
	CanEdit bool
	// EditNotice says why editing is disabled; shown on every page.
	EditNotice string
	Data       any
}

// render executes tmpl's template name with data into a buffer first, so a
// template error becomes a clean 500 rather than a half-written page.
func (u *ui) render(w http.ResponseWriter, status int, tmpl *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		u.Logger.Error("render template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// page renders a full page inside the layout.
func (u *ui) page(w http.ResponseWriter, status int, file, title, active string, data any) {
	u.render(w, status, u.pages[file], "layout.html",
		layoutData{Title: title, Active: active, Version: u.Version, CanEdit: u.Writer != nil, EditNotice: u.EditNotice, Data: data})
}

// fragment renders partial name with data: bare for htmx requests, wrapped in
// the layout otherwise (so the URL also works when opened directly).
func (u *ui) fragment(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	if r.Header.Get("HX-Request") == "true" {
		u.render(w, http.StatusOK, u.frags[name], name, data)
		return
	}
	u.render(w, http.StatusOK, u.frags[name], "layout.html",
		layoutData{Title: title, Version: u.Version, CanEdit: u.Writer != nil, EditNotice: u.EditNotice, Data: data})
}

// known returns the registry status for the request's {name}, or writes a 404.
func (u *ui) known(w http.ResponseWriter, r *http.Request) (store.Status, bool) {
	st, ok := u.Registry.Get(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
	}
	return st, ok
}

// knownPoll is known for the polled partials. When the server has gone away
// mid-page, an htmx poll must not swap an error body over its block: it gets
// an empty 200 instead. A row (row=true) is swapped out, which removes it;
// the detail page's blocks refresh the whole page. Other requests get a 404.
func (u *ui) knownPoll(w http.ResponseWriter, r *http.Request, row bool) (store.Status, bool) {
	st, ok := u.Registry.Get(r.PathValue("name"))
	if ok {
		return st, true
	}
	if r.Header.Get("HX-Request") != "true" {
		http.NotFound(w, r)
		return st, false
	}
	if row {
		w.Header().Set("HX-Reswap", "outerHTML")
	} else {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusOK)
	return st, false
}

// specFor returns the configured spec for name.
func (u *ui) specFor(name string) (config.ServerSpec, bool) {
	if u.Config == nil {
		return config.ServerSpec{}, false
	}
	cfg := u.Config.Current()
	if cfg == nil {
		return config.ServerSpec{}, false
	}
	i := slices.IndexFunc(cfg.Servers, func(s config.ServerSpec) bool { return s.Name == name })
	if i < 0 {
		return config.ServerSpec{}, false
	}
	return cfg.Servers[i], true
}

// baseURL is the externally visible URL clients should use: the configured
// public_url, else the address the request came in on.
func (u *ui) baseURL(r *http.Request) string {
	if u.PublicURL != "" {
		return strings.TrimRight(u.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
