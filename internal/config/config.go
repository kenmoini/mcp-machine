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
	Enabled   *bool               `yaml:"enabled,omitempty"`
	Remote    *RemoteSpec         `yaml:"remote,omitempty"`
	Command   *CommandSpec        `yaml:"command,omitempty"`
	Build     *BuildSpec          `yaml:"build,omitempty"`
	Serve     *ServeSpec          `yaml:"serve,omitempty"`
	Container *ContainerOverrides `yaml:"container,omitempty"`
	Env       map[string]string   `yaml:"env,omitempty"`
}

// RemoteSpec describes a remote MCP server reached over HTTP or SSE.
type RemoteSpec struct {
	URL       string            `yaml:"url,omitempty"`
	Transport string            `yaml:"transport,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
}

// CommandSpec describes a server launched from a command inside a runtime image.
type CommandSpec struct {
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Run     string            `yaml:"run,omitempty"`
}

// BuildSpec describes a server built from a git repository.
type BuildSpec struct {
	Repo          string            `yaml:"repo,omitempty"`
	Ref           string            `yaml:"ref,omitempty"`
	Subdir        string            `yaml:"subdir,omitempty"`
	Containerfile string            `yaml:"containerfile,omitempty"`
	Args          map[string]string `yaml:"args,omitempty"`
	Auth          *GitAuth          `yaml:"auth,omitempty"`
}

// GitAuth holds git credentials for a build source.
type GitAuth struct {
	Token      string `yaml:"token,omitempty"`
	SSHKeyFile string `yaml:"ssh_key_file,omitempty"`
	// KnownHostsFile verifies the SSH server's host key. The json tag keeps
	// it out of config.Hash while unset, so existing specs keep their hash.
	KnownHostsFile string `yaml:"known_hosts_file,omitempty" json:",omitempty"`
}

// ServeSpec describes how a container exposes its MCP endpoint.
type ServeSpec struct {
	Mode      string `yaml:"mode,omitempty"`
	Port      int    `yaml:"port,omitempty"`
	Path      string `yaml:"path,omitempty"`
	Transport string `yaml:"transport,omitempty"`
}

// ContainerOverrides are optional overrides on the generated container spec.
type ContainerOverrides struct {
	Image   string            `yaml:"image,omitempty"`
	Volumes []string          `yaml:"volumes,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
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
