# MCP Machine

[![CI](https://github.com/kenmoini/mcp-machine/actions/workflows/ci.yaml/badge.svg)](https://github.com/kenmoini/mcp-machine/actions/workflows/ci.yaml)

MCP Machine is a small self-hosted web app, shipped as one container, that runs your MCP servers as sibling Podman containers and proxies each one to clients over Streamable HTTP, legacy HTTP+SSE, and stdio. You describe servers in hand-editable YAML files (or through the web UI): a remote endpoint, a client-style command such as `npx`, `uvx`, or `docker run`, or a git repository with a Containerfile. Configure a server once on a homelab box and every client on every laptop can use it a few seconds later.

## Status

Version 1. There is **no authentication**: anyone who can reach the port can use every server and edit the configuration. Run it on a trusted network or behind something that authenticates. See [Limitations](#limitations).

## Quick start

MCP Machine talks to the Podman API socket. It reads the administrator's configuration from `/etc/mcp-machine` (mounted read-only) and keeps its state, including the servers you add in the web UI, in `/data`. Enable the socket, create both directories, and write a minimal config:

```sh
sudo systemctl enable --now podman.socket
sudo mkdir -p /etc/mcp-machine /srv/mcp-machine /srv/projects
sudo tee /etc/mcp-machine/config.yaml >/dev/null <<'YAML'
servers:
  - name: filesystem
    type: command
    command:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/projects"]
    container:
      volumes: ["/srv/projects:/projects:ro"]
YAML
```

### podman run

```sh
sudo podman run -d --name mcp-machine \
  -p 8080:8080 \
  -v /run/podman/podman.sock:/run/podman/podman.sock \
  -v /etc/mcp-machine:/etc/mcp-machine:ro \
  -v /srv/mcp-machine:/data \
  --security-opt label=disable \
  ghcr.io/kenmoini/mcp-machine:latest
```

Open <http://localhost:8080>. The sibling network (`mcp-machine`) is joined automatically, so no `--network` flag is needed. The `/etc/mcp-machine` mount is optional: without it MCP Machine falls back to the old location `/data/config.yaml` with a deprecation warning (see [Upgrading from `/data/config.yaml`](docs/config.md#upgrading-from-dataconfigyaml)), or, if that is absent too, starts with no servers and a "no configuration found" warning, and you add servers in the UI. Pass secrets that the config references as `${VAR}` with `-e NAME` (see [docs/config.md](docs/config.md)). If you browse to MCP Machine by a DNS name rather than an IP address or `localhost`, set `public_url` to that address (see [Connecting clients](#connecting-clients)).

On an SELinux host, a host path mounted into a server container (like `/srv/projects` above) must be labeled for containers: write the volume as `/srv/projects:/projects:ro,z` (or `:Z` for a path only that server uses). MCP Machine's own container runs with `label=disable`, which does not extend to the containers it starts. See [Volumes](docs/config.md#volumes).

### Compose

[`compose.yaml`](compose.yaml) is the same thing with directories next to it: `./config` is mounted read-only at `/etc/mcp-machine` and `./data` at `/data`. Create both (`mkdir -p config data`), put your config in `./config/config.yaml` (optional drop-ins go in `./config/config.d/`), then:

```sh
make compose-up      # podman compose up -d
make compose-down
```

### Quadlet (systemd)

[`deploy/quadlet/`](deploy/quadlet) holds a `.container` and a `.network` unit. The default is rootful. The unit mounts `/etc/mcp-machine` read-only and `/srv/mcp-machine` at `/data`, as above; both directories must exist (`/etc/mcp-machine` may be empty). Put your config in `/etc/mcp-machine/config.yaml`, then:

```sh
sudo make quadlet-install      # copies the units to /etc/containers/systemd and reloads systemd
sudo systemctl start mcp-machine
```

For a rootless setup, install into your user directory and edit the socket volume in the installed `mcp-machine.container` to `Volume=%t/podman/podman.sock:/run/podman/podman.sock`, then enable the user socket and start the unit. Enable lingering for the user so the user manager (and with it the socket, MCP Machine and its servers) starts at boot and keeps running after you log out:

```sh
make quadlet-install QUADLET_DIR=$HOME/.config/containers/systemd SYSTEMCTL="systemctl --user"
# edit the Volume= line for the socket in $HOME/.config/containers/systemd/mcp-machine.container
sudo loginctl enable-linger "$USER"
systemctl --user enable --now podman.socket
systemctl --user daemon-reload
systemctl --user start mcp-machine
```

(The `Install` section is `WantedBy=multi-user.target default.target`, so the unit also starts at boot for the system or user manager.)

## Server types

Each entry in `servers:` has a `type`. All fields are in [docs/config.md](docs/config.md).

**`remote`**: MCP Machine connects to an existing HTTP or SSE endpoint. No container.

```yaml
- name: github
  type: remote
  remote:
    url: https://api.githubcopilot.com/mcp/
    transport: http            # http | sse | auto (default)
    headers: { Authorization: "Bearer ${GITHUB_TOKEN}" }
```

**`command`**: a client-style command, run in a container and spoken to over stdio. `npx`, `node`, `uvx`, and `python` use the images in `runtimes`; or give a whole `docker run` line.

```yaml
- name: filesystem
  type: command
  command:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/projects"]
  container:
    volumes: ["/srv/projects:/projects:ro"]

- name: fetch
  type: command
  command:
    run: "docker run -i --rm -e FOO=bar ghcr.io/example/fetch-mcp:latest --verbose"
```

**`build`**: clone a git repository, build its Containerfile, and run the result over stdio or HTTP.

```yaml
- name: my-server
  type: build
  build:
    repo: https://github.com/example/my-mcp-server.git
    ref: main
    auth: { token: "${GITHUB_TOKEN}" }
  serve: { mode: http, port: 3000, path: /mcp }
```

## Connecting clients

Each server is served at `http://HOST:8080/servers/NAME/mcp` (Streamable HTTP) and `http://HOST:8080/servers/NAME/sse` (legacy SSE):

```sh
claude mcp add --transport http filesystem http://mcp.lan:8080/servers/filesystem/mcp
```

For stdio-only clients use `mcp-machine connect <url>` or `npx mcp-remote <url>`. Details, including origin settings for browser-based clients, are in [docs/clients.md](docs/clients.md).

To block DNS-rebinding attacks from web pages, MCP Machine only answers browser requests (those carrying an `Origin` or `Sec-Fetch-Site` header) to the MCP routes, and browser UI changes, when they are addressed to an IP address, `localhost`, its own host name, the host of `public_url`, or a host in `allowed_origins`. **Browser requests to a DNS name such as `mcp.lan` must match `public_url` (`public_url: "http://mcp.lan:8080"`) or `allowed_origins`**, otherwise they get `403 host not allowed`. Non-browser clients (Claude Code, SDKs, `curl`, `mcp-machine connect`, other containers) are unaffected.

## The web UI

- **Dashboard** (`/`): every server with a state badge, type, image, time in state, restart count, and Start, Stop, Restart, Resync, Remove, and (for build servers) Rebuild actions. The table refreshes every two seconds, so servers added or removed elsewhere show up; it pauses while an action error is shown (click the error to dismiss it).
- **Server page** (`/servers/NAME`): status, where the server is defined, the tools, resources, and prompts the server exposes, live container logs, and copy-ready connect commands.
- **Add, edit, delete, import**: forms for each server type write the server to its own file, `/data/config.d/<name>.yaml`, and apply it at once. Editing a server from the administrator's files writes an override there; such a server can be reverted to the administrator's definition but not deleted. **Import** takes pasted `mcpServers` client JSON or a `docker run` line, shows a preview, and adds the servers; a client entry whose command is `docker run ...` or `podman run ...` becomes a `command.run` line. If `/data/config.d` is not writable, editing is disabled and every page says so.
- **Config** (`/config`): the loaded settings, the files loaded and where each server is defined, the last reload error if any, and a Reload now button.

`mcp-machine import <file|->` prints the same client-JSON conversion as YAML on the command line, and `mcp-machine validate -c /etc/mcp-machine -v` checks the configuration and lists the files read and where each server is defined. `mcp-machine help` (or `-h`, `--help`) lists the commands.

## Configuration

YAML in three places, merged and hot-reloaded when any of them changes:

| Location | Holds | Written by |
|---|---|---|
| `/etc/mcp-machine/config.yaml` | Global keys and servers (the base file) | The administrator only; mount it read-only |
| `/etc/mcp-machine/config.d/*.yaml` | Servers only (system drop-ins) | The administrator only |
| `/data/config.d/*.yaml` | Servers only, one per `<name>.yaml` (user drop-ins) | The web UI |

Later administrator files refine a server field by field; a file in `/data/config.d` replaces the administrator's definition of that server. Secrets are `${VAR}` references resolved from MCP Machine's own environment, and the UI keeps them as placeholders in what it writes. An existing single `/data/config.yaml` from an earlier version keeps working as a fallback, with a deprecation warning, until you move it by hand as described in [Upgrading from `/data/config.yaml`](docs/config.md#upgrading-from-dataconfigyaml). Layers, top-level keys, every server field, defaults, reload and UI write behavior are in [docs/config.md](docs/config.md); [`config.example.yaml`](config.example.yaml) is a full base file (it references `GHCR_USER`, `GHCR_TOKEN`, and `GITHUB_TOKEN`, which must be set for it to validate) and [`config.example.d/`](config.example.d) holds a drop-in. Design notes are in [docs/architecture.md](docs/architecture.md) and server states in [docs/state-machine.md](docs/state-machine.md).

## Makefile targets

| Target | What it does |
|---|---|
| `make build` | Static binary at `bin/mcp-machine` (`CGO_ENABLED=0`, with the `GOTAGS` build tags). |
| `make test` | Unit tests. |
| `make test-race` | Unit tests with the race detector. |
| `make test-integration` | Tests against a real Podman socket (needs `MCP_MACHINE_PODMAN_SOCKET`). |
| `make vet` | `go vet`. |
| `make lint` | `golangci-lint run` using `.golangci.yml` (install with `make tools`). |
| `make vuln` | `govulncheck` over reachable code. |
| `make cover` | Coverage profile `coverage.out`, a summary, and `coverage.html`. |
| `make check` | `fmt-check`, `lint`, `vuln`, and `test` in one go. |
| `make tools` | Install the pinned `golangci-lint` and `govulncheck`. |
| `make fmt`, `make fmt-check` | Format with gofmt, or fail if anything needs formatting. |
| `make run` | Build and run `serve -c $(CONFIG) --config-d $(CONFIG_D)` (defaults `/etc/mcp-machine` and `/data/config.d`). For a local run: `make run CONFIG=config.example.yaml CONFIG_D=./data/config.d` (with `GHCR_USER`, `GHCR_TOKEN`, and `GITHUB_TOKEN` set, since the example references them). |
| `make validate` | Build and run `validate` with the same `CONFIG` and `CONFIG_D`. |
| `make image` | Build `$(IMAGE):$(TAG)` with `$(ENGINE)` from the `Containerfile` and also tag it `$(IMAGE):latest`, the tag `compose.yaml`, the Quadlet unit and the commands above run (defaults: `ghcr.io/kenmoini/mcp-machine`, `podman`, the git description). |
| `make push` | Build and push `$(IMAGE):$(TAG)` (not `:latest`). |
| `make compose-up`, `make compose-down` | Start or stop the Compose deployment. |
| `make quadlet-install` | Copy the Quadlet units to `$(QUADLET_DIR)` (default `/etc/containers/systemd`) and run `$(SYSTEMCTL) daemon-reload`. |
| `make htmx-vendor` | Re-download and checksum-verify the vendored htmx 2.0.10. |
| `make clean` | Remove `bin/` and the coverage files. |

## Development

Go 1.26 or newer is required.

```sh
make tools   # one time: installs golangci-lint and govulncheck
make check   # format check, lint, vulnerability scan, tests
make test-race
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the make targets, commit convention, and pull request checklist.

Unit tests never touch a socket; they use an in-memory fake engine. Integration tests run against a real Podman and are skipped unless a socket is given:

```sh
MCP_MACHINE_PODMAN_SOCKET=unix:///run/podman/podman.sock make test-integration
```

With `podman machine` (macOS), use the socket path printed by `podman machine inspect`. The config watcher may not see edits made on the macOS side of a `podman machine` bind mount (virtiofs does not always deliver inotify events); press **Reload now** on the Config page after editing a config file. The UI is `html/template` with one vendored htmx file; there is no JavaScript build step.

## Limitations

- No authentication or multi-user access control.
- No unified `/mcp` endpoint that merges all servers; endpoints are per server.
- Only tools, resources, resource templates, prompts, and list-changed notifications are forwarded. Sampling, elicitation, progress, logging, and resource subscriptions are not.
- No secret store. Secrets are `${VAR}` references resolved from MCP Machine's own environment.
- Podman only (4.x, 5.x, 6.x API). No Kubernetes, Docker Desktop, or Windows.
- A stdio container cannot be safely re-attached, so stdio containers are restarted when MCP Machine restarts. http-mode containers are adopted in place.
- Host paths in `container.volumes` and in `docker run -v` lines resolve on the Podman host, not inside the MCP Machine container.
- Hot reload needs the config files inside bind-mounted directories (`/etc/mcp-machine` and `/data`); single-file mounts and named volumes are not supported for reload. Under `podman machine` (macOS) file changes made on the host may not be noticed; use Reload now.
- On SELinux hosts, host paths mounted into server containers need `:z`/`:Z` (or a container label) to be readable there.
- Browser requests to the MCP routes and browser UI changes accept only IP addresses, `localhost`, MCP Machine's own host name, and the `public_url`/`allowed_origins` hosts in the `Host` header; set `public_url` when browsing by a DNS name. Non-browser clients are unaffected.

## License

MIT License. See [LICENSE](LICENSE).
