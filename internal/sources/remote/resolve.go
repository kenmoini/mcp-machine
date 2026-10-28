// Package remote resolves "remote" server specs into an HTTP/SSE endpoint.
package remote

import (
	"errors"
	"fmt"
	"maps"
	"net/url"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

// Resolve turns a remote-type server spec into a remote endpoint.
func Resolve(_ *config.Config, spec config.ServerSpec) (sources.Resolved, error) {
	if spec.Remote == nil {
		return sources.Resolved{}, errors.New("remote is required")
	}
	u, err := url.Parse(spec.Remote.URL)
	if err != nil {
		return sources.Resolved{}, fmt.Errorf("invalid remote url %q: %w", spec.Remote.URL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return sources.Resolved{}, fmt.Errorf("invalid remote url %q: want an http or https URL with a host", spec.Remote.URL)
	}
	transport := spec.Remote.Transport
	if transport == "" {
		transport = "auto"
	}
	return sources.Resolved{
		Kind: sources.KindRemote,
		Remote: &sources.RemoteEndpoint{
			URL:       spec.Remote.URL,
			Transport: transport,
			Headers:   maps.Clone(spec.Remote.Headers),
		},
	}, nil
}
