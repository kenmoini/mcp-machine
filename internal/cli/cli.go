// Package cli implements the mcp-machine command line.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/kenmoini/mcp-machine/internal/buildinfo"
)

// Exit codes.
const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

// Options carries injectable dependencies for Run. Fields are added as the
// subcommands that need them are implemented.
type Options struct{}

// Main runs the CLI with a background context and default options.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return Run(context.Background(), args, stdin, stdout, stderr, Options{})
}

// Run dispatches to a subcommand and returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch cmd := args[0]; cmd {
	case "version":
		fmt.Fprintf(stdout, "mcp-machine %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return exitOK
	case "serve", "connect", "validate", "import":
		fmt.Fprintf(stderr, "mcp-machine %s: not implemented\n", cmd)
		return exitRuntime
	default:
		fmt.Fprintf(stderr, "mcp-machine: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: mcp-machine <command> [flags]

commands:
  serve      run the proxy and web UI
  connect    bridge stdio to a running mcp-machine
  validate   validate configuration
  import     import MCP server definitions
  version    print version information
`)
}
