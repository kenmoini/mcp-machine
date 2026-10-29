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

## Layered configuration residuals (2026-10-28)

### A refused revert answers 500, not 409 or 422
- Where: `internal/web/forms.go` (`deleteServer`), `internal/config/dirwriter.go` (`DeleteServer` validation).
- Symptom: reverting an override that would leave an invalid set (for example an unset `${VAR}` in the admin definition) correctly refuses and re-renders the form, but with status 500 although it is a user-actionable refusal. The message also doubles the `config:` prefix and, with several unset variables, breaks the sentence across lines.
- Fix: map the validation refusal to 409 like the other refusals; strip the inner `config:` prefix when wrapping.

### A retrying first apply can use a stale set
- Where: `internal/cli/serve.go` (`applyUntilSuccess` reads `state.currentSet()` outside `applyMu`; the watcher calls `state.loaded` before `applyOnce`).
- Symptom: after an adoption-scan failure at startup, a retry can apply a set that a newer reload has already superseded; two overlapping reloads (UI reload plus the watcher) can apply out of order. Narrow window, self-correcting at the next event.
- Fix: read the set inside `applyMu`, or serialize load+apply behind one lock (see "Concurrent UI writes" above).

### Delete is offered for a canonical file that holds several servers
- Where: `internal/web/forms.go` (`deleteActionFor`).
- Symptom: when `/data/config.d/<name>.yaml` is the effective file but defines more than one server, the UI offers Delete or Revert and the writer refuses with 409. Hand-placed files are the only way to reach this.
- Fix: have the config view expose whether the canonical file is exclusive, or read it in the handler, and show the "not managed by the UI" note instead.

### Small test and polish items from the per-task reviews
- Watcher: prove a late-created drop-in directory gets its own watch (write a second file after the first reload); test the dot-prefix branch; re-run pending resolution after a resolve so nested `MkdirAll` directories are not stranded; report non-ENOENT `Add` errors through `onError`; add a timeout to the import-burst test.
- DirWriter: check `NameRE` before stat-ing `path(name)`; wrap `ErrAdminOwned` with the name; tests for update-replaces-existing, failed validation leaving the file untouched, and update of a foreign-only server.
- Serve: `prepareUserDir` "exists but unwritable" branch untested (chmod 0500 subtest); directory is created before validation, so an invalid config still leaves `/data/config.d` behind.
- Web: plain-text 409 for a hand-made POST swaps into the page under hx-boost (render through the form instead); revert redirects to `/` rather than the server page; `Origin.AdminFiles()` would remove a duplicated loop.
- Config: `TestLoadFileWrapperUnchanged` is tautological; `Set.Searched` aliases `Layout.Searched`; the admin type-mismatch error bypasses the `fileError` helper.

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

### Deliver `${VAR}` environment through the secrets tmpfs too
- Where: `internal/manager/secretenv.go`, `internal/sources/command/resolve.go`.
- Symptom: a `${VAR}` secret still lands in the create spec, which Podman persists on disk and shows in `podman inspect`; only `${vault:...}` values go through the tmpfs delivery.
- Fix: route every expanded env value through `SecretEnv`. It would change the create spec of every server (wrapper entrypoint, tmpfs) and so recreate every container once on upgrade; needs a migration note.

### Vault secrets as files
- Where: `internal/manager/secretenv.go`.
- Symptom: a server that wants a credentials file (`GOOGLE_APPLICATION_CREDENTIALS`, a kubeconfig) has to get it as an env var and write it itself.
- Fix: a `secret_files:` map of container path to `${vault:...}` reference, delivered into the same tmpfs by the same exec; the wrapper already keeps the directory.

### Images without `/bin/sh` cannot take Vault secrets
- Where: `internal/manager/secretenv.go` (the wrapper and the delivery command are shell).
- Symptom: a distroless or scratch image exits with `exec /bin/sh: no such file or directory`.
- Fix: ship a small static init binary in the MCP Machine image, copy it into a named volume once, and mount it into such containers as the wrapper and the writer.
