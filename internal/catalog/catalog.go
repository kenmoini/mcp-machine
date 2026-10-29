// Package catalog reads the ready-made server drop-ins the Add server page
// offers as cards: one YAML per server, with a leading comment block that
// describes it. The directory is read on every call, so a bind-mounted
// catalog can change without a restart.
package catalog

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// Entry is one catalog server: the metadata from its comment header and the
// spec as written in the file (references kept verbatim).
type Entry struct {
	Name        string
	Type        string
	Category    string // from "# category: ..."; "" when absent
	Description string // the first comment line
	Upstream    string // the first comment line that is a bare URL
	Secret      string // the text after "# Secret:"
	File        string // the file's base name
	Spec        config.ServerSpec
}

// HasVaultRefs reports whether the entry references Vault secrets.
func (e Entry) HasVaultRefs() bool {
	refs, _ := config.CheckVaultRefs(e.Spec)
	return len(refs) > 0
}

var urlLine = regexp.MustCompile(`^https?://\S+$`)

// Load returns the entries of the *.yaml files in dir, sorted by name. A
// missing directory yields nil. A file that does not parse, holds anything
// but one server, or names a server differently from the file is skipped
// with a warning: the page never offers an entry the form could not take.
func Load(dir string, log *slog.Logger) []Entry {
	if dir == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil
	}
	var out []Entry
	for _, p := range paths {
		e, err := read(p)
		if err != nil {
			log.Warn("catalog: skipping entry", "file", p, "err", err)
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// read parses one catalog file.
func read(path string) (Entry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: files of the operator's catalog directory
	if err != nil {
		return Entry{}, err
	}
	e := header(data)
	e.File = filepath.Base(path)
	specs, err := config.ParseDropIn(data)
	if err != nil {
		return Entry{}, err
	}
	if len(specs) != 1 {
		return Entry{}, fmt.Errorf("holds %d servers, want exactly 1", len(specs))
	}
	e.Spec = specs[0]
	e.Name, e.Type = e.Spec.Name, e.Spec.Type
	if want := strings.TrimSuffix(e.File, ".yaml"); e.Name != want {
		return Entry{}, fmt.Errorf("names server %q, want %q (the file name)", e.Name, want)
	}
	if e.Type == "" {
		return Entry{}, fmt.Errorf("server %q has no type", e.Name)
	}
	return e, nil
}

// header reads the leading run of comment lines: the first is the
// description; a bare URL is the upstream link; "Secret:" and "category:"
// lines fill those fields.
func header(data []byte) Entry {
	var e Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		switch {
		case text == "":
		case strings.HasPrefix(strings.ToLower(text), "category:"):
			e.Category = strings.ToLower(strings.TrimSpace(text[len("category:"):]))
		case strings.HasPrefix(text, "Secret:"):
			if e.Secret == "" {
				e.Secret = strings.TrimSpace(text[len("Secret:"):])
			}
		case urlLine.MatchString(text):
			if e.Upstream == "" {
				e.Upstream = text
			}
		case e.Description == "":
			e.Description = text
		}
	}
	return e
}
