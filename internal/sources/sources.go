// Package sources turns a configured server entry into a Resolved description
// of how to reach it: either a remote endpoint or a container to run.
package sources

import (
	"context"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
)

// Kind says how a server is reached.
type Kind int

const (
	// KindRemote is a server reached over HTTP or SSE.
	KindRemote Kind = iota + 1
	// KindContainer is a server run as a container.
	KindContainer
)

// RemoteEndpoint is a remote MCP server.
type RemoteEndpoint struct {
	URL, Transport string
	Headers        map[string]string
}

// HTTPEndpoint describes where a containerised server serves MCP over HTTP.
type HTTPEndpoint struct {
	Port            int
	Path, Transport string
}

// BuildPlan describes an image to build from a git repository.
type BuildPlan struct {
	Name, Repo, Ref, Subdir, Containerfile string
	Args                                   map[string]string
	Auth                                   *config.GitAuth
	RegistryAuth                           *runtime.RegistryAuth
}

// ImageSource is the image a container runs: pulled by Ref, or built by Build.
type ImageSource struct {
	Ref   string
	Build *BuildPlan
	Auth  *runtime.RegistryAuth
}

// Resolved is the outcome of resolving a server spec.
type Resolved struct {
	Kind      Kind
	Remote    *RemoteEndpoint
	Container *runtime.ContainerSpec
	Image     ImageSource
	Transport string
	HTTP      *HTTPEndpoint
}

// Resolver resolves a server spec into a Resolved.
type Resolver interface {
	Resolve(ctx context.Context, cfg *config.Config, spec config.ServerSpec) (Resolved, error)
}
