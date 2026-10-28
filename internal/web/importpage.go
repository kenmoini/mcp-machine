package web

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources/clientconfig"
	"github.com/kenmoini/mcp-machine/internal/sources/dockerrun"
)

// importData is the import page's content: the paste box and, once something
// parsed, the preview of what would be added.
type importData struct {
	Text        string
	Errors      []string
	Entries     []importEntry
	Confirmable bool
}

// importEntry is one row of the preview. Fields carry the entry's spec as
// hidden form fields so the confirmation does not depend on re-parsing text.
type importEntry struct {
	Name, Type, Summary string
	Fields              []hiddenField
}

type hiddenField struct{ Name, Value string }

func (u *ui) importForm(w http.ResponseWriter, _ *http.Request) {
	if !u.editable(w) {
		return
	}
	u.page(w, http.StatusOK, "import.html", "Import", "import", importData{})
}

// imageName derives a server name from an image reference: its last path
// segment without tag or digest, sanitized.
func imageName(image string) string {
	image, _, _ = strings.Cut(image, "@")
	seg := path.Base(image)
	seg, _, _ = strings.Cut(seg, ":")
	return clientconfig.SanitizeName(seg)
}

// parseImport turns pasted text into specs: client-config JSON when it starts
// with "{", else a docker/podman run line.
func parseImport(text string) ([]config.ServerSpec, error) {
	if text == "" {
		return nil, errors.New("paste a client config (JSON) or a docker run command")
	}
	if strings.HasPrefix(text, "{") {
		specs, err := clientconfig.Parse([]byte(text))
		if err != nil {
			return nil, err
		}
		if len(specs) == 0 {
			return nil, errors.New("no servers found in the pasted config")
		}
		return specs, nil
	}
	run, err := dockerrun.Parse(text)
	if err != nil {
		return nil, err
	}
	name := imageName(run.Image)
	if name == "" {
		return nil, fmt.Errorf("cannot derive a server name from image %q", run.Image)
	}
	return []config.ServerSpec{{Name: name, Type: "command", Command: &config.CommandSpec{Run: text}}}, nil
}

// summary is the one-line description of a spec for the preview.
func summary(s config.ServerSpec) string {
	switch {
	case s.Remote != nil:
		out := redactURL(s.Remote.URL)
		if s.Remote.Transport != "" {
			out += " (" + s.Remote.Transport + ")"
		}
		return out
	case s.Command != nil && s.Command.Run != "":
		return s.Command.Run
	case s.Command != nil:
		return strings.Join(append([]string{s.Command.Command}, s.Command.Args...), " ")
	case s.Build != nil:
		return redactURL(s.Build.Repo)
	}
	return ""
}

func entriesFor(specs []config.ServerSpec) []importEntry {
	out := make([]importEntry, len(specs))
	for i, s := range specs {
		e := importEntry{Name: s.Name, Type: s.Type, Summary: summary(s)}
		form := formFromSpec(s)
		for _, k := range slices.Sorted(maps.Keys(form)) {
			e.Fields = append(e.Fields, hiddenField{Name: "entries[" + strconv.Itoa(i) + "]." + k, Value: form.Get(k)})
		}
		out[i] = e
	}
	return out
}

func (u *ui) importServers(w http.ResponseWriter, r *http.Request) {
	if !u.editable(w) || !parsePost(w, r) {
		return
	}
	if r.PostForm.Get("confirm") == "1" {
		u.importConfirm(w, r)
		return
	}
	d := importData{Text: r.PostForm.Get("text")}
	specs, err := parseImport(strings.TrimSpace(d.Text))
	if err != nil {
		d.Errors = []string{err.Error()}
		u.importPage(w, http.StatusUnprocessableEntity, d)
		return
	}
	d.Entries = entriesFor(specs)
	problems, err := u.check("", specs...)
	switch {
	case err != nil:
		d.Errors = []string{err.Error()}
		u.importPage(w, http.StatusInternalServerError, d)
	case len(problems) > 0:
		d.Errors = problems
		u.importPage(w, http.StatusUnprocessableEntity, d)
	default:
		d.Confirmable = true
		u.importPage(w, http.StatusOK, d)
	}
}

func (u *ui) importPage(w http.ResponseWriter, status int, d importData) {
	u.page(w, status, "import.html", "Import", "import", d)
}

// postedEntries re-parses the hidden entries[i].* fields of a confirmation.
func postedEntries(form url.Values) ([]config.ServerSpec, []string) {
	byIndex := map[int]url.Values{}
	for k, vals := range form {
		rest, ok := strings.CutPrefix(k, "entries[")
		if !ok {
			continue
		}
		num, field, ok := strings.Cut(rest, "].")
		i, err := strconv.Atoi(num)
		if !ok || err != nil || i < 0 {
			continue
		}
		if byIndex[i] == nil {
			byIndex[i] = url.Values{}
		}
		byIndex[i][field] = vals
	}
	specs := make([]config.ServerSpec, 0, len(byIndex))
	var problems []string
	for _, i := range slices.Sorted(maps.Keys(byIndex)) {
		s, err := specFromForm(byIndex[i])
		for _, m := range messages(err) {
			problems = append(problems, fmt.Sprintf("entry %d: %s", i+1, m))
		}
		specs = append(specs, s)
	}
	return specs, problems
}

func (u *ui) importConfirm(w http.ResponseWriter, r *http.Request) {
	specs, problems := postedEntries(r.PostForm)
	d := importData{Entries: entriesFor(specs)}
	if len(specs) == 0 {
		d.Errors = []string{"nothing to import"}
		u.importPage(w, http.StatusUnprocessableEntity, d)
		return
	}
	if len(problems) == 0 {
		var err error
		if problems, err = u.check("", specs...); err != nil {
			d.Errors = []string{err.Error()}
			u.importPage(w, http.StatusInternalServerError, d)
			return
		}
	}
	if len(problems) > 0 {
		d.Errors = problems
		u.importPage(w, http.StatusUnprocessableEntity, d)
		return
	}

	// The batch is valid as a whole, so only the writer can fail now. Keep
	// the entries that did not make it so the confirmation can be retried
	// without adding the others twice.
	var failed []config.ServerSpec
	var errs []string
	for _, s := range specs {
		if err := u.Writer.AddServer(s); err != nil {
			u.Logger.Warn("import server", "server", s.Name, "err", err)
			errs = append(errs, fmt.Sprintf("%s: %v", s.Name, err))
			failed = append(failed, s)
		}
	}
	if len(failed) < len(specs) {
		u.reloadAfterWrite()
	}
	if len(failed) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if added := len(specs) - len(failed); added > 0 {
		errs = append([]string{fmt.Sprintf("%d of %d servers were added", added, len(specs))}, errs...)
	}
	d.Entries = entriesFor(failed)
	d.Errors = errs
	d.Confirmable = true
	u.importPage(w, http.StatusUnprocessableEntity, d)
}
