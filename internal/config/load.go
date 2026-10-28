package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// podmanSocketEnv overrides podman.socket when set.
const podmanSocketEnv = "MCP_MACHINE_PODMAN_SOCKET"

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

var defaultRuntimes = map[string]string{
	"npx":    "docker.io/library/node:22-bookworm-slim",
	"node":   "docker.io/library/node:22-bookworm-slim",
	"uvx":    "ghcr.io/astral-sh/uv:python3.12-bookworm-slim",
	"python": "docker.io/library/python:3.12-slim",
}

// Parse decodes YAML config data, applies defaults, and expands ${VAR}
// references using lookup. Unknown keys and unset variables are errors.
func Parse(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	c, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := finish(c, lookup); err != nil {
		return nil, err
	}
	return c, nil
}

// decode strictly decodes YAML config data. Unknown keys are errors and an
// empty document yields a zero Config. No defaults are applied and no
// variables are expanded. Errors carry no "config:" prefix; callers add
// their own context.
func decode(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	c := &Config{}
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return c, nil
}

// finish applies defaults to c and expands ${VAR} references using lookup.
func finish(c *Config, lookup func(string) (string, bool)) error {
	ApplyDefaults(c)
	return expandEnv(c, lookup)
}

// dropIn is the shape of a drop-in file: a servers list and nothing else.
type dropIn struct {
	Servers []ServerSpec `yaml:"servers"`
}

// decodeDropIn strictly decodes a drop-in file, which may hold only a
// servers list. A global key is an error naming the key. No defaults are
// applied and no variables are expanded. Errors carry no "config:" prefix.
func decodeDropIn(data []byte) ([]ServerSpec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var d dropIn
	if err := dec.Decode(&d); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return d.Servers, nil
}

// errEmptyFile reports a config file with no content. It carries no
// "config:" prefix; the loader reports it as "config: <file>: file is empty".
var errEmptyFile = errors.New("file is empty")

// Load reads and parses the single config file at path: the file-mode
// wrapper over Layout{BaseFile: path}.Load. Unlike a layered load, a missing
// file is an error, and an empty or whitespace-only file is the error
// "config: file is empty" so a half-written file is never mistaken for an
// all-defaults config. The file is read exactly once. The environment
// variable MCP_MACHINE_PODMAN_SOCKET, when set, overrides podman.socket.
func Load(path string) (*Config, error) {
	set, err := Layout{BaseFile: path}.Load()
	if err != nil {
		if errors.Is(err, errEmptyFile) {
			return nil, fmt.Errorf("config: %w", errEmptyFile)
		}
		return nil, err
	}
	if len(set.Files) == 0 {
		return nil, fmt.Errorf("config: %s: %w", path, os.ErrNotExist)
	}
	return set.Config, nil
}

// ApplyDefaults fills unset fields with their default values. Per-server
// defaults are applied only to sections that exist, except that command and
// build servers always get a serve section so serve.mode is populated.
func ApplyDefaults(c *Config) {
	setDefault(&c.Listen, ":8080")
	setDefault(&c.Podman.Socket, "unix:///run/podman/podman.sock")
	setDefault(&c.Podman.Network, "mcp-machine")
	if c.Podman.StopTimeout == 0 {
		c.Podman.StopTimeout = 10 * time.Second
	}
	setDefault(&c.DataDir, "/data")
	setDefault(&c.WorkspaceDir, path.Join(c.DataDir, "workspace"))
	setDefault(&c.Log.Level, "info")
	setDefault(&c.Log.Format, "text")

	if c.Runtimes == nil {
		c.Runtimes = make(map[string]string, len(defaultRuntimes))
	}
	for k, v := range defaultRuntimes {
		if _, ok := c.Runtimes[k]; !ok {
			c.Runtimes[k] = v
		}
	}

	for i := range c.Servers {
		s := &c.Servers[i]
		if s.Remote != nil {
			setDefault(&s.Remote.Transport, "auto")
		}
		if s.Build != nil {
			setDefault(&s.Build.Containerfile, "Containerfile")
		}
		if s.Serve == nil && (s.Command != nil || s.Build != nil) {
			s.Serve = &ServeSpec{}
		}
		if s.Serve != nil {
			setDefault(&s.Serve.Mode, "stdio")
		}
	}
}

func setDefault(field *string, def string) {
	if *field == "" {
		*field = def
	}
}

// expandEnv replaces ${VAR} in secret-bearing string fields. command.run
// and command.args are only checked: they stay as written and are expanded
// when the container is resolved, but their variables must be set too. All
// unset variables are collected and reported in a single joined error.
func expandEnv(c *Config, lookup func(string) (string, bool)) error {
	missing := map[string]bool{}
	expand := func(s string) string {
		return envRef.ReplaceAllStringFunc(s, func(m string) string {
			name := m[2 : len(m)-1]
			v, ok := lookup(name)
			if !ok {
				missing[name] = true
				return m
			}
			return v
		})
	}
	check := checker(lookup, missing)

	for i := range c.Registries {
		r := &c.Registries[i]
		r.Username = expand(r.Username)
		r.Password = expand(r.Password)
	}
	for i := range c.Servers {
		serverVars(&c.Servers[i], expand, check)
	}

	if len(missing) == 0 {
		return nil
	}
	names := sortedNames(missing)
	errs := make([]error, len(names))
	for i, n := range names {
		errs[i] = fmt.Errorf("config: ${%s} is not set", n)
	}
	return errors.Join(errs...)
}

// UnsetVars returns, sorted, the names of the ${VAR} references in s that
// the loader would resolve (or, for command.run and command.args, require)
// and lookup does not know. s is not modified.
func UnsetVars(s ServerSpec, lookup func(string) (string, bool)) []string {
	missing := map[string]bool{}
	serverVars(&s, nil, checker(lookup, missing))
	return sortedNames(missing)
}

// checker returns a function recording in missing every ${VAR} in its
// argument that lookup does not know.
func checker(lookup func(string) (string, bool), missing map[string]bool) func(string) {
	return func(s string) {
		for _, m := range envRef.FindAllStringSubmatch(s, -1) {
			if _, ok := lookup(m[1]); !ok {
				missing[m[1]] = true
			}
		}
	}
}

// serverVars visits the fields of s that may hold ${VAR} references. The
// expanded fields are replaced by expand(value); when expand is nil they are
// passed to check instead and s is left untouched. command.run and
// command.args are always only checked.
func serverVars(s *ServerSpec, expand func(string) string, check func(string)) {
	str := func(p *string) {
		if expand == nil {
			check(*p)
			return
		}
		*p = expand(*p)
	}
	strMap := func(m map[string]string) {
		for k, v := range m {
			if expand == nil {
				check(v)
				continue
			}
			m[k] = expand(v)
		}
	}

	strMap(s.Env)
	if s.Remote != nil {
		strMap(s.Remote.Headers)
	}
	if s.Command != nil {
		strMap(s.Command.Env)
		check(s.Command.Run)
		for _, a := range s.Command.Args {
			check(a)
		}
	}
	if s.Container != nil {
		strMap(s.Container.Env)
	}
	if s.Build != nil {
		strMap(s.Build.Args)
		if s.Build.Auth != nil {
			str(&s.Build.Auth.Token)
		}
	}
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
