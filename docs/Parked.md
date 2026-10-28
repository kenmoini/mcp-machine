# Parked items

Known gaps and improvements deferred past v1. Each entry names where the behavior lives, what a user would notice, and a suggested fix. None of these block the shipped behavior; items under "Worth doing first" affect everyday use.

## Worth doing first

### UI writes block for the whole reconcile
- Where: `internal/web/forms.go` (`reloadAfterWrite`) → `internal/cli/serve_state.go` → `internal/manager/reconcile.go`.
- Symptom: deleting a server, or editing one so its hash changes, holds the POST until the old container stops (up to `podman.stop_timeout`, 10s by default) or until an in-progress reconcile finishes. Nothing fails; the redirect is slow.
- Fix: return the redirect after the write and let the reload run in the background, with the detail page tolerating a "pending" state; or reconcile removals asynchronously.

### Concurrent UI writes can briefly apply a stale config
- Where: the same path as above. Each write is serialized by the writer's mutex, but each handler's load-then-Apply is not.
- Symptom: two overlapping UI edits can roll one back for about 250ms until the watcher's debounced reload restores it; each UI edit also counts twice in `mcp_machine_config_reloads_total`.
- Fix: serialize load+apply behind the same lock, or have the handler only trigger the watcher's reload rather than applying itself.

### `${VAR}` inside a `docker run` line's image or volume path is required but never expanded
- Where: `internal/config/load.go` (`UnsetVars` scans the whole `command.run`) versus `internal/sources/command/resolve.go` (expands only `-e` values and args).
- Symptom: the variable must be set or the config fails to load, yet the image reference or `-v` path still reaches Podman as literal `${VAR}` text.
- Fix: either expand those positions in the resolver as well or exclude them from the load-time check and document that only env values and args are expanded.

## Lifecycle and manager

### Remove action semantics
- Where: `internal/manager/runner.go` (`remove`) and `docs/state-machine.md`.
- Symptom: Remove forgets the runner while the YAML entry stays, so the server comes back at the next unrelated reload.
- Fix: make Remove delete the container but keep the server as Stopped/paused, or have it delete the YAML entry.

### Shutdown budget can exceed Podman's stop timeout
- Where: `internal/cli/serve.go` (15s HTTP drain plus 15s manager close) versus Podman's default 10s stop grace.
- Symptom: under compose or Quadlet, Podman may SIGKILL MCP Machine while it is still draining long-lived SSE streams.
- Fix: cancel SSE request contexts via `RegisterOnShutdown`, and set `StopTimeout=`/`stop_grace_period` in the shipped units.

### Retry budget is still finite under a crash loop
- Where: `internal/manager/runner.go` (`fail`, `readySince`).
- Symptom: a server that never stays Ready for 60s exhausts ten retries and needs a manual Start.
- Fix: optional per-server `restart: always` with capped backoff and no sticky Failed.

### `--network host` deployments
- Where: `internal/runtime/self.go`, `internal/manager/endpoint.go`.
- Symptom: `/run/.containerenv` now always means in-container, so an MCP Machine run with `--network host` dials sibling container names it cannot resolve.
- Fix: detect host networking (no `mcp-machine` network membership after self-join) and fall back to published ports; document the deployment or reject it.

### Stale containers of unknown servers whose removal failed
- Where: `internal/manager/reconcile.go` (`adopt`).
- Symptom: a labeled container for a server no longer in the config whose force-remove errors stays until the next process start.
- Fix: retry removal on later reconciles.

### Facade capability gate is fixed at session start
- Where: `internal/proxy/facade.go` (`Resync`).
- Symptom: a backend that advertises no tools capability at initialize and adds its first tool later is never listed, even after a list-changed notification.
- Fix: treat a list-changed notification for a capability as proof that it exists.

