package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/sources/clientconfig"
)

// runImport implements "import <file|->": it reads client-config JSON
// (mcpServers) from a file or stdin and prints an equivalent YAML servers
// list to stdout.
func runImport(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: mcp-machine import <file|->")
		return exitUsage
	}

	var (
		data []byte
		err  error
	)
	if path := fs.Arg(0); path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path) //nolint:gosec // G304: reading the file the operator named on the command line is the purpose of this command
	}
	if err != nil {
		fmt.Fprintf(stderr, "mcp-machine import: %v\n", err)
		return exitRuntime
	}

	servers, err := clientconfig.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "mcp-machine import: %v\n", err)
		return exitRuntime
	}
	out, err := yaml.Marshal(struct {
		Servers []config.ServerSpec `yaml:"servers"`
	}{servers})
	if err != nil {
		fmt.Fprintf(stderr, "mcp-machine import: %v\n", err)
		return exitRuntime
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "mcp-machine import: %v\n", err)
		return exitRuntime
	}
	return exitOK
}
