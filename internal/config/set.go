package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Layer identifies which configuration layer a file belongs to.
type Layer int

// The configuration layers, in load order.
const (
	LayerBase   Layer = iota // the admin base file
	LayerSystem              // an admin system drop-in
	LayerUser                // a UI-owned user drop-in
)

// String returns "base", "system" or "user".
func (l Layer) String() string {
	switch l {
	case LayerBase:
		return "base"
	case LayerSystem:
		return "system"
	case LayerUser:
		return "user"
	default:
		return fmt.Sprintf("Layer(%d)", int(l))
	}
}

// OriginFile is one file that defines a server.
type OriginFile struct {
	Path  string
	Layer Layer
}

// Origin lists the files that define a server, in the order they were
// applied. The last LayerUser entry, if any, is the effective user definition.
type Origin struct{ Files []OriginFile }

// Admin reports whether any admin file (base or system) defines the server.
func (o Origin) Admin() bool {
	for _, f := range o.Files {
		if f.Layer != LayerUser {
			return true
		}
	}
	return false
}

// UserFile returns the path of the effective user definition, or "" when no
// user file defines the server.
func (o Origin) UserFile() string {
	for i := len(o.Files) - 1; i >= 0; i-- {
		if o.Files[i].Layer == LayerUser {
			return o.Files[i].Path
		}
	}
	return ""
}

// String describes where the server is defined, for example
// "Defined in /etc/mcp-machine/config.yaml (base); overridden by
// /data/config.d/fs.yaml (user)".
func (o Origin) String() string {
	var admin []string
	for _, f := range o.Files {
		if f.Layer != LayerUser {
			admin = append(admin, f.Path+" ("+f.Layer.String()+")")
		}
	}
	user := o.UserFile()
	switch {
	case len(admin) > 0 && user != "":
		return "Defined in " + strings.Join(admin, ", ") + "; overridden by " + user + " (user)"
	case len(admin) > 0:
		return "Defined in " + strings.Join(admin, ", ")
	case user != "":
		return "Defined in " + user + " (user)"
	default:
		return ""
	}
}

// Set is the result of loading every layer of a Layout.
type Set struct {
	Config   *Config           // merged, defaulted, expanded
	Files    []string          // every file loaded, in load order (empty when nothing was found)
	Layers   []Layer           // Layers[i] is the layer of Files[i]
	Origins  map[string]Origin // per server name
	Fallback string            // Layout.Fallback
	Searched []string          // Layout.Searched
}

// AdminFiles returns the administrator's files that were loaded (the base
// file, or the legacy file standing in for it, and the system drop-ins), in
// load order: the entries of Files outside the user layer. A file counts
// even when it defines no servers.
func (s *Set) AdminFiles() []string {
	var out []string
	for i, f := range s.Files {
		if s.Layers[i] != LayerUser {
			out = append(out, f)
		}
	}
	return out
}

// Load reads every layer of l and merges them into one Config, expanding
// ${VAR} references from the environment. MCP_MACHINE_PODMAN_SOCKET, when
// set, overrides podman.socket.
func (l Layout) Load() (*Set, error) {
	set, err := l.load(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	if v, ok := os.LookupEnv(podmanSocketEnv); ok && v != "" {
		set.Config.Podman.Socket = v
	}
	return set, nil
}

// LoadRaw is Load without environment expansion: every ${NAME} reference is
// kept as written and the socket override is not applied. It suits showing
// or editing the configuration as the files hold it.
func (l Layout) LoadRaw() (*Set, error) {
	return l.load(func(name string) (string, bool) { return "${" + name + "}", true })
}

// layerFile is one configuration file read by the loader.
type layerFile struct {
	path    string
	layer   Layer
	servers []ServerSpec
}

// load reads the base file, then the system and user drop-ins, and merges
// their servers. Admin definitions of a name refine each other field by
// field; a user definition replaces the admin result wholesale. Within the
// user layer the definition in <name>.yaml is applied last, so it wins.
// Defaults and lookup-based expansion are applied once, to the merged result.
func (l Layout) load(lookup func(string) (string, bool)) (*Set, error) {
	cfg := &Config{}
	var files []layerFile

	if l.BaseFile != "" {
		data, ok, err := readConfigFile(l.BaseFile)
		if err != nil {
			return nil, err
		}
		if ok {
			c, err := decode(data)
			if err != nil {
				return nil, fileError(l.BaseFile, err)
			}
			cfg = c
			files = append(files, layerFile{l.BaseFile, LayerBase, c.Servers})
		}
	}
	for _, d := range []struct {
		dir   string
		layer Layer
	}{{l.SystemDir, LayerSystem}, {l.UserDir, LayerUser}} {
		if d.dir == "" {
			continue
		}
		paths, err := listDropIns(d.dir)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if d.layer == LayerUser && p == l.skip {
				continue
			}
			data, ok, err := readConfigFile(p)
			if err != nil {
				return nil, err
			}
			if !ok { // removed since it was listed
				continue
			}
			specs, err := decodeDropIn(data)
			if err != nil {
				return nil, fileError(p, err)
			}
			files = append(files, layerFile{p, d.layer, specs})
		}
	}

	servers, origins, err := mergeLayers(files)
	if err != nil {
		return nil, err
	}
	cfg.Servers = servers
	where := func(server string) string {
		return strings.TrimPrefix(origins[server].String(), "Defined in ")
	}
	if err := finish(cfg, lookup, where); err != nil {
		return nil, err
	}

	set := &Set{
		Config:   cfg,
		Origins:  origins,
		Fallback: l.Fallback,
		Searched: l.Searched,
	}
	for _, f := range files {
		set.Files = append(set.Files, f.path)
		set.Layers = append(set.Layers, f.layer)
	}
	return set, nil
}

