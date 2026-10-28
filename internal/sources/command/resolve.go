// Package command resolves "command" server specs (npx/uvx/python or a
// docker run line) into a container to run.
package command

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/sources/dockerrun"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// lookupEnv resolves pass-through environment variables; tests replace it.
var lookupEnv = os.LookupEnv

// Resolve turns a command-type server spec into a container description.
func Resolve(cfg *config.Config, spec config.ServerSpec) (sources.Resolved, error) {
	if spec.Command == nil {
		return sources.Resolved{}, errors.New("command is required")
	}
	cs := &runtime.ContainerSpec{
		Name:    store.ContainerName(spec.Name),
		Stdin:   true,
		Network: cfg.Podman.Network,
		Env:     map[string]string{},
	}
	var image string
	for k, v := range spec.Command.Env {
		cs.Env[k] = v
	}

	if spec.Command.Run != "" {
		run, err := dockerrun.Parse(spec.Command.Run)
		if err != nil {
			return sources.Resolved{}, fmt.Errorf("parse run line: %w", err)
		}
		image = run.Image
		cs.Command = run.Args
		cs.WorkDir = run.WorkDir
		for k, v := range run.Env {
			cs.Env[k] = v
		}
		for _, name := range run.PassEnv {
			v, ok := lookupEnv(name)
			if !ok {
				return sources.Resolved{}, fmt.Errorf("environment variable %s is not set", name)
			}
			cs.Env[name] = v
		}
		for _, v := range run.Volumes {
			m, err := parseVolume(v)
			if err != nil {
				return sources.Resolved{}, err
			}
			cs.Mounts = append(cs.Mounts, m)
		}
		for _, p := range run.Ports {
			pm, err := parsePort(p)
			if err != nil {
				return sources.Resolved{}, err
			}
			cs.Ports = append(cs.Ports, pm)
		}
	} else {
		name := spec.Command.Command
		if name == "" {
			return sources.Resolved{}, errors.New("command.command or command.run is required")
		}
		if _, ok := cfg.Runtimes[name]; !ok {
			return sources.Resolved{}, fmt.Errorf("unknown command %q; add it to runtimes", name)
		}
		image = cfg.Runtimes[name]
		cs.Command = append([]string{name}, spec.Command.Args...)
		if m, ok := cacheMount(name); ok {
			cs.Mounts = append(cs.Mounts, m)
		}
	}

	for k, v := range spec.Env {
		cs.Env[k] = v
	}
	if o := spec.Container; o != nil {
		if o.Image != "" {
			image = o.Image
		}
		for k, v := range o.Env {
			cs.Env[k] = v
		}
		for _, v := range o.Volumes {
			m, err := parseVolume(v)
			if err != nil {
				return sources.Resolved{}, err
			}
			cs.Mounts = append(cs.Mounts, m)
		}
	}
	if len(cs.Env) == 0 {
		cs.Env = nil
	}
	cs.Image = image

	return sources.Resolved{
		Kind:      sources.KindContainer,
		Container: cs,
		Image:     sources.ImageSource{Ref: image, Auth: sources.RegistryAuthFor(cfg.Registries, image)},
		Transport: "stdio",
	}, nil
}

// cacheMount returns the package-cache volume for a runtime command.
func cacheMount(command string) (runtime.Mount, bool) {
	switch command {
	case "npx", "node":
		return runtime.Mount{Type: "volume", Source: "mcpm-cache-npm", Target: "/root/.npm"}, true
	case "uvx", "python":
		return runtime.Mount{Type: "volume", Source: "mcpm-cache-uv", Target: "/root/.cache/uv"}, true
	}
	return runtime.Mount{}, false
}

// parseVolume parses "src:dst[:ro]" into a bind mount.
func parseVolume(v string) (runtime.Mount, error) {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return runtime.Mount{}, fmt.Errorf("invalid volume %q; want src:dst[:ro]", v)
	}
	m := runtime.Mount{Type: "bind", Source: parts[0], Target: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
		default:
			return runtime.Mount{}, fmt.Errorf("invalid volume %q: unknown option %q", v, parts[2])
		}
	}
	return m, nil
}

// parsePort parses "[ip:]host:container[/proto]" into a port mapping.
func parsePort(p string) (runtime.PortMapping, error) {
	spec, proto, hasProto := strings.Cut(p, "/")
	if !hasProto {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return runtime.PortMapping{}, fmt.Errorf("invalid port %q: unknown protocol %q", p, proto)
	}
	parts := strings.Split(spec, ":")
	var ip string
	switch len(parts) {
	case 2:
	case 3:
		ip, parts = parts[0], parts[1:]
	default:
		return runtime.PortMapping{}, fmt.Errorf("invalid port %q; want host:container[/proto]", p)
	}
	host, err1 := strconv.Atoi(parts[0])
	cont, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || host < 1 || host > 65535 || cont < 1 || cont > 65535 {
		return runtime.PortMapping{}, fmt.Errorf("invalid port %q; want host:container[/proto]", p)
	}
	return runtime.PortMapping{HostIP: ip, HostPort: host, ContainerPort: cont, Protocol: proto}, nil
}

// ParseVolume parses "src:dst[:ro]" into a bind mount. It is exported so other
// resolvers apply the same volume syntax.
func ParseVolume(v string) (runtime.Mount, error) { return parseVolume(v) }
