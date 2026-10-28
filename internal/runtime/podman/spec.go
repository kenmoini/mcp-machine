package podman

import (
	"fmt"
	"sort"

	spec "github.com/opencontainers/runtime-spec/specs-go"
	nettypes "go.podman.io/common/libnetwork/types"
	"go.podman.io/podman/v6/pkg/specgen"

	"github.com/kenmoini/mcp-machine/internal/runtime"
)

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// toSpecGen translates a runtime.ContainerSpec into a libpod SpecGenerator.
// Terminal is always false: MCP stdio framing must not pass through a TTY.
func toSpecGen(s runtime.ContainerSpec) *specgen.SpecGenerator {
	g := specgen.NewSpecGenerator(s.Image, false)
	g.Name = s.Name
	g.Command = s.Command
	g.Entrypoint = s.Entrypoint
	g.Env = s.Env
	g.Labels = s.Labels
	g.Stdin = ptr(s.Stdin)
	g.Terminal = ptr(false)
	g.WorkDir = s.WorkDir
	g.RestartPolicy = s.RestartPolicy

	if s.Network != "" {
		g.Networks = map[string]nettypes.PerNetworkOptions{s.Network: {}}
	}

	for _, p := range s.Ports {
		g.PortMappings = append(g.PortMappings, nettypes.PortMapping{
			HostIP:        p.HostIP,
			HostPort:      uint16(p.HostPort),
			ContainerPort: uint16(p.ContainerPort),
			Protocol:      p.Protocol,
		})
	}

	for _, m := range s.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		opts := []string{mode}
		if m.Relabel != "" {
			opts = append(opts, m.Relabel)
		}
		switch m.Type {
		case "volume":
			g.Volumes = append(g.Volumes, &specgen.NamedVolume{Name: m.Source, Dest: m.Target, Options: opts})
		default:
			g.Mounts = append(g.Mounts, spec.Mount{
				Type:        "bind",
				Source:      m.Source,
				Destination: m.Target,
				Options:     append([]string{"rbind"}, opts...),
			})
		}
	}
	return g
}

// labelFilter builds a libpod list filter that matches containers carrying
// every given label. Entries are sorted for determinism.
func labelFilter(labels map[string]string) map[string][]string {
	if len(labels) == 0 {
		return map[string][]string{}
	}
	vals := make([]string, 0, len(labels))
	for k, v := range labels {
		vals = append(vals, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(vals)
	return map[string][]string{"label": vals}
}
