package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// ErrAdminOwned reports a delete of a server that only the administrator's
// files define.
var ErrAdminOwned = errors.New("config: server is defined by the administrator and cannot be deleted")

// ErrForeignFile reports an edit of a server whose user definition lives in
// a file the UI does not manage: one other than <name>.yaml, or a
// <name>.yaml that defines more than that one server.
var ErrForeignFile = errors.New("config: server is defined in a file the UI does not manage")

// editMu serializes every DirWriter edit in the process: an edit checks the
// loaded configuration and then writes, and two interleaved edits could both
// pass their checks.
var editMu sync.Mutex

// DirWriter makes the UI's edits as files in the user drop-in directory, one
// server per <name>.yaml. It never writes the admin base file or the system
// drop-ins: editing an admin server writes a user override, and deleting
// that override reverts to the admin definition. Each edit is validated
// against the merged configuration before it touches disk and is written
// atomically. Edits are serialized process-wide, whichever DirWriter value
// makes them.
type DirWriter struct {
	Dir    string // the user drop-in directory; files are written as <Dir>/<name>.yaml
	Layout Layout // the full layout, for validating the merged set before a write
}

// AddServer writes s to <Dir>/<s.Name>.yaml. It is an error if a server
// with that name is already defined anywhere, or if the file already exists.
func (w DirWriter) AddServer(s ServerSpec) error {
	editMu.Lock()
	defer editMu.Unlock()

	set, err := w.Layout.LoadRaw()
	if err != nil {
		return err
	}
	if _, ok := set.Origins[s.Name]; ok {
		return fmt.Errorf("config: server %q already exists", s.Name)
	}
	p := w.path(s.Name)
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("config: %s already exists", p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: stat %s: %w", p, err)
	}
	return w.write(set, s)
}

// UpdateServer replaces the definition of name with s by writing
// <Dir>/<name>.yaml, which creates a user override of an admin server or
// replaces the server's own file. The name cannot change. It is an error if
// no such server exists, or if <name>.yaml exists but is not the UI's.
func (w DirWriter) UpdateServer(name string, s ServerSpec) error {
	editMu.Lock()
	defer editMu.Unlock()

	set, err := w.Layout.LoadRaw()
	if err != nil {
		return err
	}
	if s.Name != name {
		return errors.New("config: server name cannot change")
	}
	if _, ok := set.Origins[name]; !ok {
		return notFound(name)
	}
	exists, exclusive, err := w.ownFile(name)
	if err != nil {
		return err
	}
	if exists && !exclusive {
		return fmt.Errorf("%w: %s", ErrForeignFile, w.path(name))
	}
	return w.write(set, s)
}

// DeleteServer removes <Dir>/<name>.yaml. For a server the administrator
// also defines, that reverts it to the admin definition. It is an error if
// no such server exists, if only admin files define it (ErrAdminOwned), or
// if its user definition is not in a file the UI manages (ErrForeignFile).
func (w DirWriter) DeleteServer(name string) error {
	editMu.Lock()
	defer editMu.Unlock()

	set, err := w.Layout.LoadRaw()
	if err != nil {
		return err
	}
	o, ok := set.Origins[name]
	if !ok {
		return notFound(name)
	}
	user := o.UserFile()
	if user == "" {
		if o.Admin() {
			return ErrAdminOwned
		}
		return notFound(name)
	}
	p := w.path(name)
	if user != p {
		return fmt.Errorf("%w: %s", ErrForeignFile, user)
	}
	_, exclusive, err := w.ownFile(name)
	if err != nil {
		return err
	}
	if !exclusive {
		return fmt.Errorf("%w: %s", ErrForeignFile, user)
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("config: remove %s: %w", p, err)
	}
	return nil
}

// path is the file DirWriter manages for the server called name.
func (w DirWriter) path(name string) string {
	return filepath.Join(w.Dir, name+".yaml")
}

// ownFile reads path(name), if it exists. exclusive is true when the file
// defines exactly one server, named name; a file that cannot be decoded is
// not exclusive.
func (w DirWriter) ownFile(name string) (exists, exclusive bool, err error) {
	p := w.path(name)
	data, err := os.ReadFile(p) //nolint:gosec // G304: p is <user drop-in dir>/<name>.yaml
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("config: read %s: %w", p, err)
	}
	specs, derr := decodeDropIn(data)
	return true, derr == nil && len(specs) == 1 && specs[0].Name == name, nil
}

// write validates the configuration set would hold with s in place of (or
// added to) its servers, then atomically writes s alone to path(s.Name).
// Nothing is written when validation fails.
func (w DirWriter) write(set *Set, s ServerSpec) error {
	c := *set.Config
	c.Servers = make([]ServerSpec, 0, len(set.Config.Servers)+1)
	replaced := false
	for _, cur := range set.Config.Servers {
		if cur.Name == s.Name {
			cur, replaced = s, true
		}
		c.Servers = append(c.Servers, cloneServer(cur))
	}
	if !replaced {
		c.Servers = append(c.Servers, cloneServer(s))
	}
	ApplyDefaults(&c)
	if err := Validate(&c); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	doc := struct {
		Servers []ServerSpec `yaml:"servers"`
	}{[]ServerSpec{s}}
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("config: encode server %q: %w", s.Name, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("config: encode server %q: %w", s.Name, err)
	}

	if err := os.MkdirAll(w.Dir, 0o750); err != nil {
		return fmt.Errorf("config: create %s: %w", w.Dir, err)
	}
	return writeAtomic(w.path(s.Name), buf.Bytes(), 0o640)
}

func notFound(name string) error { return fmt.Errorf("config: server %q not found", name) }

// writeAtomic writes data to a fresh temp file next to path with the given
// mode, renames it over path and syncs the directory so the rename survives
// a crash. The temp file is named ".<base>-*.tmp" so the watcher and the
// loader, which skip hidden names, ignore it. It is removed on failure.
func writeAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("config: write temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err = f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("config: chmod %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("config: sync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("config: close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: rename %s: %w", tmp, err)
	}
	// The file is in place; failing to sync the directory only weakens
	// durability, so it is not reported as a failed write.
	if d, derr := os.Open(dir); derr == nil { //nolint:gosec // G304: dir is the operator-chosen user drop-in directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
