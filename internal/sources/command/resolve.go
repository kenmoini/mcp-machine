// Package command resolves "command" server specs (npx/uvx/python or a
// docker run line) into a container to run.
package command

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/sources/dockerrun"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// lookupEnv resolves pass-through environment variables and ${VAR}
// references; tests replace it.
var lookupEnv = os.LookupEnv

// envRef matches a ${VAR} reference, as the config loader does.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expander replaces ${VAR} references with lookupEnv values and remembers
// the unset names, each once, in the order they are first seen.
type expander struct {
	missing []string
	seen    map[string]bool
}

func (x *expander) expand(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := lookupEnv(name); ok {
			return v
		}
		if !x.seen[name] {
			if x.seen == nil {
				x.seen = map[string]bool{}
			}
			x.seen[name] = true
			x.missing = append(x.missing, name)
		}
		return m
	})
}

func (x *expander) expandAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = x.expand(s)
	}
	return out
}

// err reports every unset variable, or nil.
func (x *expander) err() error {
	errs := make([]error, len(x.missing))
	for i, n := range x.missing {
		errs[i] = fmt.Errorf("environment variable %s is not set", n)
	}
	return errors.Join(errs...)
}

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
	// ${VAR} in the run line's -e values and arguments, and in command.args,
	// is expanded here: the config loader leaves those fields as written.
	var x expander

	if spec.Command.Run != "" {
		// ${vault:...} references are masked as plain ${NAME} placeholders
		// so the run-line parser carries them through, then restored in
		// the -e values, the only place they are allowed.
		masked, refs := config.MaskVaultRefs(spec.Command.Run)
		run, err := dockerrun.Parse(masked)
		if err != nil {
			return sources.Resolved{}, fmt.Errorf("parse run line: %w", err)
		}
		for k, v := range run.Env {
			run.Env[k] = config.UnmaskVaultRefs(v, refs)
		}
		if err := refuseVaultRefs(run, refs); err != nil {
			return sources.Resolved{}, err
		}
		image = run.Image
		cs.Command = x.expandAll(run.Args)
		cs.WorkDir = run.WorkDir
		for k, v := range run.Env {
			cs.Env[k] = x.expand(v)
		}
		for _, name := range run.PassEnv {
			// A bare -e NAME takes its value from command.env (already in
			// cs.Env) or the server env (applied below), else the process
			// environment.
			if _, ok := spec.Command.Env[name]; ok {
				continue
			}
			if _, ok := spec.Env[name]; ok {
				continue
			}
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
		cs.Mounts = append(cs.Mounts, run.Mounts...)
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
		for _, a := range spec.Command.Args {
			if config.HasVaultRef(a) {
				return sources.Resolved{}, errors.New("${vault:...} is not allowed in command.args")
			}
		}
		cs.Command = append([]string{name}, x.expandAll(spec.Command.Args)...)
		if m, ok := cacheMount(name); ok {
			cs.Mounts = append(cs.Mounts, m)
		}
	}
	if err := x.err(); err != nil {
		return sources.Resolved{}, err
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
	SplitSecretEnv(cs)
	cs.Image = image

	return sources.Resolved{
		Kind:      sources.KindContainer,
		Container: cs,
		Image:     sources.ImageSource{Ref: image, Auth: sources.RegistryAuthFor(cfg.Registries, image)},
		Transport: "stdio",
	}, nil
}

// SplitSecretEnv moves every Env value that holds a ${vault:...} reference
// into SecretEnv, so the engine never sees it; the manager resolves and
// delivers SecretEnv when the container starts. Empty maps become nil.
func SplitSecretEnv(cs *runtime.ContainerSpec) {
	for k, v := range cs.Env {
		if config.HasVaultRef(v) {
			if cs.SecretEnv == nil {
				cs.SecretEnv = map[string]string{}
			}
			cs.SecretEnv[k] = v
			delete(cs.Env, k)
		}
	}
	if len(cs.Env) == 0 {
		cs.Env = nil
	}
}

// refuseVaultRefs reports a masked ${vault:...} reference that landed
// anywhere in the parsed run line but its -e values, where it was restored
// already: in the image, its arguments, a volume, a port or the workdir.
func refuseVaultRefs(run dockerrun.Spec, refs []config.Ref) error {
	if len(refs) == 0 {
		return nil
	}
	masks := config.VaultMasks(refs)
	holds := func(s string) bool {
		for _, m := range masks {
			if strings.Contains(s, m) {
				return true
			}
		}
		return false
	}
	switch {
	case holds(run.Image):
		return errors.New("${vault:...} is not allowed in the run line's image")
	case holds(strings.Join(run.Args, " ")):
		return errors.New("${vault:...} is not allowed in the run line's arguments")
	case holds(strings.Join(run.Volumes, " ")), holds(strings.Join(run.Ports, " ")), holds(run.WorkDir), holds(strings.Join(run.PassEnv, " ")):
		return errors.New("${vault:...} is only allowed in the run line's -e values")
	}
	for _, m := range run.Mounts {
		if holds(m.Source) || holds(m.Target) {
			return errors.New("${vault:...} is only allowed in the run line's -e values")
		}
	}
	return nil
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

// parseVolume parses "src:dst[:opts]" into a mount. opts is a comma-separated
// list of at most one of ro/rw and at most one of z/Z (SELinux relabeling,
// shared or private). A src without a "/" names a volume; anything else is
// a host path bind-mounted.
func parseVolume(v string) (runtime.Mount, error) {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return runtime.Mount{}, fmt.Errorf("invalid volume %q; want src:dst[:ro|rw][,z|Z]", v)
	}
	m := runtime.Mount{Type: "bind", Source: parts[0], Target: parts[1]}
	if !strings.Contains(m.Source, "/") {
		m.Type = "volume"
	}
	if len(parts) == 3 {
		mode := false
		for opt := range strings.SplitSeq(parts[2], ",") {
			switch opt {
			case "ro", "rw":
				if mode {
					return runtime.Mount{}, fmt.Errorf("invalid volume %q: more than one of ro and rw", v)
				}
				mode = true
				m.ReadOnly = opt == "ro"
			case "z", "Z":
				if m.Relabel != "" {
					return runtime.Mount{}, fmt.Errorf("invalid volume %q: more than one of z and Z", v)
				}
				m.Relabel = opt
			default:
				return runtime.Mount{}, fmt.Errorf("invalid volume %q: unknown option %q", v, opt)
			}
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

// ParseVolume is parseVolume, exported so other resolvers apply the same
// volume syntax.
func ParseVolume(v string) (runtime.Mount, error) { return parseVolume(v) }