### Leaked legacy SSE sessions on runner replacement
- Where: `internal/adapters/httpadapter` and the facade's bound session.
- Symptom: SSE sessions bound to a replaced runner's facade are never closed; calls return "backend unavailable" until the client reconnects, and goroutines linger.
- Fix: track SSE sessions per facade and close them on unbind.

## Operations and UX

### Transient states answer 404 on the MCP routes
- Where: `internal/manager/manager.go` (`Lookup` only when Ready).
- Symptom: clients see "unknown server" while a server is starting or retrying and may treat it as permanent.
- Fix: return 503 with `Retry-After` for known-but-not-ready servers.

### stdio protocol traffic appears in logs
- Where: `internal/runtime/podman/engine.go` (`Logs`), `internal/manager/runner_stdio.go` (`exitError`).
- Symptom: the logs view and the "last log lines" in failure messages include the JSON-RPC stdout stream for stdio servers.
- Fix: prefer stderr for stdio transports.

### Build images are never pruned
- Where: `internal/sources/build/builder.go`.
- Symptom: every new commit adds a `localhost/mcp-machine/<name>:<sha7>` image.
- Fix: remove older tags for the same server after a successful build, keeping the last N.

### Build output is not shown in the UI
- Where: `internal/runtime/podman/engine.go` (`BuildImage`, `BuildOptions.Out` unset).
- Symptom: build progress goes to the daemon's stdout only.
- Fix: capture build output into a per-server buffer exposed by the logs partial while in the Building state.

### First run with an empty config file exits
- Where: `internal/config/set.go` (`readConfigFile`), `internal/cli/serve.go`.
- Symptom: a missing configuration now starts `serve` with defaults and a "no configuration found" warning, but a configuration file that exists and is empty (or only whitespace) is still an error, so `serve` exits 1, although the UI could add servers.
- Fix: treat an empty file as `{}`, or keep the error but start with defaults and show it on the Config page.

### Error-level log noise from the bindings on every stop
- Where: Podman bindings' stdin copy goroutine on `rwc.Close()`.
- Symptom: `Failed to write input to service: io: read/write on closed pipe` at error level.
- Fix: close stdin before cancelling the attach, or lower the bindings' logrus level.

### Dashboard polling pauses while an action error is shown
- Where: `internal/web/templates/partials/_server_rows.html`.
- Symptom: the table stops refreshing until the error is clicked away.
- Fix: keep polling and render the error in a separate flash area.

### Import preview echoes pasted secrets
- Where: `internal/web/importpage.go`.
- Symptom: literal tokens pasted in a `docker run` line or `mcpServers` env are shown in the preview and written as-is.
- Fix: warn when a value looks like a secret and suggest a `${VAR}` placeholder.

### Minor hardening and polish
- Host-only registry entries (`ghcr.io`) also match `ghcr.io:5000/x`; restrict the boundary to repository paths.
- The `/config` page shows `allowed_origins`/`public_url` checks as documented but `isAbsoluteURL` accepts any scheme and paths; restrict to http/https without a path.
- `unix://` and `tcp://` with an empty remainder pass socket validation.
- `remote.url` and `build.repo` presence is validated by the resolvers, not at config load; add config-level checks for earlier errors.
- `Delete` on the status registry does not notify subscribers.
- The per-attach Podman connection leaves one idle keep-alive socket outside the tracker; call `CloseIdleConnections` before swapping the transport.
- Token git auth is restricted to `https://`; document that `http://` remotes need a different auth mode.
- The sha-ref clone path fetches all branches; use a targeted fetch.
- Static assets have no ETag or version in the URL; after an upgrade they can be cached for up to 24 hours.
- `serve -h` exits 2; only the top-level `help`/`-h` exit 0.
- The config example and docs could mention that `enabled` omitted in a scripted POST re-enables a server.
- Blanking a scalar from a later admin layer is not possible in v1: an empty or zero value in a system drop-in keeps what the base file set. It would need a tombstone or an explicit-null syntax.
