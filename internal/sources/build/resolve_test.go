package build

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/sources"
)

func resolveCfg() *config.Config {
	return &config.Config{Podman: config.PodmanConfig{Network: "mcpm-net"}}
}

func TestBuildResolveHTTPMode(t *testing.T) {
	spec := config.ServerSpec{
		Name: "svc",
		Type: "build",
		Build: &config.BuildSpec{
			Repo: "https://example.com/r.git", Ref: "v1", Subdir: "server",
			Args: map[string]string{"A": "1"}, Auth: &config.GitAuth{Token: "t"},
		},
		Serve:     &config.ServeSpec{Mode: "http", Port: 3000},
		Env:       map[string]string{"A": "spec", "B": "spec"},
		Container: &config.ContainerOverrides{Env: map[string]string{"B": "override"}, Volumes: []string{"/h:/c:ro"}},
	}
	res, err := Resolve(resolveCfg(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != sources.KindContainer || res.Transport != "http" {
		t.Fatalf("kind/transport = %v/%q", res.Kind, res.Transport)
	}
	want := &sources.BuildPlan{
		Name: "svc", Repo: "https://example.com/r.git", Ref: "v1", Subdir: "server",
		Containerfile: "Containerfile", Args: map[string]string{"A": "1"}, Auth: &config.GitAuth{Token: "t"},
	}
	if !reflect.DeepEqual(res.Image.Build, want) {
		t.Fatalf("build plan = %+v", res.Image.Build)
	}
	if res.HTTP == nil || res.HTTP.Port != 3000 || res.HTTP.Path != "/mcp" || res.HTTP.Transport != "http" {
		t.Fatalf("http = %+v", res.HTTP)
	}
	c := res.Container
	if c == nil || c.Name != "mcpm-svc" || c.Image != "" || c.Network != "mcpm-net" || c.Stdin {
		t.Fatalf("container = %+v", c)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 3000 || c.Ports[0].HostPort != 0 || c.Ports[0].Protocol != "tcp" {
		t.Fatalf("ports = %+v", c.Ports)
	}
	if !reflect.DeepEqual(c.Env, map[string]string{"A": "spec", "B": "override"}) {
		t.Fatalf("env = %v", c.Env)
	}
	if !reflect.DeepEqual(c.Mounts, []runtime.Mount{{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true}}) {
		t.Fatalf("mounts = %+v", c.Mounts)
	}
}

func TestBuildResolveHTTPCustom(t *testing.T) {
	spec := config.ServerSpec{
		Name:  "svc",
		Build: &config.BuildSpec{Repo: "r"},
		Serve: &config.ServeSpec{Mode: "http", Port: 80, Path: "/sse", Transport: "sse"},
	}
	res, err := Resolve(resolveCfg(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.HTTP.Path != "/sse" || res.HTTP.Transport != "sse" {
		t.Fatalf("http = %+v", res.HTTP)
	}
}

func TestBuildResolveStdioDefault(t *testing.T) {
	spec := config.ServerSpec{Name: "svc", Build: &config.BuildSpec{Repo: "r", Containerfile: "Dockerfile.mcp"}}
	res, err := Resolve(resolveCfg(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport != "stdio" || res.HTTP != nil {
		t.Fatalf("transport/http = %q/%+v", res.Transport, res.HTTP)
	}
	if !res.Container.Stdin || len(res.Container.Ports) != 0 || res.Container.Env != nil {
		t.Fatalf("container = %+v", res.Container)
	}
	if res.Image.Build.Containerfile != "Dockerfile.mcp" || res.Image.Ref != "" {
		t.Fatalf("image = %+v", res.Image)
	}
}

func TestBuildResolveErrors(t *testing.T) {
	cases := map[string]config.ServerSpec{
		"build is required":  {Name: "a"},
		"repo is required":   {Name: "a", Build: &config.BuildSpec{}},
		"unknown serve mode": {Name: "a", Build: &config.BuildSpec{Repo: "r"}, Serve: &config.ServeSpec{Mode: "grpc"}},
		"port is required":   {Name: "a", Build: &config.BuildSpec{Repo: "r"}, Serve: &config.ServeSpec{Mode: "http"}},
		"invalid volume": {Name: "a", Build: &config.BuildSpec{Repo: "r"},
			Container: &config.ContainerOverrides{Volumes: []string{"nope"}}},
	}
	for want, spec := range cases {
		_, err := Resolve(resolveCfg(), spec)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v", want, err)
		}
	}
}

func TestBuildResolveLeavesRegistryAuthNil(t *testing.T) {
	cfg := resolveCfg()
	cfg.Registries = []config.Registry{{Registry: "docker.io", Username: "u", Password: "p"}}
	res, err := Resolve(cfg, config.ServerSpec{Name: "svc", Build: &config.BuildSpec{Repo: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.Build.RegistryAuth != nil {
		t.Fatalf("RegistryAuth = %+v; want nil", res.Image.Build.RegistryAuth)
	}
}
