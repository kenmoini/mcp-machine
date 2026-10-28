package cli_test

import (
	"bytes"
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

func TestStubCommandsNotImplemented(t *testing.T) {
	for _, cmd := range []string{"serve", "validate", "import"} {
		t.Run(cmd, func(t *testing.T) {
			code, out, errs := run(t, cmd)
			want := "mcp-machine " + cmd + ": not implemented\n"
			if code != 1 || errs != want || out != "" {
				t.Fatalf("code=%d out=%q err=%q want err=%q", code, out, errs, want)
			}
		})
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
