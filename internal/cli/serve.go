package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kenmoini/mcp-machine/internal/adapters/httpadapter"
	"github.com/kenmoini/mcp-machine/internal/buildinfo"
	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/manager"
	"github.com/kenmoini/mcp-machine/internal/ops"
	"github.com/kenmoini/mcp-machine/internal/runtime"
	"github.com/kenmoini/mcp-machine/internal/runtime/podman"
	"github.com/kenmoini/mcp-machine/internal/sources/build"
	"github.com/kenmoini/mcp-machine/internal/sources/resolver"
	"github.com/kenmoini/mcp-machine/internal/store"
	"github.com/kenmoini/mcp-machine/internal/web"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 15 * time.Second
)

// serveDeps is what serve receives from newManager: the parts the HTTP layer
// mounts, plus the manager, registry and config state the web layer uses.
type serveDeps struct {
	// lookup resolves a server name for the protocol adapters.
	lookup httpadapter.Lookup
	// metrics returns the current metrics snapshot.
	metrics func() ops.Metrics
	// ready is the readiness probe: the engine answers and the first config
	// has been applied.
	ready ops.Probe
	// close stops the config watcher and the manager after the HTTP server
	// has stopped.
	close func()

	mgr   *manager.Manager
	reg   *store.Registry
	state *configState
}

// containerenvPath is where Podman records the container's own identity.
// It is a variable so tests can point it elsewhere.
var containerenvPath = "/run/.containerenv"

const (
	applyBackoffMin = time.Second
	applyBackoffMax = 30 * time.Second
)

// newManager builds the server manager and starts what keeps it in step with
// the configuration: a goroutine applying cfg (retrying until the first Apply
// succeeds) and one watching the config file at path. ctx is the long-lived
// serve context; the goroutines and every Apply use it, never a request
// context. deps.close stops them and the manager.
func newManager(ctx context.Context, path string, cfg *config.Config, engine runtime.Engine, logger *slog.Logger) (serveDeps, error) {
	selfID, inContainer := runtime.DetectSelf(containerenvPath)
	reg := store.NewRegistry()
	builder := &build.Builder{
		Workspace:  cfg.WorkspaceDir,
		Engine:     engine,
		Cloner:     build.GoGitCloner{},
		Registries: cfg.Registries,
	}
	mgr := manager.New(engine, resolver.New(), builder, reg, manager.Options{
		InContainer: inContainer,
		SelfID:      selfID,
		Logger:      logger,
		Version:     buildinfo.Version,
	})
	state := &configState{current: cfg}

	runCtx, cancel := context.WithCancel(ctx)
	watcher, err := config.NewWatcher(path,
		func(next *config.Config) {
			state.loaded(next)
			if applied, _ := applyOnce(runCtx, mgr, state, next, logger); applied {
				logger.Info("configuration reloaded")
			}
		},
		func(err error) {
			state.failed(err)
			logger.Error("reload config", "err", err)
		})
	if err != nil {
		cancel()
		return serveDeps{}, err
	}
	state.watcher = watcher

	var wg sync.WaitGroup
	wg.Go(func() { applyUntilSuccess(runCtx, mgr, state, logger) })
	wg.Go(func() {
		if err := watcher.Run(runCtx); err != nil {
			logger.Error("config watcher stopped", "err", err)
		}
	})

	closeAll := func() {
		cancel()
		_ = watcher.Close()
		shutCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer stop()
		waited := make(chan struct{})
		go func() { wg.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-shutCtx.Done():
			logger.Warn("background goroutines did not stop in time")
		}
		if err := mgr.Close(shutCtx); err != nil {
			logger.Error("close manager", "err", err)
		}
	}

	return serveDeps{
		lookup: mgr.Lookup,
		metrics: func() ops.Metrics {
			reloads, errs := state.counters()
			calls := map[string]uint64{}
			for _, st := range reg.List() {
				if f, ok := mgr.Facade(st.Name); ok {
					calls[st.Name] = f.Calls()
				}
			}
			return ops.Metrics{
				ServersByState:     reg.CountByState(),
				ProxyCalls:         calls,
				ConfigReloads:      reloads,
				ConfigReloadErrors: errs,
			}
		},
		ready: func(ctx context.Context) error {
			if err := engine.Ping(ctx); err != nil {
				return err
			}
			if !state.isApplied() {
				return errors.New("config applied: false")
			}
			return nil
		},
		close: closeAll,
		mgr:   mgr,
		reg:   reg,
		state: state,
	}, nil
}

// applyOnce applies cfg and records the outcome. A failed adoption scan
// (manager.ErrAdoptionScan) started nothing: it returns applied=false,
// retry=true so the caller can try again. Any other error is per-server: the
// config was applied, those servers are failed in the registry and retry on
// their own, so it is logged and the config counts as applied.
func applyOnce(ctx context.Context, mgr *manager.Manager, state *configState, cfg *config.Config, logger *slog.Logger) (applied, retry bool) {
	err := mgr.Apply(ctx, cfg)
	switch {
	case err == nil:
	case errors.Is(err, manager.ErrAdoptionScan):
		logger.Error("apply configuration: container scan failed", "err", err)
		return false, true
	case errors.Is(err, manager.ErrClosed):
		return false, false
	default:
		logger.Warn("configuration applied with server errors", "err", err)
	}
	state.markApplied()
	return true, false
}

// applyUntilSuccess applies the current config, retrying with a doubling
// backoff while the adoption scan fails (nothing was started), until it is
// applied or ctx ends. The config is re-read on every attempt so a reload
// that arrives meanwhile is not overwritten by a stale one.
func applyUntilSuccess(ctx context.Context, mgr *manager.Manager, state *configState, logger *slog.Logger) {
	backoff := applyBackoffMin
	for {
		applied, retry := applyOnce(ctx, mgr, state, state.Current(), logger)
		if applied || !retry || ctx.Err() != nil {
			return
		}
		logger.Info("retrying configuration apply", "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, applyBackoffMax)
	}
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

	deps, err := newManager(ctx, *path, cfg, engine, logger)
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
	ops.Mount(mux, deps.ready, deps.metrics)
	// The UI owns everything else. It is mounted last: the adapters' more
	// specific /servers/{name}/mcp and /sse patterns win over its "/".
	mux.Handle("/", web.New(web.Deps{
		Manager:   deps.mgr,
		Registry:  deps.reg,
		Config:    deps.state,
		PublicURL: cfg.PublicURL,
		Version:   buildinfo.Version,
		Logger:    logger,
	}))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	useTLS := cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != ""
	scheme := "http"
	if useTLS {
		// Load the key pair before listening so a bad path fails before
		// "listening on" is printed.
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return fail(fmt.Errorf("load TLS key pair: %w", err))
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		scheme = "https"
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "listening on %s://%s\n", scheme, ln.Addr())

	errCh := make(chan error, 1)
	go func() {
		if useTLS {
			errCh <- srv.ServeTLS(ln, "", "")
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
		// Long-lived streams outlast the grace period: drop them.
		logger.Error("shutdown", "err", err)
		_ = srv.Close()
	}
	return exitOK
}
