# Secrets from HashiCorp Vault (and OpenBao)

Date: 2026-10-28. Status: approved design; implementation plan in
`docs/superpowers/plans/2026-10-28-vault-secrets.md`.

## Intent

Keep the credentials MCP servers need in Vault, give MCP Machine one scoped
credential, and have each server's secrets appear only inside that server's
container, in memory: never on the host disk, never in `podman inspect`,
never in MCP Machine's environment, and never in a configuration file.

Today every secret is a `${VAR}` resolved from MCP Machine's own process
environment, which means every credential for every server is mounted into
the one MCP Machine container; the resolved values land in the server
container's create spec, which Podman persists on disk and shows in `podman
inspect`; and they feed the spec hash, so the `io.mcp-machine/hash` label is
a digest over the secrets and rotating one recreates the container.

## Goals

- A server's secret is referenced in the YAML and read from Vault when the
  server starts.
- Nothing secret is written to disk by MCP Machine or by Podman on its behalf.
- Servers without Vault references are unaffected: same hash, no recreate on
  upgrade, no new runtime dependency.
- Works unchanged against OpenBao.

## Non-goals and anti-goals

- No Vault Agent sidecars: MCP Machine already owns the Podman socket (host
  root) and is the trust root; a sidecar per server adds containers and an
  image for no gain in trust.
- No secrets as files (`GOOGLE_APPLICATION_CREDENTIALS`-style) in this pass.
- No live rotation: a changed secret is picked up on the next start.
- Not a fix for the unauthenticated UI: anyone who can reach it can define a
  server that reads any path MCP Machine's Vault policy allows.

## Decisions

| Topic | Decision |
|---|---|
| Syntax | `${vault:PATH#KEY}` next to `${VAR}`. PATH is the API path under `/v1/` (`kv/data/github` for KV v2, `kv/github` for v1); KEY is a key of the secret's data. Composes inside strings (`"Bearer ${vault:...}"`). |
| Where allowed | `env`, `command.env`, `container.env`, run-line `-e K=V` values (delivered into the container); `remote.headers`, `registries[].username/password`, `build.auth.token` (resolved in memory at the moment of use). Refused everywhere else: `command.args`, run-line image arguments, `build.args`, URLs, volumes, images. |
| Load time | References are syntax-checked and kept verbatim. The hash holds the reference text, not the value; the UI never sees values. |
| Delivery | The container gets a tmpfs at `/run/mcp-machine` (sticky `1777`) and a `/bin/sh` wrapper entrypoint that waits for `/run/mcp-machine/env`, sources it, deletes it and `exec`s the image's original entrypoint+cmd. After start, MCP Machine streams `NAME='value'` lines in over `podman exec` with stdin attached. Installed only when the server has a reference in an env field. |
| Rotation | Restart (or Stop/Start) re-reads Vault; the container is not recreated. |
| Auth | `vault.token` / `vault.token_file` / `VAULT_TOKEN`, or `vault.approle` with `role_id[_file]` and `secret_id[_file]`: login, renew-self at two thirds of the lease, re-login on failure. `VAULT_ADDR`, `VAULT_NAMESPACE`, `VAULT_CACERT`, `VAULT_SKIP_VERIFY` override the file like `MCP_MACHINE_PODMAN_SOCKET` does. |
| Client | A small `net/http` client in `internal/vault`: four endpoints, no `hashicorp/vault/api` dependency. |

## Why not the alternatives

- Vault Agent process-supervisor mode injects env vars and execs, but needs
  the `vault` binary in every server image; the stock `node`, `uv` and
  `python` runtime images do not have it.
- Podman secrets: the `file` driver stores data base64-encoded on host disk;
  the `shell` driver is a host-wide `containers.conf` hook, which is the
  "script that hands out all credentials" this design exists to avoid.
- Env in the create spec: Podman persists it in the container's OCI config on
  disk and shows it in `podman inspect`.

## Residual exposures

- The server process has its secrets in its environment; a rogue MCP server
  can read its own.
- Host root can read any container's memory and tmpfs.
- MCP Machine's memory holds each server's secrets briefly during delivery
  and the Vault token for its lifetime.
- The UI has no authentication. Scope the Vault policy to the exact paths the
  servers use.

## Left to the builder

- Exact error texts, within the patterns already used by the loader.
- The wrapper's polling interval and its 60s budget.
