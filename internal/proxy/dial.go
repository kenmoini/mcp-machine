package proxy

import (
	"context"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/buildinfo"
)

// DialIO connects an MCP client session over a duplex byte stream, such as an
// attach stream to a container's stdio. The session owns rwc and closes it
// when the session closes.
func DialIO(ctx context.Context, rwc io.ReadWriteCloser, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-machine", Version: buildinfo.Version}, opts)
	return client.Connect(ctx, &mcp.IOTransport{Reader: rwc, Writer: rwc}, nil)
}
