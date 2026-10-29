# Configuration

MCP Machine reads YAML from three places in the container, merged into one
configuration (see [Configuration layers](#configuration-layers)):

- `/etc/mcp-machine/config.yaml`, the administrator's base file: global keys
  and servers;
- `/etc/mcp-machine/config.d/*.yaml`, the administrator's drop-ins: servers only;
- `/data/config.d/*.yaml`, the drop-ins the web UI writes: one server per file.

MCP Machine never writes the first two. All of them are hand-editable and
hot-reloaded. Unknown keys are rejected. Every key is optional except what each
server type requires. A complete base file is in
[`config.example.yaml`](../config.example.yaml) and a drop-in in
[`config.example.d/filesystem.yaml`](../config.example.d/filesystem.yaml).

Check the configuration without starting anything:

```sh
mcp-machine validate -c /etc/mcp-machine -v   # prints "ok" or the problems found; -v also lists the files and where each server is defined
mcp-machine validate -c config.yaml           # a single base file (the user drop-ins in /data/config.d are still read)
```

## Top-level keys

Every key except `servers` is global and may be set only in the base file.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen` | string | `:8080` | Address the HTTP server binds. The `--listen` flag of `serve` overrides it. |
| `public_url` | string | empty | Externally visible base URL, for example `http://mcp.lan:8080`. Must be absolute. Used in the connect snippets shown in the UI, and its origin is always an allowed origin. **Required when browsers reach MCP Machine by a DNS name**: browser requests to the MCP routes and the UI's state-changing requests must name a `Host` that is an IP address, `localhost`, MCP Machine's own host name, the `public_url` host, or an `allowed_origins` host (see [Host check](#host-check)). Non-browser clients are unaffected. |
| `tls.cert_file`, `tls.key_file` | string | empty | PEM certificate and key. Both or neither; when both are set the server listens on TLS only. |
| `podman.socket` | string | `unix:///run/podman/podman.sock` | Podman API endpoint. Must start with `unix://` or `tcp://`. The environment variable `MCP_MACHINE_PODMAN_SOCKET`, when set and non-empty, overrides it. |
| `podman.network` | string | `mcp-machine` | User-defined network the managed containers join (created if missing). |
| `podman.stop_timeout` | duration | `10s` | Grace period when stopping a container. Minimum `1s`. Go duration syntax (`10s`, `1m`). |
| `workspace_dir` | string | `<data_dir>/workspace` | Where `build` servers are cloned. Must be an absolute path. |
| `data_dir` | string | `/data` | Data directory. |
| `allowed_origins` | list of strings | `[]` | Extra trusted origins for cross-origin requests to the MCP routes, each an absolute origin such as `http://mcp.lan:8080`. Their host names are also accepted by the [host check](#host-check). |
| `registries` | list | `[]` | Credentials for private image registries (below). |
| `vault` | section | unset | How to reach HashiCorp Vault or OpenBao for `${vault:PATH#KEY}` references (below). Read at startup only. |
| `runtimes` | map | see below | Image used for each `command.command` value. |
| `log.level` | string | `info` | `debug`, `info`, `warn`, or `error`. An unrecognized value falls back to `info`. |
| `log.format` | string | `text` | `text` or `json` (anything other than `json` logs text). |
| `servers` | list | `[]` | The managed servers (below). The only key a drop-in may hold. |

### registries

Credentials are matched against image references by the longest registry-host
prefix, and used for pulls and for the base images of `build` servers.

| Field | Type | Meaning |
|---|---|---|
| `registry` | string | Registry host, for example `ghcr.io`. |
| `username` | string | Username. `${VAR}` allowed. |
| `password` | string | Password or token. `${VAR}` allowed. |

### vault

Where `${vault:PATH#KEY}` references are read from (see [Secrets from
Vault](#secrets-from-vault)). The section is read at startup only, like
`podman.socket`; changing it needs a restart. Exactly one way to
authenticate must be given: `token`, `token_file`, or `approle`.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `address` | string | empty | Vault's base URL, for example `https://vault.lan:8200`. Must be an absolute `http://` or `https://` URL. Required for any `${vault:...}` reference. `${VAR}` allowed. |
| `namespace` | string | empty | Vault Enterprise or OpenBao namespace, sent as `X-Vault-Namespace`. |
| `ca_file` | string | empty | PEM file (inside the MCP Machine container) with the CA that signed Vault's certificate. |
| `tls_skip_verify` | bool | `false` | Skip verifying Vault's certificate. Not with `ca_file`. |
| `token` | string | empty | A Vault token. `${VAR}` allowed, so `"${VAULT_TOKEN}"` reads it from the environment. |
| `token_file` | string | empty | A file holding a Vault token, for example a Vault Agent auto-auth sink. |
| `approle.role_id`, `approle.role_id_file` | string | empty | The AppRole's role ID, inline or in a file. One of the two. |
| `approle.secret_id`, `approle.secret_id_file` | string | empty | The AppRole's secret ID, inline or in a file. One of the two. |
| `timeout` | duration | `10s` | Per-request timeout. Minimum `1s`. |

The Vault CLI's environment variables override the file when set and
non-empty: `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_NAMESPACE`, `VAULT_CACERT`, and
`VAULT_SKIP_VERIFY` (`true`/`false`). With them a base file needs no `vault`
section at all: `-e VAULT_ADDR=... -e VAULT_TOKEN=...` on the MCP Machine
container is enough.

> For more information on the Vault integration, read [secrets-from-vault.md](./secrets-from-vault.md)

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
| `command` | string | | One of the keys of `runtimes` (`npx`, `node`, `uvx`, `python`, or one you add). Any other value is a validation error (`command.command "docker" is not a configured runtime`); to run an image directly, use `run`. |
| `args` | list of strings | empty | Arguments passed after the command. `${VAR}` allowed. |
| `env` | map of strings | empty | Environment variables. `${VAR}` allowed. |
| `run` | string | | A `docker run` or `podman run` line. Supported: the image and its arguments, `-i`, `--rm`, `--init` (accepted and ignored), `-e/--env`, `-v/--volume` (see [Volumes](#volumes)), `--mount` (`type=bind` or `type=volume`, with `source`/`src`, `target`/`destination`/`dst`, `readonly`/`ro`, and `relabel=shared\|private`), `-p/--publish`, `--name`, `--network`, `-w/--workdir`. `-t`, `--env-file`, and any other flag are rejected. Shell expansion other than plain `$NAME` / `${NAME}` is rejected. `${VAR}` in `-e` values and in the image arguments is expanded (see below). `-e NAME` without a value takes `NAME` from `command.env`, then the server's `env`, then MCP Machine's own environment. |

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
| `auth.ssh_key_file` | string | | Path (inside the MCP Machine container) to a private key for an SSH repository. Set only one of `token` and `ssh_key_file`. |
| `auth.known_hosts_file` | string | | Path (inside the MCP Machine container) to an OpenSSH `known_hosts` file used to verify the SSH server's host key. Only with `ssh_key_file`. |

The MCP Machine image ships no `known_hosts`, so an SSH build needs one: mount
a file that holds the host's key (for example the output of
`ssh-keyscan github.com`) into the container and set `auth.known_hosts_file`
to its path, for example `/data/keys/known_hosts` when it sits in the
`/data` directory. Without `known_hosts_file`, the host key is checked against the
files named in the `SSH_KNOWN_HOSTS` environment variable (colon-separated),
else `~/.ssh/known_hosts` and `/etc/ssh/ssh_known_hosts`, which do not exist in
the image: set `SSH_KNOWN_HOSTS` on the MCP Machine container in that case.

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

MCP Machine knows it runs in a container when `/run/.containerenv` exists. To
join the shared network it needs its own container ID: Podman writes it into
that file only for `--privileged` containers, so otherwise MCP Machine looks up
its host name (the short container ID unless `--hostname` is set) through the
Podman API. If that fails too, set `MCP_MACHINE_CONTAINER_ID` to the
container's name or ID (for example `-e MCP_MACHINE_CONTAINER_ID=mcp-machine`).
When the ID cannot be found MCP Machine logs a warning and carries on without
joining, and http-mode servers are then unreachable unless MCP Machine's
container is already on `podman.network`.

### container

| Field | Type | Meaning |
|---|---|---|
| `image` | string | Replaces the runtime image of a `command` server. |
| `volumes` | list of strings | Mounts as `src:dst[:options]` (see [Volumes](#volumes)). |
| `env` | map of strings | Extra environment variables. `${VAR}` allowed. |

### Volumes

`container.volumes` entries and `-v` in a `run` line use the same syntax,
`src:dst[:options]`:

- a `src` containing a `/` is a path on the **Podman host** (not inside the
  MCP Machine container) and is bind-mounted; a `src` without a `/` (for
  example `data:/data`) is a named Podman volume;
- `options` is a comma-separated list of at most one of `ro`/`rw` and at most
  one of `z`/`Z`, for example `/srv/projects:/projects:ro,z`.

**SELinux.** On an SELinux host (Fedora, RHEL, CentOS Stream) a host path
mounted into a server container is unreadable there until it is labeled for
containers. Add `:z` (content shared between containers) or `:Z` (private to
this one container) and Podman relabels it, or label the directory yourself
(`chcon -Rt container_file_t PATH`). `:Z` on a directory used by other
containers or by the host breaks their access. MCP Machine's own container
runs with `label=disable` (`--security-opt label=disable`,
`SecurityLabelDisable=true`) so it can use the Podman socket and its data
directory; that does not extend to the server containers it starts.

## Environment variable expansion

`${NAME}` (letters, digits, underscore; not starting with a digit) is replaced
from MCP Machine's own environment when the file is loaded. These fields are
expanded: `registries[].username` and `password`; the `vault` section's
strings; each server's `env`; `remote.headers`; `command.env`;
`container.env`; `build.args`; `build.auth.token`. `command.run` (its `-e`
values and image arguments) and `command.args` are expanded too, when the
container is created; the loaded configuration keeps them as written. Other
fields (URLs, paths, volumes) are used as written.

`${vault:PATH#KEY}` is the other kind of reference: a key of a secret in Vault,
read when the server starts rather than when the file is loaded. It is
allowed in `env`, `command.env`, `container.env`, the `-e` values of
`command.run`, `remote.headers`, `registries[].username` and `password`, and
`build.auth.token`, and refused everywhere else (`command.args`, the rest of a
run line, `build.args`, URLs, images, volumes), where it would end up in the
process list or baked into an image. See [Secrets from
Vault](#secrets-from-vault). A reference in a field that cannot take it, a
malformed one, or any reference without `vault.address` rejects the file:
`config: server "fetch": ${vault:...} is not allowed in command.args`,
`config: server "fetch": env: invalid reference "${vault:kv/x}": want
${vault:PATH#KEY}`, `config: ${vault:...} references need vault.address
(referenced by server "fetch", defined in ...)`.

A `${NAME}` that is not set, in any of these fields, is an error that names
the variable and the servers that reference it, with the files that define
them, for example `config: ${TOKEN} is not set (referenced by server "fetch",
defined in /etc/mcp-machine/config.yaml (base))`, and the file is rejected (at startup, or on reload, which keeps
the previous configuration). Supply the variables with `podman run -e NAME`, an
`environment:` block in `compose.yaml`, or `EnvironmentFile=` in the Quadlet
unit.

The UI never shows resolved secrets, and the forms keep `${VAR}` and
`${vault:...}` references verbatim. A form (add, edit, or import) that
references a variable MCP Machine's environment does not set is refused with
`environment variable NAME is not set`, and one with a `${vault:...}` in a
field that cannot take it, or without a `vault.address`, with the loader's
message, so the UI never writes a file the next start would reject.

> For more information on the Vault integration, read [secrets-from-vault.md](./secrets-from-vault.md)

## Configuration layers

```
/etc/mcp-machine/config.yaml        base: global keys and servers   (administrator; never written by MCP Machine)
/etc/mcp-machine/config.d/*.yaml    system drop-ins: servers only   (administrator; never written by MCP Machine)
/data/config.d/*.yaml               user drop-ins: servers only     (written by the web UI)
```

The files are loaded in that order: the base file, then the system drop-ins
sorted bytewise by file name, then the user drop-ins sorted the same way. In
the drop-in directories only names ending in `.yaml` or `.yml` are read; names
starting with `.` and subdirectories are ignored. Every location is optional.
A file that exists but is empty (or only whitespace) is an error.

Mount `/etc/mcp-machine` read-only from the host; `/data` is the writable state
volume. The `/etc/mcp-machine` mount is optional: without it MCP Machine falls
back to `/data/config.yaml` (see [Upgrading from `/data/config.yaml`](#upgrading-from-dataconfigyaml))
or, when that does not exist either, starts with defaults (see
[No configuration found](#no-configuration-found)).

`serve` and `validate` take the same location flags:

| Flag | Default | Meaning |
|---|---|---|
| `-c PATH` | `/etc/mcp-machine` | The admin root directory, holding `config.yaml` and `config.d/`, or a single base file. `PATH` is a root when it is an existing directory, or when it does not exist and does not end in `.yaml` or `.yml`. |
| `--system-config-d DIR` | `<root>/config.d` | The system drop-in directory. When `-c` names a file there is none unless this flag is given. |
| `--config-d DIR` | `/data/config.d` | The user drop-in directory, written by the UI. `--config-d ""` turns the user layer off, and with it editing in the UI. |

So `-c /data/config.yaml` still works as a base file, without reading
`/data/config.d` twice: it is the user layer, not a system drop-in directory.

### What each file may contain

The base file may hold any top-level key. A drop-in, system or user, holds only
`servers:`; a global key there (`listen`, `podman`, `runtimes`, ...) is an
error naming the file and the key. A name defined twice in one file is an
error naming the file. The same name in several files is one server, combined
as below.

### How definitions combine

Within the administrator's files (the base file and the system drop-ins) a
later definition of a name refines the earlier one field by field:

- `type` must be the same when both set it; a mismatch is an error naming the
  files.
- A string or number in the later definition wins when it is non-empty
  (non-zero); an empty or zero value keeps the earlier one.
- `enabled` wins whenever the later definition sets it, so `enabled: false` in
  a drop-in turns a base server off.
- Maps (`env`, `remote.headers`, `command.env`, `container.env`, `build.args`)
  merge key by key, the later value winning per key.
- Lists (`command.args`, `container.volumes`) are replaced whole when the later
  definition has one.
- Sections (`remote`, `command`, `build`, `build.auth`, `serve`, `container`)
  merge field by field by these rules when both definitions have them;
  otherwise whichever exists is used.

For example, a system drop-in that changes only the volume of a base server:

```yaml
# /etc/mcp-machine/config.yaml
servers:
  - name: filesystem
    type: command
    command:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/projects"]
    container:
      volumes: ["/srv/projects:/projects:ro"]
```

```yaml
# /etc/mcp-machine/config.d/filesystem.yaml
servers:
  - name: filesystem
    container:
      volumes: ["/srv/projects:/projects:ro,z"]
```

The server that runs has the base file's command and arguments and the
drop-in's volume list.

There is no way to blank a string or number from a later layer, or to remove
a map key: an empty string, a zero, or a missing key keeps what an earlier
file set (a map key can only be given another value, such as `""`). A list is
different: an explicit empty list (`args: []`, `volumes: []`) replaces, and so
clears, the earlier one. To remove anything else, edit the file that sets it,
or override the server from the user layer.

A user drop-in (`/data/config.d`) does not merge: its definition replaces the
administrator's definition of that name entirely. What the UI saved is exactly
what runs, and a field removed in the form stays removed. The user layer holds
one server per file, named `<name>.yaml`; when another user file also defines
that name, `<name>.yaml` wins whatever the sort order.

Defaults and `${VAR}` expansion are applied once, to the merged result, so a
server's spec hash (and so whether its container is recreated) does not depend
on how many files define it.

### Where a server is defined

The server page shows a **Defined in** line, and the Config page lists every
file loaded, in order, and a **Defined in** column per server. Each names the
files and their layer, for example
`/etc/mcp-machine/config.yaml (base), /etc/mcp-machine/config.d/filesystem.yaml (system)`
or `/etc/mcp-machine/config.yaml (base); overridden by /data/config.d/fetch.yaml (user)`.
`validate -v` prints the same. Run inside the container, it sees exactly what
`serve` sees:

```
$ sudo podman exec mcp-machine /usr/local/bin/mcp-machine validate -c /etc/mcp-machine -v
ok
files:
  /etc/mcp-machine/config.yaml
  /etc/mcp-machine/config.d/filesystem.yaml
  /data/config.d/fetch.yaml
servers:
  filesystem: Defined in /etc/mcp-machine/config.yaml (base), /etc/mcp-machine/config.d/filesystem.yaml (system)
  fetch: Defined in /etc/mcp-machine/config.yaml (base); overridden by /data/config.d/fetch.yaml (user)
```

### Delete, revert, and administrator servers

What the edit form offers depends on where the server is defined:

- **Delete** for a server defined only by `/data/config.d/<name>.yaml`: the file
  is removed and the server's container is stopped and removed.
- **Revert to admin definition** for an administrator's server that the UI has
  overridden: `/data/config.d/<name>.yaml` is removed and the server goes back
  to the administrator's definition.

Before removing the file, delete and revert load the configuration as it will
be without it, with MCP Machine's real environment, and validate it. If that
fails (for example the administrator's definition references a `${VAR}` that
is not set, which the override did not use), nothing is removed and the form
shows the error. When another file in `/data/config.d` also defines the
server, the form says that its definition stays in effect after the removal.
- No button for an administrator's server without an override. The form says
  it is defined by the administrator in the files it names and can only be
  overridden by saving changes, never deleted from the UI. A delete posted
  anyway is refused with 409 and the same explanation.

The UI manages only files named `<name>.yaml` that hold exactly that one
server. Any other file in `/data/config.d` (for example a hand-placed
`team.yaml` with several servers) is foreign: it is loaded like any other, but
the UI never rewrites or removes it. Saving one of its servers writes
`<name>.yaml`, which then wins; deleting one is refused (409) with a message
naming the file (the form offers no delete for such a server and says to
edit or remove the file by hand); adding a server whose `<name>.yaml` already
exists is refused. When `<name>.yaml` itself is foreign (it holds another server, or
more than one), editing and deleting that server are refused.

### Upgrading from `/data/config.yaml`

Before configuration layers MCP Machine read one file, `/data/config.yaml`, and
the UI wrote into it. When `-c` names the admin root (the default) and the
root has neither `config.yaml` nor any system drop-in, but `/data/config.yaml`
exists, that file is used as the base file and MCP Machine logs, and shows on
the Config page:

```
configuration read from the deprecated location /data/config.yaml; move it to /etc/mcp-machine/config.yaml
```

Everything keeps running, but the servers the UI once wrote into that file are
now administrator servers: they can be overridden, not deleted. There is no
migration tool; move the file by hand, with MCP Machine stopped (the fallback
is decided at startup, so removing `/data/config.yaml` while it runs removes
its servers). On a host that mounts `/srv/mcp-machine` at `/data`:

1. For each server you manage from the UI, create
   `/srv/mcp-machine/config.d/<name>.yaml` (`sudo mkdir -p
   /srv/mcp-machine/config.d` first if needed) holding `servers:` and that one
   entry.
2. Move `/srv/mcp-machine/config.yaml` to `/etc/mcp-machine/config.yaml` and
   delete from it the servers moved in step 1.
3. Recreate the MCP Machine container with
   `-v /etc/mcp-machine:/etc/mcp-machine:ro` added (the shipped `compose.yaml`
   and Quadlet unit already mount it), and check the result with `validate -v`
   as shown above. The warning is gone and the servers from step 1 show
   `Defined in /data/config.d/<name>.yaml (user)`.

### No configuration found

When no configuration file is found at all, `serve` starts with defaults and no
servers, logs `no configuration found (searched: PATHS); running with
defaults`, and shows the same warning on the Config page. That also applies to
`serve -c FILE` when `FILE` ends in `.yaml` or `.yml` and does not exist (a
missing path without that extension is taken as an admin root directory).

Labeled containers of servers MCP Machine does not know are left running
instead of being removed whenever no administrator file was loaded: no base
file, no legacy `/data/config.yaml`, and no system drop-in. That covers
finding nothing at all and also a start where only UI-written drop-ins in
`/data/config.d` exist (say the `/etc/mcp-machine` mount was forgotten); in
the latter case there is no warning banner, but MCP Machine logs at info
`no administrator configuration loaded (searched: /etc/mcp-machine/config.yaml,
/etc/mcp-machine/config.d); unknown containers are kept`. A base file that
defines no servers still counts as loaded. This protects a start without the
administrator's configuration; removing every file while MCP Machine runs
stops their servers like any other removal. `validate` treats a missing configuration as
an error and exits 1.

## Hot reload

MCP Machine watches all three locations: the directory that holds the base
file, the system drop-in directory, and the user drop-in directory (a drop-in
directory created later is picked up as long as its parent exists). A change
to the base file or to a `.yaml`/`.yml` file directly inside a drop-in
directory (an editor save, a rename, or a UI write) is debounced for about
250 ms, then every layer is loaded and validated together. Names starting with
`.`, which include the UI's temp files, are ignored. If the result is valid it
is applied: servers are added, replaced when their spec hash changed, or
removed (see [state-machine.md](state-machine.md)). If anything is wrong the
whole reload is rejected, the previous configuration stays in effect, and the
error appears on the Config page and in the log. A file that cannot be read or
parsed, an empty file, a global key in a drop-in, a name defined twice in one
file, and a `type` mismatch between administrator files are reported with the
file's path. A validation problem is found on the merged result, so it names
the server instead (its **Defined in** on the Config page tells you which
files define it), and an unset `${VAR}` names the variable, the servers that reference it, and
the files that define them. The same server name in several files is a merge,
logged at info as `config: server merged` with the server and the
contributing files. The Config page also has a Reload now button (`POST /config/reload`).

Each location must be a **directory** that is bind-mounted: `/etc/mcp-machine`
and `/data`. A bind mount of a single file, or a named volume, does not deliver
reliable change events and is not supported for hot reload.

A drop-in may be a symlink to a file elsewhere, and it is loaded through the
link, but only the link's directory is watched: editing the link's *target*
produces no event there and is not picked up. After editing a target, make a
change in the link's directory (for example, re-create the link) or use
**Reload now**.

With `podman machine` (macOS, Windows) the bind mount crosses into the VM
through virtiofs, which may not deliver inotify events for edits made on the
host, so the watcher can miss them. Use the Config page's **Reload now**
button (or `curl -X POST http://HOST:8080/config/reload`) after editing
files there. Edits made through the UI are applied at once either way.

Not everything is hot. `listen`, `tls`, `public_url`, `allowed_origins`,
`podman.socket`, `workspace_dir`, and `log` are read once at startup: restart
the container to change them. So are the `registries` credentials used for the
base images of `build` servers. Server definitions, `runtimes`,
`podman.stop_timeout`, and the registry credentials used for image pulls are
applied on reload; `podman.network` applies to containers created after the
reload.

## Host check

Browser requests to the MCP routes (`/servers/NAME/mcp` and
`/servers/NAME/sse`) and browser state-changing UI requests (POST) have their
`Host` header checked against a list, to defeat DNS rebinding: a hostile web
page whose name resolves to your MCP Machine sends requests whose `Origin` and
`Host` both carry the page's own name, which an origin check alone would
accept. Accepted are IP addresses (with or without a port), `localhost`, MCP
Machine's own host name, the host of `public_url`, and the hosts of
`allowed_origins`; ports are not compared. Anything else gets 403
`{"jsonrpc":"2.0","error":{"code":-32000,"message":"host not allowed"}}`.

A request counts as a browser's when it carries an `Origin` header or a
`Sec-Fetch-Site` header, which every current browser sends on every request
(`Sec-Fetch-Mode`/`Sec-Fetch-Dest` alone do not count, because Node's `fetch`
sends `Sec-Fetch-Mode: cors` and would otherwise look like a browser). Non-browser clients (MCP SDKs, `curl`,
`mcp-machine connect`, a sidecar calling `http://mcp-machine:8080/...`) send
none of them and are unaffected, whatever host name they use.

So browser requests to a DNS name (`mcp.lan`, a reverse proxy's name) must
match `public_url` or `allowed_origins`: set `public_url` to that name, or list
further names in `allowed_origins`. Reading UI pages is not affected.

## Writes from the UI

Adding, editing, and importing servers in the UI always writes
`/data/config.d/<name>.yaml`, one file per server (an import writes one file
per imported server and reloads once). The file holds the whole server
definition as the form shows it, so for an administrator's server it is a
complete override, not a patch. The base file and the system drop-ins are
never written. Deleting removes that file (see
[Delete, revert, and administrator servers](#delete-revert-and-administrator-servers)).

The forms are prefilled from the files as written, so `${VAR}` references stay
placeholders in what the UI writes. A secret written literally in an
administrator's file is a different matter: editing that server copies the
whole definition, secret included, into `/data/config.d/<name>.yaml`. Keep
secrets in the administrator's files as `${VAR}` placeholders.
Files in `/data/config.d` belong to the application: the UI rewrites them
whole, and comments or formatting added by hand there are not preserved. Keep
commented configuration in the administrator's files. The edit forms leave
fields empty when the definition relies on a default (for example `transport:
auto` on a remote server, `containerfile: Containerfile` on a build server, or
`serve: {mode: stdio}`), so saving a server does not add those keys.

A change is validated first, against the merged configuration with the new
file in place, and written atomically (a uniquely named temp file in the same
directory, then a rename, then a sync of the directory); writes from
concurrent requests are serialized. Nothing is written when validation fails,
and the form shows the problem. The UI applies the change before it redirects,
so the page you land on already shows it; the watcher's own reload that
follows is harmless because reconciling is hash-based.

At startup MCP Machine creates `/data/config.d` if it is missing. If it cannot
be created or written, adding, editing, deleting, and importing are disabled
and every page shows `editing is disabled: /data/config.d is not writable
(REASON)` (or `... is not a directory` when the path is a file, or `editing is
disabled: no user drop-in directory (--config-d "")` when the user layer is
turned off); everything else keeps working.
