package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenmoini/mcp-machine/internal/buildinfo"
	"github.com/kenmoini/mcp-machine/internal/cli"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Main(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionPrintsVersion(t *testing.T) {
	code, out, errs := run(t, "version")
	want := "mcp-machine " + buildinfo.Version + " (" + buildinfo.Commit + ", " + buildinfo.Date + ")\n"
	if code != 0 || out != want || errs != "" {
		t.Fatalf("code=%d out=%q err=%q want out=%q", code, out, errs, want)
	}
}

func TestBuildinfoDefaults(t *testing.T) {
	if buildinfo.Version != "dev" || buildinfo.Commit != "none" || buildinfo.Date != "unknown" {
		t.Fatalf("unexpected defaults: %q %q %q", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	code, out, errs := run(t, "bogus")
	if code != 2 || !strings.Contains(errs, "usage:") || out != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestNoArgsIsUsageError(t *testing.T) {
	code, _, errs := run(t)
	if code != 2 || !strings.Contains(errs, "usage:") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

const importFixture = `{"mcpServers":{"airtable":{"command":"npx","args":["-y","airtable-mcp-server"],"env":{"K":"${AIRTABLE_API_KEY}"}}}}`

// importGolden is the full yaml.v3 default encoder output, which indents
// sequences under a mapping key by four spaces and nested maps by four.
const importGolden = "servers:\n" +
	"    - name: airtable\n" +
	"      type: command\n" +
	"      command:\n" +
	"        command: npx\n" +
	"        args:\n" +
	"            - -y\n" +
	"            - airtable-mcp-server\n" +
	"        env:\n" +
	"            K: ${AIRTABLE_API_KEY}\n"

func TestImportCommandPrintsYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := os.WriteFile(path, []byte(importFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "import", path)
	if code != 0 || errs != "" || out != importGolden {
		t.Fatalf("code=%d err=%q out=%q want %q", code, errs, out, importGolden)
	}
}

func TestImportReadsStdin(t *testing.T) {
	var out, errb bytes.Buffer
	code := cli.Main([]string{"import", "-"}, strings.NewReader(importFixture), &out, &errb)
	if code != 0 || errb.String() != "" || out.String() != importGolden {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
}

func TestImportUsageError(t *testing.T) {
	for _, args := range [][]string{{"import"}, {"import", "a", "b"}} {
		code, out, errs := run(t, args...)
		if code != 2 || out != "" || !strings.Contains(errs, "usage: mcp-machine import <file|->") {
			t.Fatalf("args=%v code=%d out=%q err=%q", args, code, out, errs)
		}
	}
}

func TestImportErrorsAreRuntimeErrors(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"mcpServers":{"x":{"type":"nope","url":"u"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bad, filepath.Join(t.TempDir(), "missing.json")} {
		code, out, errs := run(t, "import", path)
		if code != 1 || out != "" || !strings.HasPrefix(errs, "mcp-machine import: ") {
			t.Fatalf("path=%s code=%d out=%q err=%q", path, code, out, errs)
		}
	}
}

func TestConnectUsageError(t *testing.T) {
	for _, args := range [][]string{{"connect"}, {"connect", "http://a", "http://b"}} {
		code, out, errs := run(t, args...)
		if code != 2 || out != "" || !strings.Contains(errs, "usage: mcp-machine connect <url>") {
			t.Fatalf("args=%v code=%d out=%q err=%q", args, code, out, errs)
		}
	}
}

func TestConnectUnreachableIsRuntimeError(t *testing.T) {
	code, out, errs := run(t, "connect", "http://127.0.0.1:1/servers/demo/mcp")
	if code != 1 || out != "" || errs == "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestHelpPrintsUsageAndSucceeds(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		code, out, errs := run(t, arg)
		if code != 0 || !strings.Contains(out, "usage: mcp-machine") || !strings.Contains(out, "serve") || errs != "" {
			t.Errorf("%s: code=%d out=%q err=%q", arg, code, out, errs)
		}
	}
}

func TestUsageNamesConfigLocations(t *testing.T) {
	_, out, _ := run(t, "help")
	for _, want := range []string{
		"  serve      run the proxy and web UI (-c /etc/mcp-machine, --system-config-d, --config-d /data/config.d, --catalog /usr/share/mcp-machine/catalog)\n",
		"  validate   validate configuration (-c /etc/mcp-machine, --system-config-d, --config-d /data/config.d)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage lacks %q:\n%s", want, out)
		}
	}
}
