// Package config defines the MCP Machine configuration model and loads it
// from YAML.
package config

import "time"

// Config is the top-level MCP Machine configuration.
type Config struct {
	Listen         string            `yaml:"listen"`
	PublicURL      string            `yaml:"public_url"`
	TLS            TLSConfig         `yaml:"tls"`
	Podman         PodmanConfig      `yaml:"podman"`
	WorkspaceDir   string            `yaml:"workspace_dir"`
	DataDir        string            `yaml:"data_dir"`
	AllowedOrigins []string          `yaml:"allowed_origins"`
	Registries     []Registry        `yaml:"registries"`
	Runtimes       map[string]string `yaml:"runtimes"`
	Log            LogConfig         `yaml:"log"`
	Servers        []ServerSpec      `yaml:"servers"`
}

// TLSConfig holds the optional TLS certificate pair.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// PodmanConfig describes how to reach and use Podman.
type PodmanConfig struct {
	Socket      string        `yaml:"socket"`
	Network     string        `yaml:"network"`
	StopTimeout time.Duration `yaml:"stop_timeout"`
}

// Registry holds credentials for an image registry.
type Registry struct {
	Registry string `yaml:"registry"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// LogConfig controls logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// ServerSpec is one managed MCP server entry.
type ServerSpec struct {
	Name      string              `yaml:"name"`
	Type      string              `yaml:"type"`
	Enabled   *bool               `yaml:"enabled"`
	Remote    *RemoteSpec         `yaml:"remote"`
	Command   *CommandSpec        `yaml:"command"`
	Build     *BuildSpec          `yaml:"build"`
	Serve     *ServeSpec          `yaml:"serve"`
	Container *ContainerOverrides `yaml:"container"`
	Env       map[string]string   `yaml:"env"`
}

// RemoteSpec describes a remote MCP server reached over HTTP or SSE.
type RemoteSpec struct {
	URL       string            `yaml:"url"`
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
}

// CommandSpec describes a server launched from a command inside a runtime image.
type CommandSpec struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	Run     string            `yaml:"run"`
}

// BuildSpec describes a server built from a git repository.
type BuildSpec struct {
	Repo          string            `yaml:"repo"`
	Ref           string            `yaml:"ref"`
	Subdir        string            `yaml:"subdir"`
	Containerfile string            `yaml:"containerfile"`
	Args          map[string]string `yaml:"args"`
	Auth          *GitAuth          `yaml:"auth"`
}

// GitAuth holds git credentials for a build source.
type GitAuth struct {
	Token      string `yaml:"token"`
	SSHKeyFile string `yaml:"ssh_key_file"`
}

// ServeSpec describes how a container exposes its MCP endpoint.
type ServeSpec struct {
	Mode      string `yaml:"mode"`
	Port      int    `yaml:"port"`
	Path      string `yaml:"path"`
	Transport string `yaml:"transport"`
}

// ContainerOverrides are optional overrides on the generated container spec.
type ContainerOverrides struct {
	Image   string            `yaml:"image"`
	Volumes []string          `yaml:"volumes"`
	Env     map[string]string `yaml:"env"`
}

// IsEnabled reports whether the server is enabled; an unset flag means true.
func (s ServerSpec) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// RuntimeImage returns the image a server will use: the container image
// override if set, else the runtime image for its command, else "".
func (s ServerSpec) RuntimeImage(c *Config) string {
	if s.Container != nil && s.Container.Image != "" {
		return s.Container.Image
	}
	if s.Command != nil && c != nil {
		return c.Runtimes[s.Command.Command]
	}
	return ""
}
