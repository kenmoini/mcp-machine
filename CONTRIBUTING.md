# Contributing to MCP Machine

Thanks for helping out. This page covers how to build, test, and submit a change.

## Prerequisites

- Go 1.26 or newer. `go.mod` pins a newer `toolchain`, which the `go` command downloads on first use.
- `make`.
- Optional: Podman, for the integration tests and for `make image`.

Install the lint and vulnerability tools once:

```sh
make tools
```

This puts `golangci-lint` (pinned to the version CI uses) and `govulncheck` in `$(go env GOPATH)/bin`. The Makefile adds that directory to `PATH`.

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | Build `bin/mcp-machine`. |
| `make test` | Run all unit tests. |
| `make test-race` | Run the tests with the race detector. |
| `make test-integration` | Run the Podman integration tests (see below). |
| `make vet` | Run `go vet`. |
| `make lint` | Run `golangci-lint` using `.golangci.yml`. |
| `make fmt` / `make fmt-check` | Format with `gofmt` / fail if anything needs formatting. |
| `make vuln` | Run `govulncheck` against reachable code. |
| `make cover` | Write `coverage.out` and `coverage.html` and print the total. |
| `make check` (and `make scan` to run the same image scan CI runs, via Grype) | `fmt-check`, `lint`, `vuln`, and `test`. Run this before you open a pull request. |
| `make tools` | Install `golangci-lint` and `govulncheck`. |

All targets pass the build tags in `GOTAGS` (`remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper`). If you call `go` directly, pass `-tags` with the same value.

## Architecture rules

The linter enforces these import boundaries (see the `depguard` rules in `.golangci.yml`):

- Only `internal/proxy`, `internal/adapters/...`, and `internal/manager` import `github.com/modelcontextprotocol/go-sdk/...`. Test files may.
- Only `internal/runtime/podman` imports `go.podman.io/...`.
- Nothing imports `github.com/pkg/errors`; use the standard library.

Do not silence a linter finding without a reason. A `//nolint:<linter> // <why>` comment needs a concrete justification.

## Tests

Unit tests never touch a socket; they use the in-memory fake engine in `internal/runtime/fake`. Write the test first, watch it fail, then make it pass.

Integration tests run against a real Podman and are skipped unless a socket is given:

```sh
MCP_MACHINE_PODMAN_SOCKET=unix:///run/podman/podman.sock make test-integration
```

With `podman machine` on macOS, use the socket path from `podman machine inspect`, for example `unix://$HOME/.local/share/containers/podman/machine/podman.sock`.

Working in a restricted sandbox (no port binding, no network, no `git`)? See "Environment notes" in [docs/build-recap.md](docs/build-recap.md).

## Commit messages

Use `type(scope): subject`, in the imperative, lower case, no trailing period:

```
fix(httpadapter): treat only Origin/Sec-Fetch-Site as browser signals
chore(deps): bump grpc to v1.83.1
test: cover podman helpers and manager removal paths
docs: build recap and parked items
```

Common types: `feat`, `fix`, `test`, `docs`, `chore`, `refactor`, `ci`. The scope is the package or area and is optional. Explain the why in the body when it is not obvious.

## Pull request checklist

- [ ] Tests cover the change (a failing test existed before the fix).
- [ ] `make check` passes.
- [ ] Docs are updated (`README.md`, `config.example.yaml`, or `docs/`) when behavior or configuration changes.
- [ ] No new dependency without a reason; `go.mod` and `go.sum` are tidy.
- [ ] Commit messages follow the convention above.

## Security

Do not report vulnerabilities in public issues. See [SECURITY.md](SECURITY.md).
