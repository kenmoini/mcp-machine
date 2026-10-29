# MCP Machine design

Date: 2026-10-27. Owner: Ken Moini. Status: approved 2026-10-27.

## Intent

Ken runs MCP servers in two places that both hurt today. The same `npx` and stdio servers get re-configured in every client (Claude Code, Cursor, and others) on every laptop. And there is no good way to run them once on a homelab box and point every client at that box. MCP Machine is a small self-hosted web app, shipped as one container, that talks to a mounted Podman socket, runs MCP servers as sibling containers, and proxies each one to clients over Streamable HTTP, legacy HTTP+SSE, and stdio. Servers can be defined from a remote endpoint, a client-config style command (`npx`, `node`, `uvx`, `docker run`), or a git repo with a Containerfile.

Success looks like: Ken edits one YAML file or clicks through one form on the homelab box, and every client on every laptop sees that server a few seconds later.

## Goals

- Connect to a volume-mounted Podman socket. Root socket first, rootless second.
- Manage the running state of MCP server containers: start, stop, restart, remove, rebuild, logs, status.
- Proxy every managed server to clients over all three adapters: Streamable HTTP, HTTP+SSE, stdio.
- Accept three kinds of server definition: remote endpoint, command (`npx`/`node`/`uvx`/`python`/`docker run`), git repo with Containerfile.
- Config is a YAML file on a volume, hand-editable, hot-reloaded. The UI can also add, edit, and delete servers by writing back to that file with comments and order preserved.
- Private container registries and private git repos work.
- Packaged as a container, launchable by `podman run`, Compose, or Quadlet. UBI9 base images.
- Plain HTTP and TLS both supported. No authentication yet.
- Makefile drives build, test, lint, image, deploy. Test driven development throughout.

## Non-goals (for v1)

- Authentication or multi-user access control.
- A unified `/mcp` endpoint aggregating all servers into one tool list (routed per-server endpoints only).
- Forwarding server-to-client features through the proxy: sampling, elicitation, progress, logging, resource subscriptions. Only tools, resources, resource templates, prompts, and list-changed notifications are forwarded.
- A secret store of its own. Secrets are `${VAR}` references resolved from MCP Machine's own environment, or, since the Vault integration (`2026-10-28-vault-secrets-design.md`), `${vault:PATH#KEY}` references read from HashiCorp Vault / OpenBao at start.
- Kubernetes, Docker Desktop, or Windows.

## Anti-goals

These would make it a failure even if it worked:
- Config that lives only in a database or is not hand-editable.
- A UI that needs a JavaScript build step or a Node toolchain to ship.
- Anything that hard-codes root-only paths or needs `--privileged`.
- A fat image. Builder is `ubi9/go-toolset`, runtime is `ubi9/ubi-minimal`.

## Constraints and research facts

Verified on 2026-10-27.

**MCP protocol.** The current spec revision is 2026-07-28 and it changed the wire protocol substantially: no `initialize` handshake, every request carries `_meta` with protocol version and client info, a new `server/discover` RPC, Streamable HTTP is POST-only (the GET stream, `Mcp-Session-Id`, and `Last-Event-ID` are gone), server-initiated requests became multi-round-trip results, and change notifications arrive through `subscriptions/listen`. Revisions 2025-11-25 and earlier are "legacy" (initialize plus sessions). HTTP+SSE from 2024-11-05 is deprecated but still used by older clients; a server supporting them hosts `/sse` plus a POST endpoint beside `/mcp`. Servers must validate the `Origin` header. Claude Code marks SSE deprecated and tries HTTP first, falling back to SSE.
Sources: https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http , https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning , https://code.claude.com/docs/en/mcp

**Go SDK.** `github.com/modelcontextprotocol/go-sdk` v1.8.0 (Sep 2026), requires Go 1.25. Its Streamable and SSE handlers reject requests that arrive on a loopback address with a non-loopback `Host` header (DNS rebinding protection) and accept an `http.CrossOriginProtection` from the standard library for Origin checks. Serves 2026-07-28 and every legacy revision. Serving modern clients over HTTP requires `StreamableHTTPOptions{Stateless: true}`; legacy clients still negotiate down on the same endpoint. `NewSSEHandler` remains for HTTP+SSE clients. The client side negotiates era automatically. Transports: `StreamableClientTransport`, `SSEClientTransport`, `CommandTransport`, `IOTransport` (any `io.ReadWriteCloser`), `StdioTransport`, in-memory pairs for tests. Low-level `Server.AddTool`, `AddResource`, `AddResourceTemplate`, `AddPrompt` take raw handlers with unvalidated `json.RawMessage` arguments, and `ClientOptions` exposes list-changed handlers. The SDK's examples contain no proxy; the facade is ours.
Sources: https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0 , https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0

