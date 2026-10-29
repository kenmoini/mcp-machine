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
	// ImageConfig returns the entrypoint, command and user an image declares.
	ImageConfig(ctx context.Context, ref string) (ImageConfig, error)
	// Exec runs cmd inside a running container, feeding it stdin (nil for
	// none) until EOF, and returns its exit code.
	Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (exitCode int, err error)
}

// ImageConfig is the part of an image's configuration that decides what
// runs when a container starts.
type ImageConfig struct {
	Entrypoint, Cmd []string
	User            string
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name, Image         string
	Entrypoint, Command []string
	Env, Labels         map[string]string
	// SecretEnv holds the environment variables whose values still contain
	// ${vault:...} references. An Engine never sees them: the manager
	// resolves them right before the container starts and delivers them
	// inside it (see manager/secretenv.go). CreateContainer rejects a spec
	// that still has any.
	SecretEnv     map[string]string
	Stdin         bool
	Network       string
	Ports         []PortMapping
	Mounts        []Mount
	WorkDir       string
	RestartPolicy string
}

// PortMapping publishes a container port on the host.
type PortMapping struct {
	HostIP                  string
	HostPort, ContainerPort int
	Protocol                string
}

// Mount describes a bind, volume or tmpfs mount.
type Mount struct {
	Type     string // "bind" | "volume" | "tmpfs"
	Source   string // host path (bind) or volume name (volume); unused for tmpfs
	Target   string
	ReadOnly bool
	// Relabel asks the engine to relabel the content for SELinux: "z"
	// (shared between containers), "Z" (private to this container) or ""
	// (leave the label alone).
	Relabel string
	// Options are extra mount options for a tmpfs, comma-separated (for
	// example "nosuid,nodev,mode=1777,size=1m"); ignored for the other
	// types. A string, not a slice, so Mount stays comparable.
	Options string
}

// ContainerInfo is the observable state of a container.
type ContainerInfo struct {
	ID, Name, Image, State string
	Labels                 map[string]string
	ExitCode               int
	Ports                  []PortMapping
	Networks               []string // names of attached networks, sorted; may be empty
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
