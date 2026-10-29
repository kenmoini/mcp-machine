# Secrets from Vault

With a `vault` section (or `VAULT_ADDR` and `VAULT_TOKEN`), a value may name a
key of a Vault secret instead of an environment variable:

```yaml
vault:
  address: https://vault.lan:8200
  approle:
    role_id: 7a1c...                       # or role_id_file
    secret_id_file: /etc/mcp-machine/vault-secret-id

servers:
  - name: github
    type: command
    command:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_PERSONAL_ACCESS_TOKEN: "${vault:kv/data/mcp-machine/github#token}"
  - name: copilot
    type: remote
    remote:
      url: https://api.githubcopilot.com/mcp/
      headers: { Authorization: "Bearer ${vault:kv/data/mcp-machine/github#token}" }
```

`PATH` is the secret's API path under `/v1/`: for a KV **version 2** mount it
includes `data/` (`kv/data/mcp-machine/github` for `vault kv get
kv/mcp-machine/github`); for a KV version 1 mount it does not
(`kv/mcp-machine/github`). Any other secret engine whose read
returns a data object works the same way. `KEY` is a key of that data, and its
value must be a string. The reference can sit inside a longer value
(`"Bearer ${vault:...}"`) and next to `${VAR}` references. The same reference
in several fields of a server reads the secret once per start.

MCP Machine needs a token that can read the referenced paths and nothing
more. The token is checked at startup (`lookup-self`; the log line shows its
policies and TTL) and renewed in the background at two thirds of its TTL;
with an AppRole, MCP Machine logs in again when a renewal fails. A Vault that
is down at startup only delays the servers that reference it: they fail with
the Vault error and retry with the usual backoff.

## Setting up Vault for MCP Machine

The commands below use the `vault` CLI against the Vault you run (`export
VAULT_ADDR=https://vault.lan:8200` and log in as an operator first); with
OpenBao replace `vault` by `bao`. Every step is a plain API call too.

**1. Keep the servers' secrets under one prefix.** A KV version 2 mount named
`kv` with everything MCP Machine may read under `mcp-machine/`:

```sh
vault secrets enable -path=kv kv-v2            # skip if the mount exists
vault kv put kv/mcp-machine/github token=ghp_xxxxxxxx
vault kv put kv/mcp-machine/slack  bot_token=xoxb-... app_token=xapp-...
```

The configuration then references `${vault:kv/data/mcp-machine/github#token}`
and `${vault:kv/data/mcp-machine/slack#bot_token}` (KV v2 reads go through
`data/`; `vault kv get kv/mcp-machine/github` is the same secret).

**2. A read-only policy for that prefix.** Save as `mcp-machine-read.hcl`:

```hcl
# Read the current version of every secret under kv/mcp-machine/.
path "kv/data/mcp-machine/*" {
  capabilities = ["read"]
}

# Optional: lets `vault kv list kv/mcp-machine` work with this token, for
# checking what it can see. MCP Machine itself never lists.
path "kv/metadata/mcp-machine/*" {
  capabilities = ["list"]
}
```

```sh
vault policy write mcp-machine-read mcp-machine-read.hcl
```

To allow a single secret instead of a prefix, name it:
`path "kv/data/mcp-machine/github" { capabilities = ["read"] }`. For a KV
version 1 mount the paths have no `data/` or `metadata/` segment
(`path "kv/mcp-machine/*" { capabilities = ["read"] }`). Nothing else is
needed: MCP Machine calls `auth/token/lookup-self` and
`auth/token/renew-self`, which every token may do on itself, and with an
AppRole `auth/approle/login`, which needs no policy.

**3a. A service account: an AppRole bound to the policy** (recommended).
AppRole is Vault's identity for a machine: a role ID that identifies MCP
Machine and a secret ID that authenticates it, exchanged for tokens that
carry the policy.

```sh
vault auth enable approle                      # skip if already enabled
vault write auth/approle/role/mcp-machine \
  token_policies=mcp-machine-read \
  token_ttl=1h token_max_ttl=24h \
  secret_id_ttl=0 secret_id_num_uses=0         # a long-lived secret ID; see below

vault read -field=role_id auth/approle/role/mcp-machine/role-id
vault write -f -field=secret_id auth/approle/role/mcp-machine/secret-id
```

Put the role ID in the configuration and the secret ID in a file only root
and MCP Machine can read, mounted into the container:

```sh
sudo install -m 0600 -o root /dev/stdin /etc/mcp-machine/vault-secret-id <<< "$SECRET_ID"
```

```yaml
vault:
  address: https://vault.lan:8200
  approle:
    role_id: 7a1c3f2e-...                      # not secret on its own
    secret_id_file: /etc/mcp-machine/vault-secret-id
```

