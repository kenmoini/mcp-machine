package web

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// maxFormBytes caps a posted form; configs are small.
const maxFormBytes = 1 << 20

var serverTypes = []string{"remote", "command", "build"}

// formData is what the form pages render.
type formData struct {
	Type    string
	Name    string // the server being edited; empty when adding
	Editing bool
	Action  string
	Values  url.Values
	Errors  []string
	Delete  deleteAction // the edit form's delete section
}

// deleteAction is what the edit form offers to remove a server, which
// depends on the layers that define it.
type deleteAction struct {
	Mode string // "delete", "revert", or "none"
	Note string // shown under the button, or the reason when Mode is "none"
}

// deleteActionFor returns the delete action for server name defined by o;
// found is false when the origin is unknown. A server only the UI defines
// is deleted; an admin-defined one with a UI override reverts to the admin
// definition; an admin-only one cannot be deleted.
func deleteActionFor(name string, o config.Origin, found bool) deleteAction {
	if !found {
		return deleteAction{Mode: "none"}
	}
	user := o.UserFile()
	switch {
	case user != "" && !o.Admin():
		return deleteAction{Mode: "delete", Note: "Removes " + user + ". Its container is stopped and removed."}
	case user != "":
		return deleteAction{Mode: "revert", Note: "Removes " + user + "; " + name + " goes back to the administrator's definition."}
	}
	var admin []string
	for _, f := range o.Files {
		if f.Layer != config.LayerUser {
			admin = append(admin, f.Path)
		}
	}
	if len(admin) == 0 {
		return deleteAction{Mode: "none"}
	}
	return deleteAction{Mode: "none", Note: "Defined by the administrator in " + strings.Join(admin, ", ") +
		"; it cannot be deleted here, only overridden by saving changes."}
}

// deleteActionOf returns the delete action for the configured server name.
func (u *ui) deleteActionOf(name string) deleteAction {
	o, found := u.Config.Origin(name)
	return deleteActionFor(name, o, found)
}

// V returns the trimmed-for-display value of form field k.
func (d formData) V(k string) string { return d.Values.Get(k) }

// Enabled reports whether the enabled box starts checked: it does unless the
// last posted value is "0".
func (d formData) Enabled() bool {
	vals := d.Values["enabled"]
	return len(vals) == 0 || strings.TrimSpace(vals[len(vals)-1]) != "0"
}

// lines splits a textarea into trimmed, non-empty lines (nil when none).
func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// pairs parses a textarea of "K<sep>V" lines into a map (nil when there are
// none). Problems name the field and the 1-based line.
func pairs(field, text, sep, shape string) (map[string]string, []error) {
	var m map[string]string
	var errs []error
	for i, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, sep)
		if k = strings.TrimSpace(k); !ok || k == "" {
			errs = append(errs, fmt.Errorf("%s: line %d (%q) is not %s", field, i+1, l, shape))
			continue
		}
		if m == nil {
			m = map[string]string{}
		}
		m[k] = strings.TrimSpace(v)
	}
	return m, errs
}

