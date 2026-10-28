# Build recap: MCP Machine v1

Date: 2026-10-28. Branch: `main` at `6b9ae9c` (44 commits). Owner: Ken Moini.

## What was built

MCP Machine is a self-hosted Go web app, shipped as one container, that talks to a mounted Podman socket, runs MCP servers as sibling containers, and proxies each one to clients over Streamable HTTP (2026-07-28 and legacy revisions), legacy HTTP+SSE, and stdio (the `mcp-machine connect` bridge). Servers are defined in a hot-reloaded YAML file as one of three types: a remote endpoint, a command (`npx`, `node`, `uvx`, `python`, or a `docker run` line), or a git repo with a Containerfile. The htmx UI shows status, tools, logs, and connect snippets, and can add, edit, delete, and import servers by writing back to the YAML with comments preserved.

The design lives in `docs/superpowers/specs/2026-10-27-mcp-machine-design.md` and the implementation plan in `docs/superpowers/plans/2026-10-27-mcp-machine.md`.

## How it was built

All 32 plan tasks were implemented test-first by a fresh subagent each, reviewed by a fresh reviewer each, with fix rounds where reviews found Important issues. A final whole-branch review produced one Critical and ten Important findings; all were fixed in one wave and re-reviewed clean. One residual was fixed by the controller directly (see "Rulings" item 23).

## Verification on the final tree

Run outside the sandbox on this Mac:

- `make lint` (vet and staticcheck) clean; `make fmt-check` clean.
- `make test`: all 20 packages pass, including the listener-based tests for the adapters, the stdio bridge, the manager's http-mode path, and the `serve` wiring.
- `go test -race` on the manager, web, config, adapters, sources, and runtime packages: no races.
- `make test-integration` against the rootful podman machine socket: pass (ping, network, pull, attach/echo round trip, logs, stop, remove, build, bad-credential pull, network connect).
- `make image TAG=dev` builds from `ubi9/go-toolset:1.26` to `ubi9/ubi-minimal:9.8`; `mcp-machine version` inside the image reports the git describe, commit, and date.
- Container smoke test with one `npx` filesystem server: `/healthz` ok, server reached `ready` in a few seconds, `/readyz` 200, a 2026-07-28 `tools/list` through `/servers/filesystem/mcp` returned the backend's tools, the dashboard shows the ready badge, the detail page renders the `claude mcp add` snippet, `/servers/filesystem/sse` emits an `endpoint` event, `bin/mcp-machine connect` answers `tools/list` on stdio, a manual reload reconciled a newly added remote server to ready, `/metrics` reports both servers and the reload, the MCP Machine container joined the `mcp-machine` network without `--privileged`, and the host guard refused a browser-style POST from an unlisted host while a plain client passed.

Not verified: the Quadlet units on a Fedora or RHEL host, rootless Podman, and SELinux behavior on a real enforcing host.

## Environment notes for future work

- The sandbox blocks local port binding, the race detector, Go module downloads, and git. Use `export GOCACHE=$PWD/.gocache STATICCHECK_CACHE=$PWD/.gocache/staticcheck GOPROXY=off GOFLAGS=-mod=mod -buildvcs=false` for builds and tests inside the sandbox, and run listener tests, `-race`, `go mod tidy`, git, and `make image` outside it. Setting `sandbox.network.allowLocalBinding: true` in Claude Code settings lifts the port restriction.
- The podman machine is rootful; `MCP_MACHINE_PODMAN_SOCKET=unix://$HOME/.local/share/containers/podman/machine/podman.sock` runs the integration tests.
- inotify events do not cross the macOS virtiofs mount, so the config watcher will not see host-side edits on this Mac; use the UI's Reload button or `POST /config/reload`.

## Follow-ups for the owner

- `.github/dependabot.yml` has a docker ecosystem entry with `directory: "/agent"`; the Containerfile is at the repo root.
- The repo has no remote configured.
- Known deferred items are tracked in `docs/Parked.md`.

## Rulings made during the build

Each ruling is a decision taken on the owner's behalf, with what it costs if wrong.

1. The Makefile `test` target runs without `CGO_ENABLED=0` and without `-race` (a separate `test-race` target exists) because the race detector cannot run in the sandbox. Cost: races surface in `make test-race` rather than per task.
2. The `serve` wiring test uses a remote server against an httptest MCP server instead of a command server. Cost: stdio reconcile is covered only by manager unit tests.
3. The "only proxy, adapters, and manager import the SDK" constraint applies to direct imports; templates may read `proxy.Snapshot` values. Cost: none functional.
4. Because dispatches that bypass the sandbox are denied, the controller pre-pinned all dependencies and committed every task itself. Cost: commits carry the controller's identity.
5. `Serve{Mode: stdio}` defaults exist only for command and build servers; runtime image defaults merge per key; an empty `MCP_MACHINE_PODMAN_SOCKET` counts as unset. Cost: none expected.
6. Added validation: `podman.stop_timeout` must be at least 1s. Cost: sub-second timeouts are rejected.
7. The empty-file check lives in `config.Load` with a single read. Cost: an empty config file is an error everywhere.
8. YAML updates merge into the existing entry node rather than replacing it, overriding the plan, so in-entry comments survive UI edits. Cost: writer complexity.
9. The fake engine's re-attach after exit, lock-free progress writes, and exit-code snapshots were fixed early because the manager depends on them. Cost: a larger fake.
10. The Podman attach uses a dedicated connection per attachment; `Close` releases the stdout read only once the container stops or writes. Cost: a quiet, still-running container whose session closes without a stop holds a goroutine until it exits.
11. `ContainerInfo` gained `Networks` so the integration test asserts network membership. Cost: one field.
12. Facade capabilities are advertised via `ServerOptions.Capabilities` because `HasTools` and friends are deprecated in SDK v1.8.0. Cost: none expected.
13. Tool arguments are forwarded byte for byte; the client's `_meta` is not forwarded; backend definitions the SDK would panic on are normalized or skipped. Cost: progress tokens do not propagate (a stated non-goal).
14. The controller runs all listener-based tests outside the sandbox before each review. Cost: implementers verify such logic with recorders only.
15. The shutdown forced-close and TLS preload were carried from the first `serve` task into the manager wiring task. Cost: none now that both landed.
16. The composite resolver lives in `internal/sources/resolver` to avoid an import cycle. Cost: one small package.
17. Build-time registry credentials are derived from the Containerfile's first real `FROM` after cloning. Cost: multi-stage builds get credentials only for the first base image.
18. The retry counter resets after 60 seconds of Ready as well as on manual Start. Cost: a crash loop still exhausts ten retries.
19. `make quadlet-install` targets the rootful location by default; the rootless variant is documented. Cost: `sudo` is needed.
20. SELinux `:z`/`:Z` support and its docs were folded into the final fix wave. Cost: none.
21. The DNS-rebinding guard is a Host allowlist applied to browser requests on UI changes and MCP routes; no authentication was added. Cost: browser requests via an unlisted DNS name get 403 until `public_url` is set.
22. A set of minors was parked for post-merge; see `docs/Parked.md`.
23. One residual from the final re-review was fixed by the controller directly: browser detection uses only `Origin` and `Sec-Fetch-Site`, because Node's `fetch` sends `Sec-Fetch-Mode: cors` and `mcp-remote` would have been refused. Cost: a browser that omits `Sec-Fetch-Site` (none today) could bypass the guard.
24. The MIT `LICENSE` and the `.github/` files appeared in the tree outside any dispatch and were treated as the owner's own changes and kept; the README and image label were aligned to MIT. Cost: two lines to revert if that was not intended.
