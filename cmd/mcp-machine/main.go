// Command mcp-machine is a self-hosted proxy and manager for MCP servers.
package main

import (
	"os"

	"github.com/kenmoini/mcp-machine/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
