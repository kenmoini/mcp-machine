package manager

import (
	"fmt"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

// DefaultEndpoint returns the EndpointFunc used when Options.EndpointFunc is
// nil. When MCP Machine runs in a container (opts.InContainer) it shares a
// network with the server and reaches it by container name:
// http://<name>:<port><path>. Otherwise it uses the port published on the
// host loopback: http://127.0.0.1:<host port><path>.
func DefaultEndpoint(opts Options) EndpointFunc {
	inContainer := opts.InContainer
	return func(info runtime.ContainerInfo, ep sources.HTTPEndpoint) (string, error) {
		path := ep.Path
		if path != "" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		if inContainer {
			return fmt.Sprintf("http://%s:%d%s", strings.TrimPrefix(info.Name, "/"), ep.Port, path), nil
		}
		for _, p := range info.Ports {
			if p.ContainerPort == ep.Port && p.HostPort != 0 {
				return fmt.Sprintf("http://127.0.0.1:%d%s", p.HostPort, path), nil
			}
		}
		return "", fmt.Errorf("no published port for %d", ep.Port)
	}
}