**Podman.** Go bindings are `go.podman.io/podman/v6` (v6.1.4, Oct 2026), which require **Go 1.26**, so the project's Go floor is 1.26. Minimal server API is 4.0.0, so the client works against Podman 4.x, 5.x, and 6.x. `images.Build` needs a local context directory (it tars it over the socket), so repos are cloned first. `containers.Attach` gives a bidirectional hijacked stream to a container created with stdin open; attach must happen before start or early output is lost. Quadlet has `.container` and `.network` units; rootful units live in `/etc/containers/systemd/`, rootless in `~/.config/containers/systemd/`.
Source: https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html

**Images and libraries.** `registry.access.redhat.com/ubi9/go-toolset` has Go 1.26 tags. `ubi9/ubi-minimal:9.8` is the newest runtime tag. htmx 2.0.10 is the latest 2.x; htmx 4.0.0 (Aug 2026) renamed attributes and moved SSE to an extension, so we pin 2.0.10 and vendor the one JS file. `mcp-remote` 0.15.2 supports the new era and serves as a documented alternative stdio bridge.

**Dev box.** macOS, Go 1.27.1, Podman 5.7.1 via podman machine, `make` and `skopeo` present, no golangci-lint.

## Decisions (as Ken decided them)

- **Go**, single static binary, server-rendered UI with `html/template` and embedded htmx 2.x. No SPA.
- **YAML config**, hand-editable, hot-reloaded. No database.
- **UI adds, edits, and deletes servers** by writing back to the YAML.
- **Root Podman socket first, rootless later.** Socket path is config; nothing root-specific baked in.
- **UBI9 images.** Balanced UI, not spartan.
- **Private registries and private git repos in v1.**
- Module `github.com/kenmoini/mcp-machine`, image `ghcr.io/kenmoini/mcp-machine`.

## Design

### Architecture

```
clients (Claude Code, Cursor, mcp-machine connect, mcp-remote)
   │  Streamable HTTP  /servers/{name}/mcp     (2026-07-28 + legacy, Stateless)
   │  HTTP+SSE         /servers/{name}/sse     (2024-11-05 clients)
   ▼
┌──────────────────────── mcp-machine (one container) ─────────────────────────┐
│  web UI (html/template + htmx)   │  adapters: StreamableHTTPHandler, SSEHandler │
│  config watcher + writer (YAML)  │  proxy facade: mcp.Server per managed server │
│  manager / reconciler            │  backend session: mcp.ClientSession          │
│  runtime.Engine (podman bindings over the unix socket)                         │
└───────────────────────────────────────────────────────────────────────────────┘
   │ unix socket                      │ IOTransport over Attach   │ HTTP on network
   ▼                                  ▼                           ▼
 podman                 sibling container (stdio)      sibling container (http) / remote URL
```

**Proxy facade.** For each server there is one backend `mcp.ClientSession` (over `IOTransport` for container stdio, Streamable or SSE client transports for remote and http-mode containers) and one facade `mcp.Server` whose tools, resources, templates, and prompts mirror the backend and whose handlers forward the raw call. Lists re-sync on list-changed notifications and on reconnect. Both the Streamable handler (stateless) and the SSE handler serve the facade. The SDK handles every era combination between client and backend.

**stdio adapter.** `mcp-machine connect http://host:8080/servers/<name>/mcp` bridges `StdioTransport` to a Streamable client in the same binary. Clients add it with `claude mcp add <name> -- mcp-machine connect <url>`. The UI shows this snippet, the `claude mcp add --transport http` snippet, and the `npx mcp-remote` alternative.

**Runtime engine.** An interface (`internal/runtime.Engine`) with a Podman implementation and an in-memory fake. The heavy `containers/*` dependency tree stays behind one package built with `-tags remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper` and `CGO_ENABLED=0`. Unit tests never touch a socket. If the bindings prove unbuildable the fallback is a thin libpod REST client behind the same interface.

