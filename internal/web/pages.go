package web

import (
	"encoding/json"
	"net/http"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func (u *ui) index(w http.ResponseWriter, _ *http.Request) {
	list := u.Registry.List()
	rows := make([]rowView, len(list))
	for i, st := range list {
		rows[i] = rowView{serverView: u.serverView(st)}
	}
	u.page(w, http.StatusOK, "index.html", "Dashboard", "dashboard", rows)
}

// serverData is the detail page's content.
type serverData struct {
	Server  serverView
	Spec    *specView
	Tools   toolsView
	Logs    logsView
	Connect connectView
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
	}
	if spec, ok := u.specFor(st.Name); ok {
		sv := summarize(spec, u.Config.Current())
		d.Spec = &sv
	}
	u.page(w, http.StatusOK, "server.html", st.Name, "", d)
}

// configData is the config page's content.
type configData struct {
	Config    *config.Config
	Servers   []configServer
	LastError string
}

type configServer struct {
	Name    string
	Type    string
	Enabled bool
}

func (u *ui) configPage(w http.ResponseWriter, _ *http.Request) {
	var d configData
	if u.Config != nil {
		d.Config = u.Config.Current()
		if err := u.Config.LastError(); err != nil {
			d.LastError = err.Error()
		}
	}
	if d.Config != nil {
		for _, s := range d.Config.Servers {
			d.Servers = append(d.Servers, configServer{Name: s.Name, Type: s.Type, Enabled: s.IsEnabled()})
		}
	}
	u.page(w, http.StatusOK, "config.html", "Config", "config", d)
}

func (u *ui) reload(w http.ResponseWriter, r *http.Request) {
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
