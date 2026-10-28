package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func (u *ui) index(w http.ResponseWriter, _ *http.Request) {
	u.page(w, http.StatusOK, "index.html", "Dashboard", "dashboard", u.rows())
}

// rows is the dashboard table's data: every registered server.
func (u *ui) rows() []rowView {
	list := u.Registry.List()
	rows := make([]rowView, len(list))
	for i, st := range list {
		rows[i] = rowView{serverView: u.serverView(st)}
	}
	return rows
}

// serverRows answers the dashboard's poll (?view=rows) with the whole table
// body, so servers added or removed since the page loaded show up. /servers
// is not a page of its own: anything else goes to the dashboard.
func (u *ui) serverRows(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("view") != "rows" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.render(w, http.StatusOK, u.pages["index.html"], "_server_rows.html", u.rows())
}

// serverData is the detail page's content.
type serverData struct {
	Server  serverView
	Spec    *specView
	Tools   toolsView
	Logs    logsView
	Connect connectView
	CanEdit bool
}

func (u *ui) serverPage(w http.ResponseWriter, r *http.Request) {
	st, ok := u.known(w, r)
	if !ok {
		return
	}
	v := u.serverView(st)
	d := serverData{
		Server:  v,
		Tools:   u.toolsView(st.Name),
		Logs:    u.logsView(r, st.Name, defaultTail),
		Connect: u.connectView(r, st.Name),
		CanEdit: u.Writer != nil,
	}
	if spec, ok := u.specFor(st.Name); ok {
		sv := summarize(spec, u.Config.Current())
		sv.Provenance = u.provenance(st.Name)
		d.Spec = &sv
	}
	u.page(w, http.StatusOK, "server.html", st.Name, "", d)
}

// configData is the config page's content.
type configData struct {
	Config     *config.Config
	Registries []configRegistry
	Servers    []configServer
	LastError  string
	// Warning is the notice about where the configuration came from.
	Warning string
	// Files are the files the configuration was read from, in load order.
	Files []string
}

// configRegistry is a registry as the config page shows it: the username as
// written in the file (a ${VAR} stays a placeholder), never the password.
type configRegistry struct {
	Registry string
	Username string
}

type configServer struct {
	Name    string
	Type    string
	Enabled bool
	Origin  string // where it is defined, as on the server page
}

// provenance describes where server name is defined, without the leading
// "Defined in ", or "" when that is unknown.
func (u *ui) provenance(name string) string {
	o, ok := u.Config.Origin(name)
	if !ok {
		return ""
	}
	return strings.TrimPrefix(o.String(), "Defined in ")
}

func (u *ui) configPage(w http.ResponseWriter, _ *http.Request) {
	var d configData
	if u.Config != nil {
		d.Config = u.Config.Current()
		if err := u.Config.LastError(); err != nil {
			d.LastError = err.Error()
		}
		d.Warning = u.Config.Warning()
		d.Files = u.Config.Files()
	}
	if d.Config != nil {
		d.Registries = u.rawRegistries(d.Config)
		for _, s := range d.Config.Servers {
			d.Servers = append(d.Servers, configServer{
				Name: s.Name, Type: s.Type, Enabled: s.IsEnabled(), Origin: u.provenance(s.Name),
			})
		}
	}
	u.page(w, http.StatusOK, "config.html", "Config", "config", d)
}

// rawRegistries lists cfg's registries with their usernames as written in
// the file. When the file cannot be read (or no longer matches cfg) the
// usernames are left out rather than shown expanded.
func (u *ui) rawRegistries(cfg *config.Config) []configRegistry {
	out := make([]configRegistry, len(cfg.Registries))
	for i, r := range cfg.Registries {
		out[i].Registry = r.Registry
	}
	raw, err := u.Config.Raw()
	if err != nil {
		return out
	}
	for i := range out {
		if i < len(raw.Registries) && raw.Registries[i].Registry == out[i].Registry {
			out[i].Username = raw.Registries[i].Username
		}
	}
	return out
}

func (u *ui) reload(w http.ResponseWriter, r *http.Request) {
	if u.Config == nil {
		http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := u.Config.Reload(); err != nil {
		// The watcher records the failure; the config page shows it.
		u.Logger.Warn("reload config", "err", err)
	}
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func (u *ui) apiServers(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(u.Registry.List()); err != nil {
		u.Logger.Error("encode servers", "err", err)
	}
}