**Sibling containers.** Named `mcpm-<name>`, on a user-defined network `mcp-machine`, labeled `io.mcp-machine/managed=true`, `io.mcp-machine/server=<name>`, `io.mcp-machine/hash=<spec hash>`, `io.mcp-machine/transport=stdio|http`. On startup the manager adopts labeled containers: http-mode ones in place, stdio ones by restart (a running stdio container cannot be safely re-attached). On config change it diffs by name and spec hash.

**Sources.** Each YAML entry becomes an engine spec. `remote`: no container. `command`: `npx`/`node`/`uvx`/`python` run in a configurable runtime image with npm and uv cache volumes; a `docker run` line is parsed into image, env, volumes, ports, and args. `build`: go-git shallow clone into the workspace volume, `images.Build` tagged `localhost/mcp-machine/<name>:<sha7>`, cached by commit sha, then run as stdio or http.

**Config write-back.** The YAML is edited as a `yaml.v3` node tree so comments, order, and quoting survive. Changes are validated in memory, then written atomically (temp file plus rename). The watcher's reload after a write is harmless because reconcile is hash-based. Form fields keep `${VAR}` references verbatim; the UI never reveals resolved secrets.

**Credentials.** A top-level `registries:` list (host, username, password) is matched by longest host prefix against image references and passed to pull and build. Git credentials live under `build.auth` as `token` (HTTPS basic auth) or `ssh_key_file`.

**Origin validation** on the MCP routes only, using the standard library's `http.CrossOriginProtection` passed to the SDK handlers. Same-origin requests and requests with no Origin header (CLI clients) pass; cross-origin requests pass only if the exact origin is listed in `allowed_origins` or equals the `public_url` origin. The SDK's loopback DNS rebinding protection stays enabled.

**TLS.** `tls.cert_file` and `tls.key_file`; when both are set the server listens on TLS only.

### Config shape

```yaml
listen: ":8080"
public_url: "http://mcp.lan:8080"      # used in connect snippets + default allowed origin
tls: { cert_file: "", key_file: "" }   # both or neither
podman:
  socket: "unix:///run/podman/podman.sock"   # env MCP_MACHINE_PODMAN_SOCKET overrides
  network: mcp-machine
  stop_timeout: 10s
workspace_dir: /data/workspace         # git clones + build contexts
data_dir: /data                        # caches
allowed_origins: []                    # exact origins, e.g. "http://mcp.lan:8080"; public_url is always allowed
registries:
  - registry: ghcr.io
    username: "${GHCR_USER}"
    password: "${GHCR_TOKEN}"
runtimes:                              # images used for `command` servers
  npx:  docker.io/library/node:22-bookworm-slim
  node: docker.io/library/node:22-bookworm-slim
  uvx:  ghcr.io/astral-sh/uv:python3.12-bookworm-slim
  python: docker.io/library/python:3.12-slim
log: { level: info, format: text }

servers:
  - name: github-remote                # DNS label; container name becomes mcpm-<name>
    type: remote
    remote:
      url: https://api.githubcopilot.com/mcp/
      transport: http                  # http | sse | auto
      headers: { Authorization: "Bearer ${GITHUB_TOKEN}" }

  - name: filesystem
    type: command
    command:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/projects"]
      env: { LOG_LEVEL: debug }
    container:                         # optional overrides on the generated spec
      volumes: ["/srv/projects:/projects:ro"]
      image: docker.io/library/node:20

  - name: fetch
    type: command
    command:
      run: "docker run -i --rm -e FOO=bar ghcr.io/example/fetch-mcp:latest --verbose"

  - name: my-server
    type: build
    build:
      repo: https://github.com/example/my-mcp-server.git
      ref: main
      subdir: server
      containerfile: Containerfile
      args: { NODE_ENV: production }
      auth: { token: "${GITHUB_TOKEN}" }   # or ssh_key_file: /data/keys/id_ed25519
    serve: { mode: http, port: 3000, path: /mcp, transport: http }   # mode stdio (default) | http
    env: { PORT: "3000" }
    enabled: true
```

Validation: unique names matching `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`; exactly one of `remote|command|build` matching `type`; `command.command` XOR `command.run`; `serve.mode: http` requires `port`; TLS both-or-none; socket scheme `unix://` or `tcp://`; unknown keys rejected. Spec hash = sha256 of canonical JSON of the entry plus the runtime image it depends on.

