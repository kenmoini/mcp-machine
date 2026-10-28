package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func noEnv(string) (string, bool) { return "", false }

func TestParseAppliesDefaults(t *testing.T) {
	c, err := config.Parse([]byte("servers: []\n"), noEnv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Listen", c.Listen, ":8080"},
		{"Podman.Socket", c.Podman.Socket, "unix:///run/podman/podman.sock"},
		{"Podman.Network", c.Podman.Network, "mcp-machine"},
		{"Podman.StopTimeout", c.Podman.StopTimeout, 10 * time.Second},
		{"DataDir", c.DataDir, "/data"},
		{"WorkspaceDir", c.WorkspaceDir, "/data/workspace"},
		{"Runtimes[npx]", c.Runtimes["npx"], "docker.io/library/node:22-bookworm-slim"},
		{"Log.Level", c.Log.Level, "info"},
	}
	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestParseEmptyDocument(t *testing.T) {
	c, err := config.Parse(nil, noEnv)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	if c.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080", c.Listen)
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	y := `
listen: ":9090"
data_dir: /var/lib/mm
podman:
  stop_timeout: 30s
  network: custom
runtimes:
  npx: example.com/node:1
log:
  level: debug
`
	c, err := config.Parse([]byte(y), noEnv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Listen != ":9090" || c.DataDir != "/var/lib/mm" || c.Podman.Network != "custom" ||
		c.Podman.StopTimeout != 30*time.Second || c.Log.Level != "debug" {
		t.Errorf("explicit values overridden: %+v", c)
	}
	if c.WorkspaceDir != "/var/lib/mm/workspace" {
		t.Errorf("WorkspaceDir = %q, want derived from data_dir", c.WorkspaceDir)
	}
	if c.Runtimes["npx"] != "example.com/node:1" {
		t.Errorf("Runtimes[npx] = %q, want explicit value", c.Runtimes["npx"])
	}
}

const envYAML = `
servers:
  - name: remote1
    type: remote
    remote:
      url: https://example.com/mcp
      headers:
        Authorization: "Bearer ${TOK}"
  - name: cmd1
    type: command
    command:
      command: npx
      args: ["-y", "pkg"]
      env:
        A: "${TOK}-x"
`

func TestParseExpandsEnvInHeadersAndEnv(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if k == "TOK" {
			return "secret", true
		}
		return "", false
	}
	c, err := config.Parse([]byte(envYAML), lookup)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Servers[0].Remote.Headers["Authorization"]; got != "Bearer secret" {
		t.Errorf("header = %q, want %q", got, "Bearer secret")
	}
	if got := c.Servers[1].Command.Env["A"]; got != "secret-x" {
		t.Errorf("command env = %q, want %q", got, "secret-x")
	}
	if got := c.Servers[0].Remote.Transport; got != "auto" {
		t.Errorf("Remote.Transport = %q, want auto", got)
	}
}

func TestParseExpandsAllFields(t *testing.T) {
	y := `
registries:
  - registry: ghcr.io
    username: "${U}"
    password: "${P}"
servers:
  - name: b
    type: build
    env:
      S: "${V}"
    build:
      repo: https://example.com/r.git
      args:
        ARG: "${V}"
      auth:
        token: "${V}"
    container:
      env:
        C: "${V}"
`
	lookup := func(k string) (string, bool) { return "v-" + k, true }
	c, err := config.Parse([]byte(y), lookup)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Registries[0].Username != "v-U" || c.Registries[0].Password != "v-P" {
		t.Errorf("registry not expanded: %+v", c.Registries[0])
	}
	s := c.Servers[0]
	if s.Env["S"] != "v-V" || s.Build.Args["ARG"] != "v-V" || s.Build.Auth.Token != "v-V" || s.Container.Env["C"] != "v-V" {
		t.Errorf("server fields not expanded: %+v", s)
	}
}

func TestParseMissingEnvVarIsError(t *testing.T) {
	_, err := config.Parse([]byte(envYAML), noEnv)
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
	if !strings.Contains(err.Error(), "${TOK} is not set") {
		t.Errorf("error = %q, want it to contain %q", err, "${TOK} is not set")
	}
}

func TestParseMissingEnvVarsListedTogether(t *testing.T) {
	y := `
registries:
  - registry: r
    username: "${AAA}"
    password: "${BBB}"
`
	_, err := config.Parse([]byte(y), noEnv)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"config: ${AAA} is not set", "config: ${BBB} is not set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := config.Parse([]byte("lisen: x\n"), noEnv)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "lisen") {
		t.Errorf("error = %q, want it to mention lisen", err)
	}
}

