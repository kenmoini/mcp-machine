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
	"os"
	"strings"
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
	"github.com/kenmoini/mcp-machine/internal/vault"
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

// detectSelf reports whether MCP Machine runs in a container and, if so, its
// container ID, which the manager needs to join the servers' network. When
// the ID cannot be determined it logs a warning and returns "": serve goes on
// without the self-join.
func detectSelf(ctx context.Context, engine runtime.Engine, logger *slog.Logger) (inContainer bool, selfID string) {
	hint, inContainer := runtime.DetectSelf(containerenvPath)
	if !inContainer {
		return false, ""
	}
	id, err := runtime.ResolveSelfID(ctx, engine, hint)
	if err != nil {
		logger.Warn("running in a container but its ID is unknown; not joining the servers' network, so http-mode servers may be unreachable (set "+runtime.SelfIDEnv+")", "err", err)
		return true, ""
	}
	return true, id
}

const (
	applyMinBackoff = time.Second
	applyMaxBackoff = 30 * time.Second
)

// newManager builds the server manager and starts what keeps it in step with
// the configuration: a goroutine applying set (retrying until the first Apply
// succeeds) and one watching every location of the layout l. ctx is the
// long-lived serve context; the goroutines and every Apply use it, never a
// request context. deps.close stops them and the manager.
//
// While no administrator file is loaded (see applyOnce) the manager keeps the
// labeled containers of servers it does not know, so a missing configuration
// mount does not remove running servers.
func newManager(ctx context.Context, l config.Layout, set *config.Set, engine runtime.Engine, logger *slog.Logger) (serveDeps, error) {
	cfg := set.Config
	inContainer, selfID := detectSelf(ctx, engine, logger)
	reg := store.NewRegistry()
	vc, err := newVaultClient(ctx, cfg.Vault, logger)
	if err != nil {
		return serveDeps{}, err
	}
	builder := &build.Builder{
		Workspace:  cfg.WorkspaceDir,
		Engine:     engine,
		Cloner:     build.GoGitCloner{},
		Registries: cfg.Registries,
	}
	mopts := manager.Options{
		InContainer: inContainer,
		SelfID:      selfID,
		Logger:      logger,
		Version:     buildinfo.Version,
	}
	if vc != nil { // never a typed nil in the interfaces
		builder.Secrets = vc
		mopts.Vault = vc
	}
	mgr := manager.New(engine, resolver.New(), builder, reg, mopts)
	state := &configState{layout: l, set: set}
	logLoaded(logger, l, set)

	runCtx, cancel := context.WithCancel(ctx)
	watcher, err := config.NewWatcher(l,
		func(next *config.Set) {
			state.loaded(next)
			logLoaded(logger, l, next)
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
		if vc != nil {
			vc.Close()
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

// newVaultClient builds and starts the Vault client when the configuration
// has a vault section, else returns nil. The vault section is read at
// startup only, like podman.socket. A client that cannot be built (a
// missing credential file, say) is an error; one whose first
// authentication fails is kept, with a warning: the servers that need it
// fail on their own and retry, and a token renewal or re-login happens in
// the background.
func newVaultClient(ctx context.Context, vcfg config.VaultConfig, logger *slog.Logger) (*vault.Client, error) {
	if !vcfg.Configured() {
		return nil, nil
	}
	vc, err := vault.New(vault.FromConfig(vcfg), logger)
	if err != nil {
		return nil, err
	}
	if err := vc.Start(ctx); err != nil {
		logger.Warn("vault: authentication failed; servers that reference secrets will fail until it succeeds", "address", vcfg.Address, "err", err)
	}
	return vc, nil
}

// logLoaded logs, at info, what the operator should know about a set just
// loaded from l: each server defined by more than one file, with those
// files, and that no administrator file is among its files, so unknown
// containers are kept. (Finding no file at all is the startup warning
// instead.)
func logLoaded(logger *slog.Logger, l config.Layout, set *config.Set) {
	for _, s := range set.Config.Servers {
		o := set.Origins[s.Name]
		if len(o.Files) < 2 {
			continue
		}
		paths := make([]string, len(o.Files))
		for i, f := range o.Files {
			paths[i] = f.Path
		}
		logger.Info("config: server merged", "server", s.Name, "files", strings.Join(paths, ","))
	}
	if len(set.Files) > 0 && len(set.AdminFiles()) == 0 {
		admin := []string{l.BaseFile}
		if l.SystemDir != "" {
			admin = append(admin, l.SystemDir)
		}
		logger.Info(fmt.Sprintf("no administrator configuration loaded (searched: %s); unknown containers are kept",
			strings.Join(admin, ", ")))
	}
}

// applyOnce applies set's configuration and records the outcome. Labeled
// containers of servers set does not define are kept when it holds no
// administrator file (no base file, legacy file or system drop-in), so a
// missing admin mount cannot remove the servers it defined. A failed
// adoption scan (manager.ErrAdoptionScan) started nothing: it returns
// applied=false, retry=true so the caller can try again. Any other error is per-server: the
// config was applied, those servers are failed in the registry and retry on
// their own, so it is logged and the config counts as applied.
func applyOnce(ctx context.Context, mgr *manager.Manager, state *configState, set *config.Set, logger *slog.Logger) (applied, retry bool) {
	// The flag and the Apply that reads it are paired under applyMu, so a
	// concurrent apply of another set cannot swap the flag in between.
	state.applyMu.Lock()
	mgr.SetKeepUnknown(len(set.AdminFiles()) == 0)
	err := mgr.Apply(ctx, set.Config)
	state.applyMu.Unlock()
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

// applyUntilSuccess applies the current set, retrying with a doubling
// backoff while the adoption scan fails (nothing was started), until it is
// applied or ctx ends. The set is re-read on every attempt so a reload that
// arrives meanwhile is not overwritten by a stale one.
func applyUntilSuccess(ctx context.Context, mgr *manager.Manager, state *configState, logger *slog.Logger) {
	backoff := applyMinBackoff
	for {
		applied, retry := applyOnce(ctx, mgr, state, state.currentSet(), logger)
		if applied || !retry || ctx.Err() != nil {
			return
		}
		logger.Info("retrying configuration apply", "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, applyMaxBackoff)
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

// prepareUserDir creates the user drop-in directory of l and checks that it
// can be written. notice is "" when it can; otherwise it says why editing is
// disabled, including when there is no user layer. A directory that cannot
// be created (or is not a directory) is dropped from the returned layout, as
// there is nothing to read from it; one that exists but cannot be written
// stays, and is still read.
func prepareUserDir(l config.Layout) (config.Layout, string) {
	if l.UserDir == "" {
		return l, `editing is disabled: no user drop-in directory (--config-d "")`
	}
	disabled := func(err error) string {
		return fmt.Sprintf("editing is disabled: %s is not writable (%v)", l.UserDir, err)
	}
	if fi, err := os.Stat(l.UserDir); err == nil && !fi.IsDir() {
		notice := fmt.Sprintf("editing is disabled: %s is not a directory", l.UserDir)
		l.UserDir = ""
		return l, notice
	}
	if err := os.MkdirAll(l.UserDir, 0o750); err != nil {
		notice := disabled(err)
		l.UserDir = ""
		return l, notice
	}
	f, err := os.CreateTemp(l.UserDir, ".probe-*")
	if err != nil {
		return l, disabled(err)
	}
	_ = f.Close()
	if err := os.Remove(f.Name()); err != nil {
		return l, disabled(err)
	}
	return l, ""
}

// runServe implements "serve": it wires the engine, adapters, and ops
// endpoints into one HTTP server and runs it until ctx is cancelled.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer, opts Options) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := addConfigFlags(fs)
	listen := fs.String("listen", "", "listen address, overriding the configuration")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "mcp-machine serve: %v\n", err)
		return exitRuntime
	}

	l, err := cf.layout()
	if err != nil {
		return fail(err)
	}
	l, editNotice := prepareUserDir(l)
	set, err := loadSet(l)
	if err != nil {
		return fail(err)
	}
	cfg := set.Config
	if *listen != "" {
		cfg.Listen = *listen
	}
	logger := newLogger(stderr, cfg.Log)
	if w := configWarning(set); w != "" {
		logger.Warn(w)
	}
	var writer web.ConfigWriter
	if editNotice != "" {
		logger.Warn(editNotice)
	} else if l.UserDir != "" {
		writer = config.DirWriter{Dir: l.UserDir, Layout: l}
	}

	// TLS and the listener come first: if either fails, nothing has been
	// started (no engine connection, no containers).
	var tlsConfig *tls.Config
	scheme := "http"
	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return fail(fmt.Errorf("load TLS key pair: %w", err))
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		scheme = "https"
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fail(err)
	}
	// srv.Serve closes ln once it runs; until then every exit closes it.
	serving := false
	defer func() {
		if !serving {
			_ = ln.Close()
		}
	}()

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

	deps, err := newManager(ctx, l, set, engine, logger)
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
		Manager:        deps.mgr,
		Registry:       deps.reg,
		Config:         deps.state,
		Writer:         writer,
		EditNotice:     editNotice,
		PublicURL:      cfg.PublicURL,
		AllowedOrigins: cfg.AllowedOrigins,
		Version:        buildinfo.Version,
		Logger:         logger,
	}))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout, TLSConfig: tlsConfig}
	fmt.Fprintf(stdout, "listening on %s://%s\n", scheme, ln.Addr())

	serving = true
	errCh := make(chan error, 1)
	go func() {
		if tlsConfig != nil {
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
