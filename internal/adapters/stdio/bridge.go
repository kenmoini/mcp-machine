// Package stdio bridges a stdio MCP client to a Streamable HTTP MCP endpoint.
package stdio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenmoini/mcp-machine/internal/buildinfo"
	"github.com/kenmoini/mcp-machine/internal/proxy"
)

// errUpstreamLost reports that the HTTP session ended before stdio did.
var errUpstreamLost = errors.New("stdio bridge: upstream session closed")

// Run bridges stdio (in/out) to the Streamable HTTP MCP endpoint at url.
//
// It returns nil when ctx is cancelled or the stdio side closes cleanly, and
// the underlying error otherwise, including when the upstream session is lost.
func Run(ctx context.Context, url string, in io.ReadCloser, out io.WriteCloser, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	facade := proxy.New("connect", buildinfo.Version)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-machine-connect", Version: buildinfo.Version}, facade.ClientOptions())
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown was requested while connecting; that is not a failure.
			return nil //nolint:nilerr // a cancelled context is a clean exit, not an error
		}
		return fmt.Errorf("connect to %s: %w", url, err)
	}
	return runWith(ctx, facade, sess, in, out, logger)
}

// run bridges stdio to an already connected upstream session.
func run(ctx context.Context, sess *mcp.ClientSession, in io.ReadCloser, out io.WriteCloser, logger *slog.Logger) error {
	facade := proxy.New("connect", buildinfo.Version)
	return runWith(ctx, facade, sess, in, out, logger)
}

// runWith mirrors sess through facade and serves it on stdio until ctx ends,
// stdio closes, or sess closes. It closes sess before returning.
func runWith(ctx context.Context, facade *proxy.Facade, sess *mcp.ClientSession, in io.ReadCloser, out io.WriteCloser, logger *slog.Logger) error {
	defer sess.Close()
	if err := facade.Bind(ctx, sess); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("sync upstream: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := make(chan struct{})
	go func() {
		_ = sess.Wait()
		close(lost)
		cancel()
	}()

	logger.Info("stdio bridge serving")
	err := facade.Server().Run(runCtx, &mcp.IOTransport{Reader: in, Writer: out})

	select {
	case <-lost:
		if ctx.Err() == nil {
			return errUpstreamLost
		}
	default:
	}
	if ctx.Err() != nil {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}