func TestLoadEnvOverridesSocket(t *testing.T) {
	t.Setenv("MCP_MACHINE_PODMAN_SOCKET", "unix:///tmp/p.sock")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Podman.Socket != "unix:///tmp/p.sock" {
		t.Errorf("Socket = %q, want env override", c.Podman.Socket)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestServerSpecDefaults(t *testing.T) {
	y := `
servers:
  - name: b
    type: build
    build:
      repo: https://example.com/r.git
  - name: c
    type: command
    command:
      command: npx
  - name: r
    type: remote
    remote:
      url: https://example.com/mcp
`
	c, err := config.Parse([]byte(y), noEnv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, cmd, r := c.Servers[0], c.Servers[1], c.Servers[2]
	if b.Build.Containerfile != "Containerfile" {
		t.Errorf("Build.Containerfile = %q", b.Build.Containerfile)
	}
	if cmd.Serve == nil || cmd.Serve.Mode != "stdio" {
		t.Errorf("command Serve = %+v, want mode stdio", cmd.Serve)
	}
	if b.Serve == nil || b.Serve.Mode != "stdio" {
		t.Errorf("build Serve = %+v, want mode stdio", b.Serve)
	}
	if r.Serve != nil || r.Build != nil || r.Command != nil {
		t.Errorf("absent sections were created: %+v", r)
	}
	if !b.IsEnabled() || !cmd.IsEnabled() || !r.IsEnabled() {
		t.Error("servers should default to enabled")
	}
	off := false
	if (config.ServerSpec{Enabled: &off}).IsEnabled() {
		t.Error("enabled: false should be disabled")
	}
}

func TestApplyDefaultsDoesNotCreateSections(t *testing.T) {
	c := &config.Config{Servers: []config.ServerSpec{{Name: "x", Type: "command"}}}
	config.ApplyDefaults(c)
	s := c.Servers[0]
	if s.Serve != nil || s.Remote != nil || s.Build != nil || s.Command != nil || s.Container != nil {
		t.Errorf("ApplyDefaults created sections: %+v", s)
	}
}

func TestRuntimeImage(t *testing.T) {
	c := &config.Config{Runtimes: map[string]string{"npx": "node:22"}}
	cases := []struct {
		name string
		s    config.ServerSpec
		want string
	}{
		{"runtime lookup", config.ServerSpec{Command: &config.CommandSpec{Command: "npx"}}, "node:22"},
		{"override wins", config.ServerSpec{
			Command:   &config.CommandSpec{Command: "npx"},
			Container: &config.ContainerOverrides{Image: "custom:1"},
		}, "custom:1"},
		{"unknown runtime", config.ServerSpec{Command: &config.CommandSpec{Command: "uvx"}}, ""},
		{"remote", config.ServerSpec{Remote: &config.RemoteSpec{URL: "https://x"}}, ""},
		{"build with image", config.ServerSpec{Container: &config.ContainerOverrides{Image: "b:1"}}, "b:1"},
	}
	for _, tc := range cases {
		if got := tc.s.RuntimeImage(c); got != tc.want {
			t.Errorf("%s: RuntimeImage = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLoadRejectsEmptyFile(t *testing.T) {
	for name, content := range map[string]string{
		"zero bytes": "",
		"whitespace": " \n\t\n  ",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(path)
			if err == nil || err.Error() != "config: file is empty" {
				t.Errorf("Load error = %v, want %q", err, "config: file is empty")
			}
		})
	}
}