### Routes

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | dashboard: server table with state badges |
| GET | `/servers/{name}` | detail: status, tools/resources/prompts, logs, connect snippets |
| GET | `/servers/{name}/status`, `/logs?tail=`, `/tools` | htmx partials polled every 2s |
| POST | `/servers/{name}/actions/{start,stop,restart,remove,rebuild,resync}` | returns row partial |
| GET / POST | `/servers/new?type=remote\|command\|build` | add form and submit; writes YAML |
| GET / POST | `/servers/{name}/edit` | edit form and submit; writes YAML |
| POST | `/servers/{name}/delete` | removes the entry; reconcile removes the container |
| POST | `/servers/import` | paste `mcpServers` JSON or a `docker run` line, preview, add |
| GET / POST | `/config`, `/config/reload` | current config, last reload error, force reload |
| GET | `/api/servers` | JSON list |
| GET | `/static/{file...}` | embedded htmx and css |
| POST | `/servers/{name}/mcp` | Streamable HTTP adapter (stateless; GET/DELETE answer 405) |
| GET, POST | `/servers/{name}/sse` | legacy HTTP+SSE adapter |
| GET | `/healthz`, `/readyz`, `/metrics` | liveness, engine ping + config loaded, Prometheus text |

### Lifecycle

```
Defined → Resolving → [Building] → ImageReady → Creating → Running → Connecting → Ready
                                                                       ↘ Stopped
Any → Failed{err}  (backoff 1s,2s,4s…60s, then sticky until manual Start)
Any → Removing → gone
remote type: Defined → Resolving → Connecting → Ready   (no container states)
```

Creating for stdio = attach then start, with a goroutine waiting on exit. Creating for http = start then probe the endpoint for up to 60s. Session closed or container exit from Ready → Failed → retry (stop, remove, re-create after backoff). Stop keeps the container for fast restart. `enabled: false` means Stop. A UI Stop sets a paused flag that survives reloads until the spec hash changes.

Reconcile on every apply (startup and each hot reload, serialized on one goroutine): build the desired name-to-hash map; adopt labeled containers with a matching hash; stop and remove containers that are absent from desired or carry a stale hash; start missing runners; an invalid config keeps the last good one and records the error for the UI.

### Packaging

Multi-stage Containerfile (`ubi9/go-toolset:1.26` → `ubi9/ubi-minimal:9.8`), `VOLUME /data`, `EXPOSE 8080`, entrypoint `mcp-machine serve -c /data/config.yaml`. `compose.yaml` and a Quadlet `.container` plus `.network` unit mount the socket, bind-mount a host directory at `/data`, and set `security_opt: label=disable`. Makefile targets: build, test, test-integration, lint, fmt, run, validate, image, push, compose-up/down, quadlet-install, htmx-vendor, clean.

## Calls I made (check these while reading)

1. **Sibling networking.** MCP Machine detects it is in a container via `/run/.containerenv`, reads its own container ID, and joins the `mcp-machine` network itself, so plain `podman run` needs no `--network` flag. When running as a bare binary (dev on the Mac), http-mode siblings publish their port on `127.0.0.1:0` and the endpoint dials that port.
2. **Config location.** Deployment bind-mounts a host directory at `/data`; config is `/data/config.yaml`. The watcher watches the directory so editor renames and UI write-back both trigger reloads. Single-file bind mounts and named volumes are documented as unsupported for hot reload.
3. **Proxy core is the SDK**, not hand-rolled JSON-RPC forwarding, because of the era split in the protocol.
4. **Routed endpoints only** (`/servers/{name}/...`), no aggregated endpoint.
5. **Build caching by commit sha**; `ref: main` is re-resolved only on Rebuild.
6. **Registry and git credentials** are config, not a secret store, and only ever via `${VAR}` or `${vault:PATH#KEY}` (see `2026-10-28-vault-secrets-design.md`).
7. **htmx 2.0.10** rather than 4.0.0.

## Left to the builder

- Exact Go field names, package internals, and error wording.
- Whether registry auth reaches the build endpoint via `BuildOptions.SystemContext` or another route (confirm against the bindings early).
- Backoff numbers, probe timeouts, and debounce intervals beyond the defaults above.
- UI layout and styling within "balanced": readable tables, status badges, forms per server type, no framework.
- Handling of a missing `${VAR}` (recommended: validation error naming the variable).
- Cross-platform distribution of the `connect` binary and TLS trust for self-signed certs (document, do not solve).