// mergeLayers merges the servers of files (in load order) into one list and
// records where each name is defined. The list keeps the order in which
// names first appear across files. Servers without a name are kept as they
// are, each on its own, so Validate can report them.
func mergeLayers(files []layerFile) ([]ServerSpec, map[string]Origin, error) {
	var out []ServerSpec
	index := map[string]int{}
	for _, f := range files {
		seen := map[string]bool{}
		for _, s := range f.servers {
			if s.Name == "" {
				out = append(out, cloneServer(s))
				continue
			}
			if seen[s.Name] {
				return nil, nil, fileError(f.path, fmt.Errorf("duplicate server %q", s.Name))
			}
			seen[s.Name] = true
			if _, ok := index[s.Name]; !ok {
				index[s.Name] = len(out)
				out = append(out, ServerSpec{})
			}
		}
	}

	defined := map[string][]OriginFile{}
	apply := func(f layerFile, s ServerSpec) error {
		i, prev := index[s.Name], defined[s.Name]
		if len(prev) == 0 || f.layer == LayerUser {
			out[i] = cloneServer(s)
		} else {
			m, err := mergeServer(out[i], s)
			if err != nil {
				paths := make([]string, len(prev))
				for j, p := range prev {
					paths[j] = p.Path
				}
				return fmt.Errorf("config: %s: %w (defined in %s)", f.path, err, strings.Join(paths, ", "))
			}
			out[i] = m
		}
		defined[s.Name] = append(prev, OriginFile{Path: f.path, Layer: f.layer})
		return nil
	}

	// Admin files first, then user definitions outside their canonical
	// <name>.yaml, then the canonical ones, so <name>.yaml wins.
	type def struct {
		f layerFile
		s ServerSpec
	}
	var canonical []def
	for _, f := range files {
		for _, s := range f.servers {
			if s.Name == "" {
				continue
			}
			if f.layer == LayerUser && filepath.Base(f.path) == s.Name+".yaml" {
				canonical = append(canonical, def{f, s})
				continue
			}
			if err := apply(f, s); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, d := range canonical {
		if err := apply(d.f, d.s); err != nil {
			return nil, nil, err
		}
	}

	origins := make(map[string]Origin, len(defined))
	for name, fs := range defined {
		origins[name] = Origin{Files: fs}
	}
	return out, origins, nil
}

// readConfigFile reads one configuration file. ok is false when the file
// does not exist. An empty or whitespace-only file is an error.
func readConfigFile(path string) (data []byte, ok bool, err error) {
	data, err = os.ReadFile(path) //nolint:gosec // G304: paths come from the operator-chosen layout
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fileError(path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, fileError(path, errEmptyFile)
	}
	return data, true, nil
}

// fileError reports err as a problem with the config file at path:
// "config: <path>: <err>".
func fileError(path string, err error) error {
	return fmt.Errorf("config: %s: %w", path, err)
}
