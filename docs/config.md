# Configuration

MCP Machine reads one YAML file, `/data/config.yaml` in the container. Pass
another path with `mcp-machine serve -c PATH` (and `mcp-machine validate -c PATH`).
The file is hand-editable, hot-reloaded, and also written back by the web UI.
Unknown keys are rejected. Every key is optional except what each server type
requires. A complete example is in [`config.example.yaml`](../config.example.yaml).

Check a file without starting anything:

```sh
mcp-machine validate -c config.yaml      # prints "ok" or every problem found
```

## Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen` | string | `:8080` | Address the HTTP server binds. The `--listen` flag of `serve` overrides it. |
| `public_url` | string | empty | Externally visible base URL, for example `http://mcp.lan:8080`. Must be absolute. Used in the connect snippets shown in the UI, and its origin is always an allowed origin. |
| `tls.cert_file`, `tls.key_file` | string | empty | PEM certificate and key. Both or neither; when both are set the server listens on TLS only. |
| `podman.socket` | string | `unix:///run/podman/podman.sock` | Podman API endpoint. Must start with `unix://` or `tcp://`. The environment variable `MCP_MACHINE_PODMAN_SOCKET`, when set and non-empty, overrides it. |
| `podman.network` | string | `mcp-machine` | User-defined network the managed containers join (created if missing). |
| `podman.stop_timeout` | duration | `10s` | Grace period when stopping a container. Minimum `1s`. Go duration syntax (`10s`, `1m`). |
| `workspace_dir` | string | `<data_dir>/workspace` | Where `build` servers are cloned. Must be an absolute path. |
| `data_dir` | string | `/data` | Data directory. |
| `allowed_origins` | list of strings | `[]` | Extra trusted origins for cross-origin requests to the MCP routes, each an absolute origin such as `http://mcp.lan:8080`. |
| `registries` | list | `[]` | Credentials for private image registries (below). |
| `runtimes` | map | see below | Image used for each `command.command` value. |
| `log.level` | string | `info` | `debug`, `info`, `warn`, or `error`. An unrecognized value falls back to `info`. |
| `log.format` | string | `text` | `text` or `json` (anything other than `json` logs text). |
| `servers` | list | `[]` | The managed servers (below). |

### registries

Credentials are matched against image references by the longest registry-host
prefix, and used for pulls and for the base images of `build` servers.

| Field | Type | Meaning |
|---|---|---|
| `registry` | string | Registry host, for example `ghcr.io`. |
| `username` | string | Username. `${VAR}` allowed. |
| `password` | string | Password or token. `${VAR}` allowed. |

### runtimes

Maps a `command.command` value to the image that runs it. The defaults are
always present and individual entries can be overridden or extended:

| Command | Default image |
|---|---|
| `npx` | `docker.io/library/node:22-bookworm-slim` |
| `node` | `docker.io/library/node:22-bookworm-slim` |
| `uvx` | `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` |
| `python` | `docker.io/library/python:3.12-slim` |

`npx` and `node` servers get a named volume `mcpm-cache-npm` at `/root/.npm`;
`uvx` and `python` servers get `mcpm-cache-uv` at `/root/.cache/uv`.

## Servers

Each entry in `servers` has a `name`, a `type`, and exactly one section that
matches the type.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | DNS label: lowercase letters, digits and `-`, 1-63 characters, starting and ending with a letter or digit, unique. `new` and `import` are reserved. The container is named `mcpm-<name>` and the routes are `/servers/<name>/...`. |
| `type` | string | required | `remote`, `command`, or `build`. |
| `enabled` | bool | `true` | `false` stops the server (see [state-machine.md](state-machine.md)). |
| `env` | map of strings | empty | Environment variables for the container (`command` and `build` servers). `${VAR}` allowed. |
| `remote` | section | | Required for `type: remote`, forbidden otherwise. |
| `command` | section | | Required for `type: command`, forbidden otherwise. |
| `build` | section | | Required for `type: build`, forbidden otherwise. |
| `serve` | section | `{mode: stdio}` | How a `build` container exposes MCP. Command servers always use stdio. |
| `container` | section | empty | Optional overrides on the generated container (`command` and `build` servers). |

### remote

A server MCP Machine only connects to. No container is created.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `url` | string | required | `http://` or `https://` endpoint. |
| `transport` | string | `auto` | `http` (Streamable HTTP), `sse`, or `auto` (try Streamable HTTP, fall back to SSE). |
| `headers` | map of strings | empty | Extra request headers, for example `Authorization: "Bearer ${GITHUB_TOKEN}"`. `${VAR}` allowed. |

### command

