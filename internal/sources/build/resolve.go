package build

import (
	"errors"
	"fmt"
	"maps"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
	"github.com/kenmoini/mcp-machine/internal/sources/command"
	"github.com/kenmoini/mcp-machine/internal/store"
)

// Resolve turns a build-type server spec into a container description plus a
// build plan. Container.Image is left empty: the manager fills it with the
// tag returned by Builder.Build.
func Resolve(cfg *config.Config, spec config.ServerSpec) (sources.Resolved, error) {
	if spec.Build == nil {
		return sources.Resolved{}, errors.New("build is required")
	}
	if spec.Build.Repo == "" {
		return sources.Resolved{}, errors.New("build.repo is required")
	}
	containerfile := spec.Build.Containerfile
	if containerfile == "" {
		containerfile = "Containerfile"
	}

	mode := "stdio"
	if spec.Serve != nil && spec.Serve.Mode != "" {
		mode = spec.Serve.Mode
	}
	if mode != "stdio" && mode != "http" {
		return sources.Resolved{}, fmt.Errorf("unknown serve mode %q; want stdio or http", mode)
	}

	cs := &runtime.ContainerSpec{
		Name:    store.ContainerName(spec.Name),
		Network: cfg.Podman.Network,
		Stdin:   mode == "stdio",
		Env:     maps.Clone(spec.Env),
	}
	res := sources.Resolved{
		Kind:      sources.KindContainer,
		Container: cs,
		Transport: mode,
	}
	if mode == "http" {
		port := spec.Serve.Port
		if port < 1 || port > 65535 {
			return sources.Resolved{}, fmt.Errorf("serve.port is required for http mode (got %d)", port)
		}
		path, transport := spec.Serve.Path, spec.Serve.Transport
		if path == "" {
			path = "/mcp"
		}
		if transport == "" {
			transport = "http"
		}
		res.HTTP = &sources.HTTPEndpoint{Port: port, Path: path, Transport: transport}
		cs.Ports = []runtime.PortMapping{{ContainerPort: port, Protocol: "tcp"}}
	}

	if o := spec.Container; o != nil {
		if len(o.Env) > 0 && cs.Env == nil {
			cs.Env = map[string]string{}
		}
		maps.Copy(cs.Env, o.Env)
		for _, v := range o.Volumes {
			m, err := command.ParseVolume(v)
			if err != nil {
				return sources.Resolved{}, err
			}
			cs.Mounts = append(cs.Mounts, m)
		}
	}
	if len(cs.Env) == 0 {
		cs.Env = nil
	}

	res.Image = sources.ImageSource{Build: &sources.BuildPlan{
		Name:          spec.Name,
		Repo:          spec.Build.Repo,
		Ref:           spec.Build.Ref,
		Subdir:        spec.Build.Subdir,
		Containerfile: containerfile,
		Args:          maps.Clone(spec.Build.Args),
		Auth:          spec.Build.Auth,
		// RegistryAuth stays nil: the registry hosting the Containerfile's base
		// image is only known once the Containerfile is read. Builder.Build
		// looks the credentials up (Builder.Registries) after cloning.
	}}
	return res, nil
}
