# Server lifecycle

Every configured server has a runner that moves it through these states. The
current state, the last error, the time of the last change, the restart count,
and the paused flag are visible on the dashboard, on the server page, and in
`GET /api/servers`.

## States

| State | Meaning |
|---|---|
| `defined` | The runner exists; nothing has been done yet. |
| `resolving` | The config entry is being turned into an endpoint or a container spec. |
| `building` | A `build` server is cloning its repository and building the image. |
| `image-ready` | The image is present (pulled, or built). |
| `creating` | The container is being created and started. For stdio this is attach, then start. |
| `running` | The container is up. |
| `connecting` | MCP Machine is establishing the backend MCP session. For an http-mode container this includes probing the endpoint for up to 60 seconds. |
| `ready` | The session is bound to the facade; the server's `/mcp` and `/sse` routes answer. |
| `stopped` | The container (if any) is stopped but kept, for a fast restart. |
| `failed` | Something went wrong; the error is recorded. A retry may be scheduled. |
| `removing` | The container is being stopped and removed; the server then disappears. |

## Transitions

```
defined -> resolving -> [building] -> image-ready -> creating -> running -> connecting -> ready
                                                                                \-> stopped
any state -> failed{error}       (automatic retry with backoff, then sticky)
any state -> removing -> gone

remote servers: defined -> resolving -> connecting -> ready      (no container states)
```

- A server from a `command` entry skips `building`. A `remote` entry skips
  every container state.
- Leaving `ready`: if the backend session closes or the container exits, the
  server goes to `failed` and a retry is scheduled. The error quotes the exit
  code and the last log lines when the container's exit explains the failure.
- `stopped` keeps the container. Starting a stopped server whose container
  still exists and matches the current spec resumes that container instead of
  rebuilding it.

## Retries and backoff

After a failure the runner schedules an automatic retry with exponential
backoff: 1s, 2s, 4s, 8s, 16s, 32s, then 60s for every further attempt. After 10
automatic retries the failure is sticky: the server stays `failed` until you
press Start (or Restart, Rebuild) or change its configuration. Start resets the
retry budget, and so does a server that stays `ready` for at least 60 seconds:
its next failure is a new incident with a fresh budget of 10 retries. A retry resumes from `creating` when the image is already known
and from the top (`resolving`) otherwise; each retry re-creates the container
(stop, remove, create). The restart counter increments on every automatic retry.

## Stop, start, and paused

The UI Stop action stops the container, keeps it, and sets the **paused** flag.
A paused server stays stopped across config reloads for as long as its spec hash
is unchanged; editing the server so its hash changes replaces the runner with a
fresh one, which starts normally. Start clears the flag.

`enabled: false` in the config also stops a server, but without the paused flag:
setting it back to `true` (or removing the line) starts the server again on the
next reload. Toggling `enabled` never changes the spec hash.

Resync re-reads the backend's tool, resource, and prompt lists into the proxy without touching the container. Restart stops and starts the server (a fresh container). Rebuild applies to
`build` servers only: it re-clones, force-builds the image, and starts a new
container; other types answer with an error. Remove (UI action) stops and
removes the container and drops the runner. If the entry is still in the config,
the server comes back the next time the configuration is applied. Deleting a
server through the UI removes the entry from the file, and the reload removes
the container.

## Reconcile

The configuration is applied at startup and after every successful hot reload.
Applies are serialized. For the enabled servers MCP Machine builds a map of
name to **spec hash**, then:

- creates a runner for a server that has none;
- replaces the runner (container included) of a server whose hash changed;
- removes the runner and container of a server that is no longer in the file;
- stops a server that became `enabled: false`, and starts one that became
  enabled again;
- leaves everything else alone, including paused servers.

Applying the same configuration twice does nothing.

The spec hash is the sha256 of the canonical JSON of the server entry (with
`enabled` cleared) plus the runtime image the entry depends on. Because
`${VAR}` references are resolved before hashing, changing the value of an
environment variable that an entry uses also changes its hash and recreates
that server.

## Adoption

On the first apply after MCP Machine starts, it lists the containers labeled
`io.mcp-machine/managed=true` and decides, per container:

- the server is in the config, enabled, and the container's hash label matches:
  **adopt** it. An http-mode container is adopted in place: a running one is
  connected to without a restart. A stdio container cannot be re-attached
  safely, so a running one is stopped and then started again through the normal
  attach path (a restart);
- anything else (stale hash, server removed, server disabled, duplicate): stop
  and remove it.

If the container list cannot be fetched, nothing is started or removed and the
apply is retried with a doubling delay (1s up to 30s) until the engine answers.
`/readyz` reports not ready until a configuration has been applied.

An invalid configuration never reaches the manager: the previous one stays in
effect and the error is shown on the Config page (and counted in
`mcp_machine_config_reload_errors_total`).
