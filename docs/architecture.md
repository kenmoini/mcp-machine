# Architecture

MCP Machine is one Go binary shipped as one container. It talks to a Podman
socket, runs MCP servers as sibling containers (or connects to remote ones),
and re-exposes every server to clients over Streamable HTTP, legacy HTTP+SSE,
and stdio (through `mcp-machine connect`).

```
clients (Claude Code, Cursor, mcp-machine connect, mcp-remote)
   |  Streamable HTTP  /servers/{name}/mcp     (2026-07-28 + legacy, Stateless)
   |  HTTP+SSE         /servers/{name}/sse     (2024-11-05 clients)
   v
+---------------------- mcp-machine (one container) -----------------------+
|  web UI (html/template + htmx)   |  adapters: StreamableHTTPHandler,       |
|  config watcher + writer (YAML)  |            SSEHandler                   |
|  manager / reconciler            |  proxy facade: mcp.Server per server   |
|  runtime.Engine (podman bindings |  backend session: mcp.ClientSession    |
|  over the unix socket)           |                                         |
+----------------------------------------------------------------------------+
   | unix socket                 | stdio over container attach | HTTP on network
   v                             v                             v
 podman              sibling container (stdio)     sibling container (http)
                                                    or a remote URL
```

## Packages

| Package | Role |
|---|---|
| `cmd/mcp-machine`, `internal/cli` | Entry point and the subcommands `serve`, `connect`, `validate`, `import`, `version`. |
| `internal/config` | YAML model, defaults, `${VAR}` expansion, validation, spec hashing, the layered loader and merge, the file watcher, and the drop-in writer. |
| `internal/manager` | One runner per server. Reconciles the desired config against running containers, drives each server through its lifecycle, retries with backoff. |
| `internal/store` | The in-memory status registry the manager writes and the UI reads, plus the container label helpers. |
| `internal/sources` | Turns a config entry into something runnable: `remote` (an endpoint), `command` (a container spec), `build` (a git clone plus an image build plan). |
| `internal/runtime` | The `Engine` interface, a Podman implementation (`internal/runtime/podman`), and an in-memory fake used by unit tests. |
| `internal/proxy` | The facade: one `mcp.Server` per managed server that mirrors a backend session. |
| `internal/adapters` | `httpadapter` mounts the Streamable HTTP and SSE routes with origin protection; `stdio` is the `connect` bridge. |
| `internal/web` | Server-rendered UI (`html/template`, vendored htmx 2.0.10, no JavaScript build step). |
| `internal/ops` | `/healthz`, `/readyz`, `/metrics`. |

## The proxy facade

For each managed server there is one backend `mcp.ClientSession`:

- over an `IOTransport` wrapped around the container attach stream, for a
  stdio container;
- over a Streamable HTTP or SSE client transport, for a remote server or an
  http-mode container.

Next to it sits one facade `mcp.Server` whose tools, resources, resource
templates, and prompts mirror the backend's, and whose handlers forward the raw
call to the backend session. The facade re-syncs its lists when the backend
sends a list-changed notification and whenever a session is (re)bound. Both the
Streamable HTTP handler and the SSE handler serve the same facade. A server's
routes answer only while it is `ready`; before that they return 404 with
`{"error":"unknown server"}`.

Only tools, resources, resource templates, prompts, and list-changed
notifications are forwarded. Sampling, elicitation, progress, logging, and
resource subscriptions are not (see the limitations in the README).

### Two protocol eras

MCP revision 2026-07-28 changed the wire protocol: no `initialize` handshake,
every request carries its own protocol version and client info, Streamable HTTP
is POST-only, and `Mcp-Session-Id` is gone. Revisions 2025-11-25 and earlier
("legacy") use `initialize` plus sessions, and HTTP+SSE from 2024-11-05 is a
separate legacy transport. MCP Machine relies on the Go SDK for every
combination of client era and backend era: the Streamable HTTP handler runs in
stateless mode (needed to serve modern clients, and legacy clients still
negotiate down on the same endpoint), the SSE handler serves old clients, and
the SDK client side negotiates with the backend automatically. That is why
`GET` and `DELETE` on `/servers/{name}/mcp` answer 405.

## Sibling containers

Managed containers are named `mcpm-<name>`, attached to the user-defined
network from `podman.network` (default `mcp-machine`), and labeled:

| Label | Value |
|---|---|
| `io.mcp-machine/managed` | `true` |
| `io.mcp-machine/server` | the server name |
| `io.mcp-machine/hash` | the spec hash (see [state-machine.md](state-machine.md)) |
| `io.mcp-machine/transport` | `stdio` or `http` |

