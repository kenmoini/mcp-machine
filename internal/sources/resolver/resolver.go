// Package resolver provides the composite sources.Resolver that dispatches a
// server spec to the resolver for its type. It lives outside package sources
// because the per-type resolvers import sources for the shared types.
package resolver

import (
	"context"
	"fmt"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/sources/build"
	"github.com/kenmoini/mcp-machine/internal/sources/command"
	"github.com/kenmoini/mcp-machine/internal/sources/remote"
)

type composite struct{}

// New returns a Resolver that dispatches on spec.Type to the remote, command
// or build resolver. Every error is wrapped as `server "<name>": <err>`.
func New() sources.Resolver { return composite{} }

// Resolve implements sources.Resolver.
func (composite) Resolve(_ context.Context, cfg *config.Config, spec config.ServerSpec) (sources.Resolved, error) {
	var (
		res sources.Resolved
		err error
	)
	switch spec.Type {
	case "remote":
		res, err = remote.Resolve(cfg, spec)
	case "command":
		res, err = command.Resolve(cfg, spec)
	case "build":
		res, err = build.Resolve(cfg, spec)
	default:
		err = fmt.Errorf("unknown server type %q", spec.Type)
	}
	if err != nil {
		return sources.Resolved{}, fmt.Errorf("server %q: %w", spec.Name, err)
	}
	return res, nil
}
