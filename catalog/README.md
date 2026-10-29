# Server catalog

One drop-in per server, ready to copy into `/etc/mcp-machine/config.d/`
(administrator) or `/data/config.d/<name>.yaml` (the UI's layer), and shown
as cards in the **Catalog** section of the web UI's Add server page, where a
card opens the matching form prefilled. The image ships these files at
`/usr/share/mcp-machine/catalog` (`serve --catalog`); bind-mount a directory
of your own over it to offer a different set. Each file holds only a
`servers:` list, as a drop-in must, with exactly one server named like the
file. Secrets are
`${vault:kv/data/mcp-machine/<name>#<key>}` references; the comment at the top
of each file says what to store:

```sh
vault kv put kv/mcp-machine/github token=ghp_...
```

The UI reads the leading comment block of each file for its card: the first
line is the description; a line that is a bare URL is the upstream link; a
`# Secret:` line is the secret to store; a `# category:` line (`development`
or `security`) is shown as a tag. A file that does not parse, holds more than
one server, or names its server differently from the file is skipped with a
warning in the log.

With no Vault, replace a reference by `${VAR}` and pass the variable to the
MCP Machine container. Host paths in `volumes:` are paths on the Podman host;
adjust them. See [docs/config.md](../docs/config.md) for every field and
[Secrets from Vault](../docs/config.md#secrets-from-vault) for the policy.

| File | Type | What it is | Secret |
|---|---|---|---|
| `github.yaml` | command (image) | GitHub's official server, local | `github#token` |
| `github-remote.yaml` | remote | GitHub's hosted server | `github#token` |
| `gitlab.yaml` | remote | GitLab's built-in server (19.2+) | `gitlab#token` (fine-grained PAT with the MCP tool resource) |
| `context7.yaml` | remote | Up-to-date library docs | `context7#api_key` (optional) |
| `filesystem.yaml` | command (npx) | Files under a read-only host directory | none |
| `git.yaml` | command (uvx) | Git operations on mounted repositories | none |
| `fetch.yaml` | command (uvx) | Fetch and convert web pages | none |
| `memory.yaml` | command (npx) | A knowledge graph shared by every consumer | none |
| `sequential-thinking.yaml` | command (npx) | Structured step-by-step reasoning | none |
| `playwright.yaml` | command (image) | Browser automation (one shared browser) | none |
| `kubernetes.yaml` | command (npx) | Read-only cluster access through a mounted kubeconfig | none (kubeconfig mount) |
| `terraform.yaml` | command (image) | Terraform registry and module docs | none |
| `postgres.yaml` | command (image) | Postgres, restricted mode | `postgres#uri` |
| `grafana.yaml` | command (image) | Grafana dashboards, datasources, alerts | `grafana#token` |
| `sentry.yaml` | command (npx) | Sentry issues and events | `sentry#token` |
| `snyk.yaml` | command (npx) | SAST, SCA, IaC, container scanning | `snyk#token` |
| `semgrep.yaml` | command (image) | Semgrep scanning (`semgrep mcp`) | `semgrep#token` (optional) |
| `trivy.yaml` | build | Trivy with its MCP plugin, built from `build/trivy` | none |
| `virustotal.yaml` | command (npx) | VirusTotal enrichment | `virustotal#api_key` |
| `shodan.yaml` | command (npx) | Shodan lookups | `shodan#api_key` |
| `burp.yaml` | remote | PortSwigger's server running inside Burp | none (reachability) |
| `vault.yaml` | command (image) | HashiCorp's Vault MCP server (beta) | `vault-mcp#token` |

Everything that reaches MCP Machine's port can call every tool, so the
entries use read-only modes and expect narrowly scoped tokens. The `trivy`
entry builds an image from this repository; point `build.repo` at your own
fork if you change the Containerfile.
