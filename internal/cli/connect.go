package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/kenmoini/mcp-machine/internal/adapters/stdio"
)

// nopWriteCloser adds a no-op Close to a Writer.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// runConnect implements "connect <url>": it bridges stdin/stdout to a
// Streamable HTTP MCP endpoint. stdout carries only MCP traffic; all logs go
// to stderr.
func runConnect(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: mcp-machine connect <url>")
		return exitUsage
	}
	in, ok := stdin.(io.ReadCloser)
	if !ok {
		in = io.NopCloser(stdin)
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if err := stdio.Run(ctx, args[0], in, nopWriteCloser{stdout}, logger); err != nil {
		fmt.Fprintf(stderr, "mcp-machine connect: %v\n", err)
		return exitRuntime
	}
	return exitOK
}
