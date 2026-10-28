package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/kenmoini/mcp-machine/internal/adapters/httpadapter"
	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/ops"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/podman"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 15 * time.Second
)

// serveDeps is the seam through which serve receives the parts that later
// tasks provide (the server manager, the web UI). newManager builds it.
type serveDeps struct {
	// lookup resolves a server name for the protocol adapters.
	lookup httpadapter.Lookup
	// metrics returns the current metrics snapshot.
	metrics func() ops.Metrics
	// close releases the manager's resources after the HTTP server stops.
	close func()
}

// newManager builds the serve dependencies. It is a stub that serves no
// backends; the manager task replaces it.
func newManager(_ context.Context, _ *config.Config, _ runtime.Engine, _ *slog.Logger) (serveDeps, error) {
	return serveDeps{
		lookup:  func(string) (*mcp.Server, bool) { return nil, false },
		metrics: func() ops.Metrics { return ops.Metrics{} },
		close:   func() {},
	}, nil
}

// newLogger builds the process logger from the log configuration.
func newLogger(w io.Writer, lc config.LogConfig) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(lc.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if lc.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// runServe implements "serve": it wires the engine, adapters, and ops
// endpoints into one HTTP server and runs it until ctx is cancelled.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer, opts Options) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("c", defaultConfigPath, "path to the configuration file")
	listen := fs.String("listen", "", "listen address, overriding the configuration")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "mcp-machine serve: %v\n", err)
		return exitRuntime
	}

	cfg, err := loadConfig(*path)
	if err != nil {
		return fail(err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	logger := newLogger(stderr, cfg.Log)

	newEngine := opts.NewEngine
	if newEngine == nil {
		newEngine = func(ctx context.Context, socket string) (runtime.Engine, error) {
			return podman.New(ctx, socket)
		}
	}
	engine, err := newEngine(ctx, cfg.Podman.Socket)
	if err != nil {
		return fail(err)
	}

	deps, err := newManager(ctx, cfg, engine, logger)
	if err != nil {
		return fail(err)
	}
	defer deps.close()

	mux := http.NewServeMux()
	if err := httpadapter.Mount(mux, deps.lookup, httpadapter.Options{
		AllowedOrigins: cfg.AllowedOrigins,
		PublicURL:      cfg.PublicURL,
		Logger:         logger,
	}); err != nil {
		return fail(err)
	}
	ops.Mount(mux, engine.Ping, deps.metrics)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fail(err)
	}
	useTLS := cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != ""
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	fmt.Fprintf(stdout, "listening on %s://%s\n", scheme, ln.Addr())

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	errCh := make(chan error, 1)
	go func() {
		if useTLS {
			errCh <- srv.ServeTLS(ln, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			errCh <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fail(err)
		}
		return exitOK
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
	return exitOK
}