A container generated from a client-style command. Exactly one of `command` or
`run`.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `command` | string | | One of the keys of `runtimes` (`npx`, `node`, `uvx`, `python`, or one you add). |
| `args` | list of strings | empty | Arguments passed after the command. |
| `env` | map of strings | empty | Environment variables. `${VAR}` allowed. |
| `run` | string | | A `docker run` or `podman run` line. Supported: the image and its arguments, `-i`, `--rm`, `-e/--env` (`-e NAME` without a value passes MCP Machine's own variable through), `-v/--volume`, `-p/--publish`, `--name`, `--network`, `-w/--workdir`. `-t`, `--env-file`, and any other flag are rejected. Shell expansion other than plain `$NAME` is rejected. |

Command servers always speak MCP over the container's stdin and stdout.

### build

An image built from a git repository, then run as a container.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `repo` | string | required | Git URL (`https://...`, `ssh://...`, or `git@host:path`). |
| `ref` | string | the default branch | Branch, tag, or a full 40-character commit sha. |
| `subdir` | string | repo root | Build context directory inside the repository. |
| `containerfile` | string | `Containerfile` | Path of the Containerfile relative to `subdir`. |
| `args` | map of strings | empty | Build arguments. `${VAR}` allowed. |
| `auth.token` | string | | Token for an `https://` repository (sent as basic auth). `${VAR}` allowed. |
| `auth.ssh_key_file` | string | | Path to a private key for an SSH repository. Set only one of `token` and `ssh_key_file`. |

The image is tagged `localhost/mcp-machine/<name>:<sha7>`. The clone is redone
whenever the build step runs, so a moving `ref` such as `main` is re-resolved
then; the build itself is skipped when the tag already exists, and the Rebuild
action forces it.

### serve

| Field | Type | Default | Meaning |
|---|---|---|---|
| `mode` | string | `stdio` | `stdio` (MCP over stdin/stdout) or `http` (the container serves MCP over HTTP). |
| `port` | int | | Required when `mode` is `http`: the port inside the container. |
| `path` | string | `/mcp` | Endpoint path (http mode). |
| `transport` | string | `http` | `http` (Streamable HTTP) or `sse` (http mode). |

An http-mode container is probed (up to 60 seconds) until its endpoint answers,
then connected to over the shared network (or `127.0.0.1` when MCP Machine runs
as a bare binary).

### container

| Field | Type | Meaning |
|---|---|---|
| `image` | string | Replaces the runtime image of a `command` server. |
| `volumes` | list of strings | Bind mounts as `src:dst` or `src:dst:ro` (`rw` is accepted). The source is a path on the **Podman host**, not inside the MCP Machine container. |
| `env` | map of strings | Extra environment variables. `${VAR}` allowed. |

## Environment variable expansion

`${NAME}` (letters, digits, underscore; not starting with a digit) is replaced
from MCP Machine's own environment when the file is loaded. Only these fields
are expanded: `registries[].username` and `password`; each server's `env`;
`remote.headers`; `command.env`; `container.env`; `build.args`;
`build.auth.token`. Other fields (URLs, paths, `command.run`, `args`) are used
as written.

A `${NAME}` that is not set is an error that names the variable, and the file
is rejected (at startup, or on reload, which keeps the previous configuration).
Supply the variables with `podman run -e NAME`, an `environment:` block in
`compose.yaml`, or `EnvironmentFile=` in the Quadlet unit.

The UI never shows resolved secrets, and the forms keep `${VAR}` references
verbatim.

## Hot reload

MCP Machine watches the directory that holds the config file. A change (an
editor save, a rename, or a UI write) is debounced for about 250 ms, then the
file is loaded and validated. If it is valid it is applied: servers are added,
replaced when their spec hash changed, or removed (see
[state-machine.md](state-machine.md)). If it is invalid the previous
configuration stays in effect and the error appears on the Config page and in
the log. The Config page also has a Reload now button (`POST /config/reload`).

The config file must live in a **directory** that is bind-mounted at `/data`.
A bind mount of the single file, or a named volume, does not deliver reliable
change events and is not supported for hot reload.

Not everything is hot. `listen`, `tls`, `public_url`, `allowed_origins`,
`podman.socket`, `workspace_dir`, and `log` are read once at startup: restart
the container to change them. So are the `registries` credentials used for the
base images of `build` servers. Server definitions, `runtimes`,
`podman.stop_timeout`, and the registry credentials used for image pulls are
applied on reload; `podman.network` applies to containers created after the
reload.

## Write-back from the UI

Adding, editing, importing, and deleting servers in the UI edits the YAML file
in place. The file is processed as a `yaml.v3` node tree, so comments, key
order, quoting style, and `${VAR}` references elsewhere in the file survive.
A change is validated in memory first and written atomically (a temp file,
then a rename); the reload this triggers is harmless because reconciling is
hash-based.

An edit merges into the existing entry: keys you did not change keep their
comments and quoting. Be aware that the first UI save of a server may add keys
that the form fills with defaults (for example `transport: auto` on a remote
server, `containerfile: Containerfile` on a build server, or `serve: {mode:
stdio}`), which can change the spec hash and recreate that server's container
once.