How MCP Machine reaches an http-mode sibling depends on where it runs. Inside a
container (detected by the presence of `/run/.containerenv`) it joins the
sibling network itself, so a plain `podman run` needs no `--network` flag, and
it dials the sibling by container name. Joining needs its own container ID,
which Podman writes into `/run/.containerenv` only for privileged containers;
otherwise `serve` resolves its host name through the Podman API, and as a last
resort reads `MCP_MACHINE_CONTAINER_ID`. If none works it logs a warning and
does not join. As a bare binary on the host (development), the
sibling publishes its port on `127.0.0.1` with an ephemeral host port and the
endpoint dials that port.

A stdio container is created with stdin open, attached before it starts (so
early output is not lost), and talked to over the attach stream. Container
stdout/stderr logs are what the UI shows on the server page.

## Server sources

- `remote`: no container. MCP Machine connects to the URL with a Streamable
  HTTP client, an SSE client, or tries both (`transport: auto`).
- `command`: a generated container. `npx`, `node`, `uvx`, and `python` run in
  the matching image from `runtimes`, with a cache volume for npm or uv. A
  `docker run` line is parsed into image, env, volumes, mounts, ports, and
  args; `${VAR}` in its env values and args (and in `command.args`) is expanded
  when the container is resolved. Always stdio.
- `build`: the repository is cloned with go-git (shallow), the image is built
  through the Podman API as `localhost/mcp-machine/<name>:<sha7>`, and the
  container runs in stdio or http mode. An image that already exists for the
  resolved commit is reused unless you press Rebuild.

## Configuration flow

Configuration comes in three layers: the administrator's base file
`/etc/mcp-machine/config.yaml` and system drop-ins in
`/etc/mcp-machine/config.d/`, mounted read-only, and the user drop-ins in
`/data/config.d/` that the UI writes. `config.ResolveLayout` turns the `-c`,
`--system-config-d`, and `--config-d` flags into a `Layout` (falling back to
the deprecated `/data/config.yaml` when the admin root holds neither
`config.yaml` nor a drop-in);
`Layout.Load` reads every file in order and returns a `Set`: the merged,
defaulted, and expanded `Config`, the files loaded, and each server's `Origin`
(the files that define it, by layer), which the UI and `validate -v` show.
Admin definitions of a name deep-merge; a user definition replaces them. The
UI writes only through `config.DirWriter`, which validates the merged set with
the change in place and then writes or removes one `<name>.yaml` in the user
directory, atomically and serialized process-wide; it has no way to write the
administrator's files.

The watcher watches the base file's directory and both drop-in directories
(not the files), so editor renames and the UI's atomic writes trigger a reload.
A reload in which any file fails to read, parse, or validate keeps the last
good configuration and records the error, naming the file, which the Config
page shows. Each successful reload is handed to the manager, which reconciles
by name and spec hash. The UI reloads right after writing so the change is
applied before the browser is redirected; see [config.md](config.md).

## Origin and TLS

Origin validation applies to the MCP routes (`/servers/{name}/mcp` and
`/servers/{name}/sse`) using the standard library's `http.CrossOriginProtection`:
same-origin requests and requests with no `Origin` header (CLI clients) pass;
cross-origin requests pass only if the origin is in `allowed_origins` or equals
the `public_url` origin. The Go SDK's loopback DNS-rebinding protection stays
on. The UI's state-changing requests have their own same-origin check. Browser
requests (those with an `Origin` or `Sec-Fetch-Site` header) to the MCP routes,
and browser state-changing UI requests, also pass a host check
(`httpadapter.HostGuard`): the `Host` header must be an IP address,
`localhost`, the process's own host name, the `public_url` host, or an
`allowed_origins` host, which stops DNS rebinding on LAN addresses (a
rebinding page sends a matching `Origin`). Non-browser clients are not
checked. When
`tls.cert_file` and `tls.key_file` are both set, the server listens on TLS only.

## Operations endpoints

| Path | Meaning |
|---|---|
| `GET /healthz` | Liveness. Answers `ok`. |
| `GET /readyz` | Ready when the Podman engine answers a ping and the first configuration has been applied. |
| `GET /metrics` | Prometheus text: `mcp_machine_servers{state}`, `mcp_machine_proxy_calls_total{server}`, `mcp_machine_config_reloads_total`, `mcp_machine_config_reload_errors_total`. |

## Shutdown and restart

On SIGINT or SIGTERM the HTTP server shuts down gracefully (15 seconds, then
open streams are dropped) and the manager closes its backend sessions. Sibling
containers are left as they are. On the next start the manager adopts them
(see [state-machine.md](state-machine.md)).
