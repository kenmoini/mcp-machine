// Package runtime defines the container-engine abstraction used by MCP
// Machine. It deliberately has no Podman imports so every consumer can be
// unit-tested against the in-memory engine in the fake subpackage.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when a container (or other object) does not exist.
var ErrNotFound = errors.New("runtime: not found")

// Engine is the set of container operations MCP Machine needs.
type Engine interface {
	Ping(ctx context.Context) error
	EnsureNetwork(ctx context.Context, name string) error
	ConnectNetwork(ctx context.Context, containerID, network string) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	PullImage(ctx context.Context, ref string, auth *RegistryAuth, progress io.Writer) error
	BuildImage(ctx context.Context, req BuildRequest) (imageID string, err error)
	CreateContainer(ctx context.Context, spec ContainerSpec) (id string, err error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (ContainerInfo, error)
	ListContainers(ctx context.Context, labels map[string]string) ([]ContainerInfo, error)
	Logs(ctx context.Context, id string, tail int) ([]LogLine, error)
	Attach(ctx context.Context, id string) (io.ReadWriteCloser, error) // call before StartContainer
	Wait(ctx context.Context, id string) (exitCode int, err error)
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name, Image         string
	Entrypoint, Command []string
	Env, Labels         map[string]string
	Stdin               bool
	Network             string
	Ports               []PortMapping
	Mounts              []Mount
	WorkDir             string
	RestartPolicy       string
}

// PortMapping publishes a container port on the host.
type PortMapping struct {
	HostIP                  string
	HostPort, ContainerPort int
	Protocol                string
}

// Mount describes a bind or volume mount.
type Mount struct {
	Type     string // "bind" | "volume"
	Source   string
	Target   string
	ReadOnly bool
}

// ContainerInfo is the observable state of a container.
type ContainerInfo struct {
	ID, Name, Image, State string
	Labels                 map[string]string
	ExitCode               int
	Ports                  []PortMapping
}

// BuildRequest describes an image build.
type BuildRequest struct {
	ContextDir, Containerfile, Tag string
	Args, Labels                   map[string]string
	Auth                           *RegistryAuth
}

// RegistryAuth holds registry credentials.
type RegistryAuth struct{ Username, Password string }

// LogLine is one line of container output.
type LogLine struct{ Stream, Text string }
