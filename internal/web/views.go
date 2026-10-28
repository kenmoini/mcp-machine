package web

import (
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/store"
)

var funcs = template.FuncMap{
	"since": since,
}

// since formats t relative to now ("3m ago"); the zero time is "-".
func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// serverView is one server as the row and status partials show it.
type serverView struct {
	Name        string
	Type        string
	State       string
	Image       string
	Since       time.Time
	Restarts    int
	Paused      bool
	Error       string
	Hash        string
	ContainerID string
	CanRebuild  bool
}

func (u *ui) serverView(st store.Status) serverView {
	v := serverView{
		Name: st.Name, State: string(st.State), Image: st.Image, Since: st.Since,
		Restarts: st.Restarts, Paused: st.Paused, Error: st.Error,
		Hash: short(st.Hash), ContainerID: short(st.ContainerID), Type: "-",
	}
	if spec, ok := u.specFor(st.Name); ok {
		v.Type = spec.Type
		v.CanRebuild = spec.Type == "build"
	}
	return v
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// specView is the display summary of a configured server. It deliberately
// carries no environment, header or credential fields.
type specView struct {
	Type    string
	Enabled bool
	Source  string
	Serve   string
}

func summarize(s config.ServerSpec, cfg *config.Config) specView {
	v := specView{Type: s.Type, Enabled: s.IsEnabled()}
	switch {
	case s.Remote != nil:
		v.Source = redactURL(s.Remote.URL)
		if s.Remote.Transport != "" {
			v.Source += " (" + s.Remote.Transport + ")"
		}
	case s.Build != nil:
		v.Source = redactURL(s.Build.Repo)
		if s.Build.Ref != "" {
			v.Source += " @ " + s.Build.Ref
		}
		if s.Build.Subdir != "" {
			v.Source += " /" + s.Build.Subdir
		}
	case s.Command != nil && s.Command.Run != "":
		// command.run is never expanded by the loader: ${VAR}s show as
		// written.
		v.Source = s.Command.Run
		if s.Container != nil && s.Container.Image != "" {
			v.Source += " (image " + s.Container.Image + ")"
		}
	case s.Command != nil:
		v.Source = s.Command.Command
		if img := s.RuntimeImage(cfg); img != "" {
			v.Source += " in " + img
		}
	case s.Container != nil && s.Container.Image != "":
		v.Source = s.Container.Image
	}
	if s.Serve != nil {
		var parts []string
		for _, p := range []string{s.Serve.Mode, s.Serve.Transport, s.Serve.Path} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if s.Serve.Port != 0 {
			parts = append(parts, fmt.Sprintf("port %d", s.Serve.Port))
		}
		v.Serve = strings.Join(parts, " ")
	}
	return v
}

// redactURL drops userinfo and query from a URL, which is where credentials
// travel; anything that does not parse as a URL is returned unchanged.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
