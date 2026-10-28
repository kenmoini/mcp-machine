package clientconfig_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources/clientconfig"
	"github.com/kenmoini/mcp-machine/internal/sources/command"
)

func TestParseCommandEntry(t *testing.T) {
	data := `{"mcpServers":{"airtable":{"command":"npx","args":["-y","airtable-mcp-server"],"env":{"K":"${AIRTABLE_API_KEY}"}}}}`
	got, err := clientconfig.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.ServerSpec{{
		Name: "airtable",
		Type: "command",
		Command: &config.CommandSpec{
			Command: "npx",
			Args:    []string{"-y", "airtable-mcp-server"},
			Env:     map[string]string{"K": "${AIRTABLE_API_KEY}"},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestParseHttpAndSseEntries(t *testing.T) {
	data := `{"mcpServers":{
		"b-sse":{"type":"sse","url":"https://b.example/sse","headers":{"Authorization":"Bearer x"}},
		"a-http":{"type":"http","url":"https://a.example/mcp","headers":{"X-K":"v"}},
		"c-stream":{"type":"streamable-http","url":"https://c.example/mcp"},
		"d-url":{"url":"https://d.example/mcp"},
		"e-stdio":{"type":"stdio","command":"uvx","args":["srv"]}
	}}`
	got, err := clientconfig.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	remote := func(url, transport string, h map[string]string) *config.RemoteSpec {
		return &config.RemoteSpec{URL: url, Transport: transport, Headers: h}
	}
	want := []config.ServerSpec{
		{Name: "a-http", Type: "remote", Remote: remote("https://a.example/mcp", "http", map[string]string{"X-K": "v"})},
		{Name: "b-sse", Type: "remote", Remote: remote("https://b.example/sse", "sse", map[string]string{"Authorization": "Bearer x"})},
		{Name: "c-stream", Type: "remote", Remote: remote("https://c.example/mcp", "http", nil)},
		{Name: "d-url", Type: "remote", Remote: remote("https://d.example/mcp", "http", nil)},
		{Name: "e-stdio", Type: "command", Command: &config.CommandSpec{Command: "uvx", Args: []string{"srv"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestParseBareObject(t *testing.T) {
	data := `{"zed":{"command":"a"},"alpha":{"url":"https://x.example/mcp"}}`
	got, err := clientconfig.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zed" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, data, want string
	}{
		{"unknown type", `{"mcpServers":{"x":{"type":"websocket","url":"ws://x"}}}`, `websocket`},
		{"neither command nor url", `{"mcpServers":{"lonely":{"args":["a"]}}}`, `lonely`},
		{"remote type without url", `{"mcpServers":{"nourl":{"type":"sse"}}}`, `nourl`},
		{"empty name", `{"mcpServers":{"!!!":{"command":"a"}}}`, `!!!`},
		{"duplicate sanitized", `{"mcpServers":{"My Server":{"command":"a"},"my-server":{"command":"b"}}}`, `my-server`},
		{"invalid json", `{`, `parse`},
		{"not an mcp config", `{"other":1}`, `mcpServers`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clientconfig.Parse([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want substring %q", err, tt.want)
			}
		})
	}
}

func TestParseUnknownTypeIsError(t *testing.T) {
	_, err := clientconfig.Parse([]byte(`{"mcpServers":{"x":{"type":"carrier-pigeon","url":"https://x"}}}`))
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("err=%v", err)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"My Server!":                   "my-server",
		"airtable":                     "airtable",
		"--a__b..c--":                  "a-b-c",
		"UPPER_case":                   "upper-case",
		"!!!":                          "",
		strings.Repeat("a", 70):        strings.Repeat("a", 63),
		strings.Repeat("a", 62) + "!b": strings.Repeat("a", 62),
	}
	for in, want := range tests {
		if got := clientconfig.SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseDockerRunEntry(t *testing.T) {
	data := `{"mcpServers":{"github":{
		"command":"docker",
		"args":["run","-i","--rm","-e","GITHUB_PERSONAL_ACCESS_TOKEN","-e","LABEL=two words","ghcr.io/github/github-mcp-server","--toolsets","repos,issues","it's"],
		"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"${GITHUB_TOKEN}"}
	}}}`
	got, err := clientconfig.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.ServerSpec{{
		Name: "github",
		Type: "command",
		Command: &config.CommandSpec{
			Run: `docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN -e 'LABEL=two words' ghcr.io/github/github-mcp-server --toolsets repos,issues 'it'\''s'`,
			Env: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got[0].Command, want[0].Command)
	}
}

func TestParsePodmanRunEntry(t *testing.T) {
	got, err := clientconfig.Parse([]byte(`{"mcpServers":{"p":{"command":"podman","args":["run","-i","img"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Command.Run != "podman run -i img" || got[0].Command.Command != "" || got[0].Command.Args != nil {
		t.Fatalf("command = %+v", got[0].Command)
	}
	// docker without "run" is left alone (and rejected by validation).
	got, err = clientconfig.Parse([]byte(`{"mcpServers":{"p":{"command":"docker","args":["ps"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Command.Command != "docker" || got[0].Command.Run != "" {
		t.Fatalf("command = %+v", got[0].Command)
	}
}

func TestImportGitHubDockerEntryRuns(t *testing.T) {
	// The GitHub MCP server's documented client config, end to end: import,
	// validate, resolve into a container.
	data := `{"mcpServers":{"github":{
		"command":"docker",
		"args":["run","-i","--rm","-e","GITHUB_PERSONAL_ACCESS_TOKEN","ghcr.io/github/github-mcp-server"],
		"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"ghp_example"}
	}}}`
	specs, err := clientconfig.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Servers: specs}
	config.ApplyDefaults(cfg)
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	res, err := command.Resolve(cfg, cfg.Servers[0])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	c := res.Container
	if c.Image != "ghcr.io/github/github-mcp-server" || !c.Stdin {
		t.Fatalf("container = %+v", c)
	}
	if c.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "ghp_example" {
		t.Fatalf("env = %v", c.Env)
	}
}
