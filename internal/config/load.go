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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	c := &Config{}
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	ApplyDefaults(c)
	if err := expandEnv(c, lookup); err != nil {
		return nil, err
	}
	return c, nil
}

// errEmptyFile reports a config file with no content.
var errEmptyFile = errors.New("config: file is empty")

// Load reads and parses the config file at path. An empty or whitespace-only
// file is an error ("config: file is empty") so a half-written file is never
// mistaken for an all-defaults config. The file is read exactly once. The
// environment variable MCP_MACHINE_PODMAN_SOCKET, when set, overrides
// podman.socket.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errEmptyFile
	}
	c, err := Parse(data, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	if v, ok := os.LookupEnv(podmanSocketEnv); ok && v != "" {
		c.Podman.Socket = v
	}
	return c, nil
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

// expandEnv replaces ${VAR} in secret-bearing string fields. All unset
// variables are collected and reported in a single joined error.
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
	expandMap := func(m map[string]string) {
		for k, v := range m {
			m[k] = expand(v)
		}
	}

	for i := range c.Registries {
		r := &c.Registries[i]
		r.Username = expand(r.Username)
		r.Password = expand(r.Password)
	}
	for i := range c.Servers {
		s := &c.Servers[i]
		expandMap(s.Env)
		if s.Remote != nil {
			expandMap(s.Remote.Headers)
		}
		if s.Command != nil {
			expandMap(s.Command.Env)
		}
		if s.Container != nil {
			expandMap(s.Container.Env)
		}
		if s.Build != nil {
			expandMap(s.Build.Args)
			if s.Build.Auth != nil {
				s.Build.Auth.Token = expand(s.Build.Auth.Token)
			}
		}
	}

	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	errs := make([]error, len(names))
	for i, n := range names {
		errs[i] = fmt.Errorf("config: ${%s} is not set", n)
	}
	return errors.Join(errs...)
}