// specFromForm builds a server spec from posted form values. Values are
// trimmed; empty optional fields leave their section nil (the section of the
// chosen type is always present so validation can name what is missing). The
// result carries placeholders such as ${VAR} exactly as typed.
func specFromForm(v url.Values) (config.ServerSpec, error) {
	var errs []error
	get := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	anySet := func(keys ...string) bool {
		return slices.ContainsFunc(keys, func(k string) bool { return get(k) != "" })
	}
	kv := func(field, sep, shape string) map[string]string {
		m, e := pairs(field, v.Get(field), sep, shape)
		errs = append(errs, e...)
		return m
	}
	const envShape, hdrShape = "KEY=VALUE", `"Key: Value"`

	s := config.ServerSpec{Name: get("name"), Type: get("type")}
	// A checkbox only posts when checked, so the form sends a hidden "0"
	// first; the last value wins.
	if vals := v["enabled"]; len(vals) > 0 {
		switch strings.TrimSpace(vals[len(vals)-1]) {
		case "0", "false":
			off := false
			s.Enabled = &off
		}
	}
	s.Env = kv("env", "=", envShape)

	if s.Type == "remote" || anySet("remote.url", "remote.transport", "remote.headers") {
		s.Remote = &config.RemoteSpec{
			URL: get("remote.url"), Transport: get("remote.transport"),
			Headers: kv("remote.headers", ":", hdrShape),
		}
	}
	if s.Type == "command" || anySet("command.command", "command.args", "command.env", "command.run") {
		s.Command = &config.CommandSpec{
			Command: get("command.command"), Args: lines(v.Get("command.args")),
			Env: kv("command.env", "=", envShape), Run: strings.ReplaceAll(get("command.run"), "\r\n", "\n"),
		}
	}
	if s.Type == "build" || anySet("build.repo", "build.ref", "build.subdir", "build.containerfile", "build.args",
		"build.auth.token", "build.auth.ssh_key_file", "build.auth.known_hosts_file") {
		b := &config.BuildSpec{
			Repo: get("build.repo"), Ref: get("build.ref"), Subdir: get("build.subdir"),
			Containerfile: get("build.containerfile"), Args: kv("build.args", "=", envShape),
		}
		if anySet("build.auth.token", "build.auth.ssh_key_file", "build.auth.known_hosts_file") {
			b.Auth = &config.GitAuth{
				Token: get("build.auth.token"), SSHKeyFile: get("build.auth.ssh_key_file"),
				KnownHostsFile: get("build.auth.known_hosts_file"),
			}
		}
		s.Build = b
	}
	if anySet("serve.mode", "serve.port", "serve.path", "serve.transport") {
		sv := &config.ServeSpec{Mode: get("serve.mode"), Path: get("serve.path"), Transport: get("serve.transport")}
		if p := get("serve.port"); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("serve.port: %q is not a number", p))
			}
			sv.Port = n
		}
		s.Serve = sv
	}
	if anySet("container.image", "container.volumes", "container.env") {
		s.Container = &config.ContainerOverrides{
			Image: get("container.image"), Volumes: lines(v.Get("container.volumes")),
			Env: kv("container.env", "=", envShape),
		}
	}
	return s, errors.Join(errs...)
}

// formFromSpec is the inverse of specFromForm: the values a form would post to
// produce s. Empty fields are omitted and map entries are sorted, so the same
// spec always yields the same form.
func formFromSpec(s config.ServerSpec) url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	join := func(k string, items []string) { set(k, strings.Join(items, "\n")) }
	mapLines := func(k, sep string, m map[string]string) {
		out := make([]string, 0, len(m))
		for _, key := range slices.Sorted(maps.Keys(m)) {
			out = append(out, key+sep+m[key])
		}
		join(k, out)
	}

	set("name", s.Name)
	set("type", s.Type)
	if s.IsEnabled() {
		v.Set("enabled", "1")
	} else {
		v.Set("enabled", "0")
	}
	mapLines("env", "=", s.Env)
	if r := s.Remote; r != nil {
		set("remote.url", r.URL)
		set("remote.transport", r.Transport)
		mapLines("remote.headers", ": ", r.Headers)
	}
	if c := s.Command; c != nil {
		set("command.command", c.Command)
		join("command.args", c.Args)
		mapLines("command.env", "=", c.Env)
		set("command.run", c.Run)
	}
	if b := s.Build; b != nil {
		set("build.repo", b.Repo)
		set("build.ref", b.Ref)
		set("build.subdir", b.Subdir)
		set("build.containerfile", b.Containerfile)
		mapLines("build.args", "=", b.Args)
		if b.Auth != nil {
			set("build.auth.token", b.Auth.Token)
			set("build.auth.ssh_key_file", b.Auth.SSHKeyFile)
			set("build.auth.known_hosts_file", b.Auth.KnownHostsFile)
		}
	}
	if sv := s.Serve; sv != nil {
		set("serve.mode", sv.Mode)
		if sv.Port != 0 {
			set("serve.port", strconv.Itoa(sv.Port))
		}
		set("serve.path", sv.Path)
		set("serve.transport", sv.Transport)
	}
	if c := s.Container; c != nil {
		set("container.image", c.Image)
		join("container.volumes", c.Volumes)
		mapLines("container.env", "=", c.Env)
	}
	return v
}

