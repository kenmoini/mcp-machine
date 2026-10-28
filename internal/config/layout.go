package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LayoutOptions describes where configuration layers may be found.
type LayoutOptions struct {
	Path       string // -c: the base file, or the admin root directory
	SystemDir  string // --system-config-d; "" means <root>/config.d in root mode, none in file mode
	UserDir    string // --config-d; "" means no user layer
	LegacyFile string // upgrade fallback base, e.g. /data/config.yaml; "" disables the fallback
}

// Layout is the resolved set of configuration locations.
type Layout struct {
	BaseFile  string   // "" when there is no base file
	SystemDir string   // "" when there is no system drop-in directory
	UserDir   string   // "" when there is no user drop-in directory
	Fallback  string   // == BaseFile when the legacy file is in use, else ""
	Searched  []string // every path consulted, for the "no configuration found" warning

	skip string // a user drop-in the loader passes over, to see the set without it; "" skips nothing
}

// ResolveLayout turns LayoutOptions into a Layout. Path is the admin root
// directory when it is an existing directory, or when it does not exist and
// has no .yaml/.yml extension; otherwise it is the base file itself. In root
// mode, when neither the base file nor any system drop-in exists, the legacy
// file (if present) becomes the base file so an upgrade keeps working.
func ResolveLayout(o LayoutOptions) (Layout, error) {
	var l Layout
	rootMode, err := isRootPath(o.Path)
	if err != nil {
		return Layout{}, err
	}
	if rootMode {
		l.BaseFile = filepath.Join(o.Path, "config.yaml")
		l.SystemDir = o.SystemDir
		if l.SystemDir == "" {
			l.SystemDir = filepath.Join(o.Path, "config.d")
		}
	} else {
		l.BaseFile = o.Path
		l.SystemDir = o.SystemDir
	}
	l.UserDir = o.UserDir

	l.Searched = append(l.Searched, l.BaseFile)
	if l.SystemDir != "" {
		l.Searched = append(l.Searched, l.SystemDir)
	}
	if rootMode && o.LegacyFile != "" {
		consulted, fallback, err := legacyFallback(l, o.LegacyFile)
		if err != nil {
			return Layout{}, err
		}
		if consulted {
			l.Searched = append(l.Searched, o.LegacyFile)
		}
		if fallback {
			l.BaseFile = o.LegacyFile
			l.Fallback = o.LegacyFile
		}
	}
	if l.UserDir != "" {
		l.Searched = append(l.Searched, l.UserDir)
	}
	return l, nil
}

// isRootPath reports whether path names the admin root directory rather than
// a base file.
func isRootPath(path string) (bool, error) {
	fi, err := os.Stat(path)
	switch {
	case err == nil:
		return fi.IsDir(), nil
	case errors.Is(err, os.ErrNotExist):
		ext := filepath.Ext(path)
		return ext != ".yaml" && ext != ".yml", nil
	default:
		return false, fmt.Errorf("config: stat %s: %w", path, err)
	}
}

// legacyFallback decides about the upgrade fallback. consulted is true when
// the legacy file was considered at all (the admin base file is absent and
// there are no system drop-ins); use is true when it also exists and so
// stands in as the base.
func legacyFallback(l Layout, legacy string) (consulted, use bool, err error) {
	if _, err := os.Stat(l.BaseFile); err == nil {
		return false, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, false, fmt.Errorf("config: stat %s: %w", l.BaseFile, err)
	}
	dropIns, err := listDropIns(l.SystemDir)
	if err != nil {
		return false, false, err
	}
	if len(dropIns) > 0 {
		return false, false, nil
	}
	if _, err := os.Stat(legacy); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, false, nil
		}
		return true, false, fmt.Errorf("config: stat %s: %w", legacy, err)
	}
	return true, true, nil
}

// listDropIns returns the full paths of the drop-in files in dir, sorted
// bytewise: names ending in .yaml or .yml that do not start with ".", and
// that resolve (following symlinks) to a regular file. A missing dir yields
// nil, nil.
func listDropIns(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read dir %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		full := filepath.Join(dir, name)
		fi, err := os.Stat(full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) { // dangling symlink
				continue
			}
			return nil, fmt.Errorf("config: stat %s: %w", full, err)
		}
		if fi.Mode().IsRegular() {
			out = append(out, full)
		}
	}
	return out, nil
}
