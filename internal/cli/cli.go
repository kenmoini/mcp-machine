// Package cli implements the mcp-machine command line.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/kenmoini/mcp-machine/internal/buildinfo"
	"github.com/kenmoini/mcp-machine/internal/runtime"
)

// Exit codes.
const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

// Options carries injectable dependencies for Run.
type Options struct {
	// NewEngine connects to the container engine at socket. Nil means
	// podman.New.
	NewEngine func(ctx context.Context, socket string) (runtime.Engine, error)
}

// Main runs the CLI with default options. The context is cancelled on SIGINT
// or SIGTERM so long-running commands (serve, connect) stop cleanly.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, args, stdin, stdout, stderr, Options{})
}

// Run dispatches to a subcommand and returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch cmd := args[0]; cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "version":
		fmt.Fprintf(stdout, "mcp-machine %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return exitOK
	case "connect":
		return runConnect(ctx, args[1:], stdin, stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr, opts)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "import":
		return runImport(args[1:], stdin, stdout, stderr)
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
  help       print this help

Run "mcp-machine <command> -h" for a command's flags.
`)
}