// messages flattens an error (possibly a errors.Join tree) into one message
// per leaf.
func messages(err error) []string {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range j.Unwrap() {
			out = append(out, messages(e)...)
		}
		return out
	}
	return []string{err.Error()}
}

// editable reports whether the editing forms are available, else answers 503.
func (u *ui) editable(w http.ResponseWriter) bool {
	if u.Writer == nil || u.Config == nil {
		http.Error(w, "editing is not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// parsePost reads the posted form with a size cap.
func parsePost(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// rawSpec returns the unexpanded spec for name from the configuration file.
func (u *ui) rawSpec(name string) (spec config.ServerSpec, found bool, err error) {
	raw, err := u.Config.Raw()
	if err != nil {
		return config.ServerSpec{}, false, err
	}
	i := slices.IndexFunc(raw.Servers, func(s config.ServerSpec) bool { return s.Name == name })
	if i < 0 {
		return config.ServerSpec{}, false, nil
	}
	return raw.Servers[i], true, nil
}

// cloneSections copies s's section structs so applying defaults to the copy
// cannot touch the original.
func cloneSections(s config.ServerSpec) config.ServerSpec {
	if s.Remote != nil {
		c := *s.Remote
		s.Remote = &c
	}
	if s.Command != nil {
		c := *s.Command
		s.Command = &c
	}
	if s.Build != nil {
		c := *s.Build
		s.Build = &c
	}
	if s.Serve != nil {
		c := *s.Serve
		s.Serve = &c
	}
	if s.Container != nil {
		c := *s.Container
		s.Container = &c
	}
	return s
}

// stripDefaults returns s without the values config.ApplyDefaults would fill
// in anyway, found by applying the defaults to an empty spec with the same
// sections. Raw() parses the file, so its specs carry those defaults; an
// edit form prefilled with them would write them back as explicit keys. s
// is not modified.
func stripDefaults(s config.ServerSpec) config.ServerSpec {
	s = cloneSections(s)
	empty := config.ServerSpec{Type: s.Type}
	if s.Remote != nil {
		empty.Remote = &config.RemoteSpec{}
	}
	if s.Command != nil {
		empty.Command = &config.CommandSpec{}
	}
	if s.Build != nil {
		empty.Build = &config.BuildSpec{}
	}
	if s.Serve != nil {
		empty.Serve = &config.ServeSpec{}
	}
	defs := config.Config{Servers: []config.ServerSpec{empty}}
	config.ApplyDefaults(&defs)
	d := defs.Servers[0]

	if s.Remote != nil && d.Remote != nil && s.Remote.Transport == d.Remote.Transport {
		s.Remote.Transport = ""
	}
	if s.Build != nil && d.Build != nil && s.Build.Containerfile == d.Build.Containerfile {
		s.Build.Containerfile = ""
	}
	if s.Serve != nil && d.Serve != nil {
		if s.Serve.Mode == d.Serve.Mode {
			s.Serve.Mode = ""
		}
		if *s.Serve == (config.ServeSpec{}) {
			s.Serve = nil
		}
	}
	return s
}

// required returns the problems config.Validate does not cover.
func required(s config.ServerSpec) []string {
	var out []string
	if s.Remote != nil && s.Type == "remote" && s.Remote.URL == "" {
		out = append(out, fmt.Sprintf("server %q: remote.url is required", s.Name))
	}
	if s.Build != nil && s.Type == "build" && s.Build.Repo == "" {
		out = append(out, fmt.Sprintf("server %q: build.repo is required", s.Name))
	}
	return out
}

// check validates specs against the file's current (unexpanded) configuration
// with them added, or, when replace names a server, with specs[0] taking its
// place, and checks that every ${VAR} the specs reference is set. It returns
// the problems found; err means the file could not be read.
func (u *ui) check(replace string, specs ...config.ServerSpec) (problems []string, err error) {
	raw, err := u.Config.Raw()
	if err != nil {
		return nil, err
	}
	cp := *raw
	cp.Servers = make([]config.ServerSpec, 0, len(raw.Servers)+len(specs))
	replaced := false
	for _, s := range raw.Servers {
		if replace != "" && s.Name == replace {
			cp.Servers = append(cp.Servers, specs[0])
			replaced = true
			continue
		}
		cp.Servers = append(cp.Servers, s)
	}
	if !replaced {
		cp.Servers = append(cp.Servers, specs...)
	}
	for i := range cp.Servers {
		cp.Servers[i] = cloneSections(cp.Servers[i])
	}
	config.ApplyDefaults(&cp)

	for _, s := range specs {
		problems = append(problems, required(s)...)
		// The form validates against placeholders, but the file it writes is
		// loaded with the real environment: every variable must be set.
		for _, name := range config.UnsetVars(s, u.LookupEnv) {
			problems = append(problems, fmt.Sprintf("server %q: environment variable %s is not set", s.Name, name))
		}
	}
	return append(problems, messages(config.Validate(&cp))...), nil
}

// formPage renders the form for d.Type.
func (u *ui) formPage(w http.ResponseWriter, status int, d formData) {
	title, active := "Add "+d.Type+" server", "new"
	if d.Editing {
		title, active = "Edit "+d.Name, ""
	}
	u.page(w, status, "form_"+d.Type+".html", title, active, d)
}

func validType(t string) bool { return slices.Contains(serverTypes, t) }

func (u *ui) newForm(w http.ResponseWriter, r *http.Request) {
	if !u.editable(w) {
		return
	}
	t := r.URL.Query().Get("type")
	if t == "" {
		u.page(w, http.StatusOK, "form_pick.html", "Add server", "new", nil)
		return
	}
	if !validType(t) {
		http.Error(w, "unknown server type", http.StatusBadRequest)
		return
	}
	vals := url.Values{"type": {t}, "enabled": {"1"}}
	u.formPage(w, http.StatusOK, formData{Type: t, Action: "/servers/new", Values: vals})
}

func (u *ui) createServer(w http.ResponseWriter, r *http.Request) {
	if !u.editable(w) || !parsePost(w, r) {
		return
	}
	t := strings.TrimSpace(r.PostForm.Get("type"))
	if !validType(t) {
		http.Error(w, "unknown server type", http.StatusBadRequest)
		return
	}
	d := formData{Type: t, Action: "/servers/new", Values: r.PostForm}

	spec, err := specFromForm(r.PostForm)
	if err != nil {
		d.Errors = messages(err)
		u.formPage(w, http.StatusUnprocessableEntity, d)
		return
	}
	problems, err := u.check("", spec)
	if err != nil {
		d.Errors = []string{err.Error()}
		u.formPage(w, http.StatusInternalServerError, d)
		return
	}
	if len(problems) > 0 {
		d.Errors = problems
		u.formPage(w, http.StatusUnprocessableEntity, d)
		return
	}
	if err := u.Writer.AddServer(spec); err != nil {
		u.Logger.Warn("add server", "server", spec.Name, "err", err)
		d.Errors = []string{err.Error()}
		u.formPage(w, http.StatusInternalServerError, d)
		return
	}
	u.reloadAfterWrite()
	http.Redirect(w, r, "/servers/"+spec.Name, http.StatusSeeOther)
}

// editTarget loads the unexpanded spec of the request's {name}, or answers
// 404 (not configured) or 500 (file unreadable).
func (u *ui) editTarget(w http.ResponseWriter, r *http.Request) (config.ServerSpec, bool) {
	spec, found, err := u.rawSpec(r.PathValue("name"))
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case !found:
		http.NotFound(w, r)
	default:
		return spec, true
	}
	return config.ServerSpec{}, false
}

func (u *ui) editData(spec config.ServerSpec, vals url.Values) formData {
	return formData{
		Type: spec.Type, Name: spec.Name, Editing: true,
		Action: "/servers/" + spec.Name + "/edit", Values: vals,
		Delete: u.deleteActionOf(spec.Name),
	}
}

func (u *ui) editForm(w http.ResponseWriter, r *http.Request) {
	if !u.editable(w) {
		return
	}
	spec, ok := u.editTarget(w, r)
	if !ok {
		return
	}
	u.formPage(w, http.StatusOK, u.editData(spec, formFromSpec(stripDefaults(spec))))
}

func (u *ui) updateServer(w http.ResponseWriter, r *http.Request) {
	if !u.editable(w) || !parsePost(w, r) {
		return
	}
	cur, ok := u.editTarget(w, r)
	if !ok {
		return
	}
	// Name and type are fixed once a server exists.
	posted := maps.Clone(r.PostForm)
	posted.Set("name", cur.Name)
	posted.Set("type", cur.Type)
	d := u.editData(cur, posted)

	spec, err := specFromForm(posted)
	if err != nil {
		d.Errors = messages(err)
		u.formPage(w, http.StatusUnprocessableEntity, d)
		return
	}
	problems, err := u.check(cur.Name, spec)
	if err != nil {
		d.Errors = []string{err.Error()}
		u.formPage(w, http.StatusInternalServerError, d)
		return
	}
	if len(problems) > 0 {
		d.Errors = problems
		u.formPage(w, http.StatusUnprocessableEntity, d)
		return
	}
	if err := u.Writer.UpdateServer(cur.Name, spec); err != nil {
		u.Logger.Warn("update server", "server", cur.Name, "err", err)
		d.Errors = []string{err.Error()}
		u.formPage(w, http.StatusInternalServerError, d)
		return
	}
	u.reloadAfterWrite()
	http.Redirect(w, r, "/servers/"+cur.Name, http.StatusSeeOther)
}

func (u *ui) deleteServer(w http.ResponseWriter, r *http.Request) {
	if !u.editable(w) {
		return
	}
	cur, ok := u.editTarget(w, r)
	if !ok {
		return
	}
	// The form offers no delete for an admin-only server; refuse a posted one
	// before the writer is involved.
	if act := u.deleteActionOf(cur.Name); act.Mode == "none" {
		msg := act.Note
		if msg == "" {
			msg = cur.Name + " is not defined in a file the UI knows; it cannot be deleted here."
		}
		http.Error(w, msg, http.StatusConflict)
		return
	}
	if err := u.Writer.DeleteServer(cur.Name); err != nil {
		u.Logger.Warn("delete server", "server", cur.Name, "err", err)
		status := http.StatusInternalServerError
		if errors.Is(err, config.ErrAdminOwned) || errors.Is(err, config.ErrForeignFile) {
			status = http.StatusConflict
		}
		d := u.editData(cur, formFromSpec(stripDefaults(cur)))
		d.Errors = []string{err.Error()}
		u.formPage(w, status, d)
		return
	}
	u.reloadAfterWrite()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// reloadAfterWrite applies the file just written before the browser is
// redirected, so the page it lands on already reflects the change; the file
// watcher would do the same, but only after its debounce. Applying is
// serialized and idempotent, so the watcher's later reload changes nothing. A
// failed reload is recorded by the config state and shown on the config page.
func (u *ui) reloadAfterWrite() {
	if err := u.Config.Reload(); err != nil {
		u.Logger.Warn("reload config after edit", "err", err)
	}
}
