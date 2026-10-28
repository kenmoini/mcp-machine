package clientconfig_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources/clientconfig"
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
