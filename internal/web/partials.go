package web

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/kenmoini/mcp-machine/internal/proxy"
)

// rowView is the _server_row partial's data: the server plus any error from
// the action that just ran.
type rowView struct {
	serverView
	ActionError string
}

func (u *ui) status(w http.ResponseWriter, r *http.Request) {
	row := r.URL.Query().Get("view") == "row"
	st, ok := u.knownPoll(w, r, row)
	if !ok {
		return
	}
	v := u.serverView(st)
	if row {
		// The dashboard polls each row through this view.
		u.render(w, http.StatusOK, u.pages["index.html"], "_server_row.html", rowView{serverView: v})
		return
	}
	u.fragment(w, r, "_status.html", st.Name+" status", v)
}

// actions maps the action URL segment to its manager call.
func (u *ui) actions() map[string]func(r *http.Request, name string) error {
	return map[string]func(*http.Request, string) error{
		"start":   func(r *http.Request, n string) error { return u.Manager.Start(r.Context(), n) },
		"stop":    func(r *http.Request, n string) error { return u.Manager.Stop(r.Context(), n) },
		"restart": func(r *http.Request, n string) error { return u.Manager.Restart(r.Context(), n) },
		"remove":  func(r *http.Request, n string) error { return u.Manager.Remove(r.Context(), n) },
		"rebuild": func(r *http.Request, n string) error { return u.Manager.Rebuild(r.Context(), n) },
		"resync":  func(r *http.Request, n string) error { return u.Manager.Resync(r.Context(), n) },
	}
}

// flashView is the _flash partial's data, shown after an action started from
// the detail page (whose target is the layout's flash area).
type flashView struct{ Action, Error string }

func (u *ui) action(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	do, ok := u.actions()[action]
	if !ok {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if _, ok := u.known(w, r); !ok {
		return
	}

	status := http.StatusOK
	var actionErr string
	if err := do(r, name); err != nil {
		u.Logger.Warn("server action failed", "server", name, "action", action, "err", err)
		actionErr = err.Error()
		status = http.StatusInternalServerError
	}

	st, exists := u.Registry.Get(name)
	if !exists {
		// Removed. From the detail page there is no row to drop, so go back
		// to the dashboard.
		if cur, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && cur.Path == "/servers/"+name {
			w.Header().Set("HX-Redirect", "/")
		}
	}
	if r.Header.Get("HX-Target") == "flash" {
		u.render(w, status, u.pages["server.html"], "_flash.html", flashView{Action: action, Error: actionErr})
		return
	}
	if !exists {
		if actionErr != "" {
			http.Error(w, actionErr, status)
			return
		}
		// An empty body swapped over the row removes it.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	u.render(w, status, u.pages["index.html"], "_server_row.html",
		rowView{serverView: u.serverView(st), ActionError: actionErr})
}

// logLine is one rendered log line.
type logLine struct {
	Stderr bool
	Text   string
}

// logsView is the _logs partial's data.
type logsView struct {
	Name  string
	Tail  int
	Lines []logLine
	Error string
}

// tailParam reads ?tail=N: default when absent or invalid, capped at maxTail.
func tailParam(r *http.Request, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("tail"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, maxTail)
}

func (u *ui) logsView(r *http.Request, name string, def int) logsView {
	v := logsView{Name: name, Tail: tailParam(r, def)}
	lines, err := u.Manager.Logs(r.Context(), name, v.Tail)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	for _, l := range lines {
		v.Lines = append(v.Lines, logLine{Stderr: l.Stream == "stderr", Text: l.Text})
	}
	return v
}

func (u *ui) logs(w http.ResponseWriter, r *http.Request) {
	st, ok := u.knownPoll(w, r, false)
	if !ok {
		return
	}
	u.fragment(w, r, "_logs.html", st.Name+" logs", u.logsView(r, st.Name, defaultTail))
}

// toolsView is the _tools partial's data: what the server's facade mirrors.
type toolsView struct {
	Name      string
	HasFacade bool
	Snapshot  proxy.Snapshot
}

func (u *ui) toolsView(name string) toolsView {
	v := toolsView{Name: name}
	if f, ok := u.Manager.Facade(name); ok && f != nil {
		v.HasFacade = true
		v.Snapshot = f.Snapshot()
	}
	return v
}

func (u *ui) tools(w http.ResponseWriter, r *http.Request) {
	st, ok := u.knownPoll(w, r, false)
	if !ok {
		return
	}
	u.fragment(w, r, "_tools.html", st.Name+" tools", u.toolsView(st.Name))
}

// connectView is the _connect partial's data: ready-to-paste client commands.
type connectView struct {
	Name, URL, ClaudeHTTP, ClaudeStdio, MCPRemote string
}

func (u *ui) connectView(r *http.Request, name string) connectView {
	mcpURL := u.baseURL(r) + "/servers/" + name + "/mcp"
	return connectView{
		Name:        name,
		URL:         mcpURL,
		ClaudeHTTP:  "claude mcp add --transport http " + name + " " + mcpURL,
		ClaudeStdio: "claude mcp add " + name + " -- mcp-machine connect " + mcpURL,
		MCPRemote:   "npx mcp-remote " + mcpURL,
	}
}
