package config_test

import (
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
)

func anyEnv(string) (string, bool) { return "x", true }

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // substring of the error; "" means valid
	}{
		{
			name: "dup names",
			yaml: `
servers:
  - {name: a, type: remote, remote: {url: "http://x/mcp"}}
  - {name: a, type: remote, remote: {url: "http://y/mcp"}}
`,
			want: `duplicate server name "a"`,
		},
		{
			name: "bad name My_Server",
			yaml: `
servers:
  - {name: My_Server, type: remote, remote: {url: "http://x/mcp"}}
`,
			want: `server "My_Server": invalid name`,
		},
		{
			name: "empty name",
			yaml: `
servers:
  - {type: remote, remote: {url: "http://x/mcp"}}
`,
			want: `invalid name`,
		},
		{
			name: "name too long",
			yaml: `
servers:
  - {name: ` + strings.Repeat("a", 64) + `, type: remote, remote: {url: "http://x/mcp"}}
`,
			want: `invalid name`,
		},
		{
			name: "reserved name new",
			yaml: `
servers:
  - {name: new, type: remote, remote: {url: "http://x/mcp"}}
`,
			want: `server "new": name is reserved`,
		},
		{
			name: "reserved name import",
			yaml: `
servers:
  - {name: import, type: remote, remote: {url: "http://x/mcp"}}
`,
			want: `reserved`,
		},
		{
			name: "invalid type",
			yaml: `
servers:
  - {name: a, type: docker}
`,
			want: `server "a": type must be remote, command, or build`,
		},
		{
			name: "type remote without remote block",
			yaml: `
servers:
  - {name: a, type: remote}
`,
			want: `server "a": remote section required`,
		},
		{
			name: "type command without command block",
			yaml: `
servers:
  - {name: a, type: command}
`,
			want: `command section required`,
		},
		{
			name: "type build without build block",
			yaml: `
servers:
  - {name: a, type: build}
`,
			want: `build section required`,
		},
		{
			name: "type remote with extra command block",
			yaml: `
servers:
  - name: a
    type: remote
    remote: {url: "http://x/mcp"}
    command: {command: npx}
`,
			want: `server "a": only the remote section may be set`,
		},
		{
			name: "type command with mismatched build block",
			yaml: `
servers:
  - name: a
    type: command
    build: {repo: "https://example.com/r.git"}
`,
			want: `only the command section may be set`,
		},
		{
			name: "type command with both command and run",
			yaml: `
servers:
  - name: a
    type: command
    command: {command: npx, run: "npx foo"}
`,
			want: `command.command and command.run are mutually exclusive`,
		},
		{
			name: "type command with neither",
			yaml: `
servers:
  - name: a
    type: command
    command: {}
`,
			want: `one of command.command or command.run`,
		},
		{
			name: "serve mode http without port",
			yaml: `
servers:
  - name: a
    type: command
    command: {command: npx}
    serve: {mode: http}
`,
			want: `serve.port required`,
		},
		{
			name: "serve mode udp",
			yaml: `
servers:
  - name: a
    type: command
    command: {command: npx}
    serve: {mode: udp}
`,
			want: `serve.mode must be stdio or http`,
		},
		{
			name: "remote transport grpc",
			yaml: `
servers:
  - {name: a, type: remote, remote: {url: "http://x/mcp", transport: grpc}}
`,
			want: `remote.transport must be http, sse, or auto`,
		},
		{
			name: "tls cert without key",
			yaml: `
tls: {cert_file: /c.pem}
`,
			want: `tls.cert_file and tls.key_file must both be set`,
		},
		{
			name: "tls key without cert",
			yaml: `
tls: {key_file: /k.pem}
`,
			want: `tls.cert_file and tls.key_file must both be set`,
		},
		{
			name: "podman socket http",
			yaml: `
podman: {socket: "http://x"}
`,
			want: `podman.socket must start with unix:// or tcp://`,
		},
		{
			name: "podman stop timeout under 1s",
			yaml: `
podman: {stop_timeout: 500ms}
`,
			want: `podman.stop_timeout must be at least 1s`,
		},
		{
			name: "negative podman stop timeout",
			yaml: `
podman: {stop_timeout: -5s}
`,
			want: `podman.stop_timeout must be at least 1s`,
		},
		{
			name: "allowed origin not absolute",
			yaml: `
allowed_origins: ["http://ok.example:8080", "just-a-host"]
`,
			want: `allowed_origins[1] must be an absolute origin like http://host:port`,
		},
		{
			name: "allowed origin without host",
			yaml: `
allowed_origins: ["http://"]
`,
			want: `allowed_origins[0] must be an absolute origin like http://host:port`,
		},
		{
			name: "public url relative",
			yaml: `
public_url: /mcp
`,
			want: `public_url must be an absolute URL`,
		},
		{
			name: "valid full config",
			yaml: `
listen: ":9090"
public_url: https://mcp.example.com
tls: {cert_file: /c.pem, key_file: /k.pem}
podman: {socket: "tcp://127.0.0.1:2375", network: net, stop_timeout: 30s}
allowed_origins: ["http://localhost:3000", "https://app.example.com"]
registries:
  - {registry: ghcr.io, username: u, password: "${PW}"}
servers:
  - name: remote-one
    type: remote
    remote: {url: "https://x.example/mcp", transport: sse, headers: {A: b}}
  - name: cmd-one
    type: command
    command: {command: npx, args: ["-y", "pkg"]}
  - name: cmd-run
    type: command
    command: {run: "npx -y pkg"}
    serve: {mode: http, port: 3000, path: /mcp}
  - name: built
    type: build
    build: {repo: "https://github.com/example/r.git", ref: main}
    serve: {mode: http, port: 8000}
`,
			want: "",
		},
		{
			name: "empty config is valid",
			yaml: `servers: []`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Parse([]byte(tc.yaml), anyEnv)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = config.Validate(c)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateJoinsAllProblems(t *testing.T) {
	c, err := config.Parse([]byte(`
tls: {cert_file: /c.pem}
podman: {socket: "http://x"}
servers:
  - {name: new, type: remote}
`), anyEnv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = config.Validate(c)
	if err == nil {
		t.Fatal("Validate: want error, got nil")
	}
	for _, want := range []string{
		"tls.cert_file and tls.key_file must both be set",
		"podman.socket must start with unix:// or tcp://",
		`server "new": name is reserved`,
		`server "new": remote section required`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q; got:\n%v", want, err)
		}
	}
}

func TestNameRE(t *testing.T) {
	for _, ok := range []string{"a", "a1", "my-server", "0abc", strings.Repeat("a", 63)} {
		if !config.NameRE.MatchString(ok) {
			t.Errorf("NameRE should match %q", ok)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a.b", strings.Repeat("a", 64)} {
		if config.NameRE.MatchString(bad) {
			t.Errorf("NameRE should not match %q", bad)
		}
	}
}