MCP Machine logs in at startup, gets a token with the `mcp-machine-read`
policy valid for `token_ttl`, renews it until `token_max_ttl`, then logs in
again with the same role ID and secret ID. `secret_id_ttl=0` and
`secret_id_num_uses=0` make the secret ID valid until you revoke it
(`vault write auth/approle/role/mcp-machine/secret-id-accessor/destroy
secret_id_accessor=...`); a bounded `secret_id_ttl` means rotating the file
and restarting MCP Machine before it expires. To pin the role to MCP
Machine's host, add `secret_id_bound_cidrs=10.0.0.5/32` and
`token_bound_cidrs=10.0.0.5/32` to the role.

**3b. Or a plain token bound to the policy.** Simpler, and enough for a single
host; the token itself is then the long-lived secret:

```sh
vault token create -policy=mcp-machine-read -display-name=mcp-machine \
  -orphan -period=24h -no-default-policy
```

`-period=24h` makes it renewable indefinitely as long as MCP Machine renews
it within every 24 hours, which it does; `-orphan` keeps it alive when your
operator token expires; `-no-default-policy` leaves it with only
`mcp-machine-read`. Give it to MCP Machine as `VAULT_TOKEN` on the container
(`-e VAULT_TOKEN`, an `environment:` entry, or `EnvironmentFile=`), as
`vault.token: "${VAULT_TOKEN}"`, or in a file as `vault.token_file`. To
revoke: `vault token revoke -accessor <accessor>` (the accessor is printed
when the token is created and shown by `vault token lookup`).

**4. Check the binding before starting MCP Machine.** With the token (or one
obtained from the AppRole with `vault write auth/approle/login role_id=...
secret_id=...`):

```sh
VAULT_TOKEN=<token> vault token lookup                           # policies: [mcp-machine-read]
VAULT_TOKEN=<token> vault token capabilities kv/data/mcp-machine/github   # read
VAULT_TOKEN=<token> vault token capabilities kv/data/other/secret         # deny
VAULT_TOKEN=<token> vault kv get kv/mcp-machine/github
```

Then, with the configuration in place,
`mcp-machine validate -c /etc/mcp-machine --vault` reads every reference and
reports the ones the policy does not cover (`403 permission denied`) or
that name a missing secret or key. Vault's audit log (`vault audit enable
file file_path=/var/log/vault-audit.log`) records every read under the
token's display name or the AppRole's name.

**How a secret reaches a container.** The reference text, never the value,
is in the loaded configuration, in the container's create spec, and in the
spec hash. When a container server with references starts, MCP Machine reads
the secrets, creates the container with a small tmpfs at `/run/mcp-machine`
and a `/bin/sh` wrapper as entrypoint (`podman inspect` shows the wrapper as
`Entrypoint` and the image's own entrypoint and command as `Cmd`), starts it,
and streams `NAME='value'` lines into `/run/mcp-machine/env` over `podman
exec`. The wrapper waits for that file (up to 60 seconds), sources it,
deletes it, and `exec`s the image's own command with the variables in its
environment. Nothing is written to disk and nothing shows in `podman
inspect`. Servers without references are created exactly as before, with no
wrapper and no tmpfs. The image must have `/bin/sh` (the stock `node`, `uv`,
and `python` images do); an image without it exits at once with `exec
/bin/sh: no such file or directory`, which the server page shows.

Values for MCP Machine's own use (`remote.headers`, registry credentials,
`build.auth.token`) are resolved in memory at the moment of use: when the
remote server is dialled, the image is pulled, or the repository is cloned.

**Rotation.** A changed secret does not change the spec hash, so the
container is not recreated. Restart the server (or Stop and Start it) to
read the new value: every start, including the one after an MCP Machine
restart, delivers the secrets again. A `remote` server reads its headers
again on every connect.

`mcp-machine validate --vault` reads every referenced key and reports the
ones that fail (`server "github": ${vault:kv/data/mcp-machine/github#token}: vault: read
kv/data/mcp-machine/github: 403 permission denied`), so a policy mistake is found before
a server fails.

What this does and does not protect: the secret is never in a file MCP
Machine writes, never in Podman's on-disk container configuration or
`podman inspect`, never in a container label, and never in MCP Machine's
environment. It is in the server process's environment (that is the point),
readable by anything running in that container and by root on the host, and
briefly in MCP Machine's memory during delivery. MCP Machine's Vault token
can read every path its policy allows, and the UI has no authentication, so
anyone who can reach the UI can define a server that prints any of those
paths: keep the policy to the exact paths the servers need. See
[SECURITY.md](../SECURITY.md).

Containers created by a version without Vault support from a file that
already held a literal `${vault:...}` keep their hash and are adopted on
upgrade, but have no wrapper; their first delivery fails and the retry
recreates them.