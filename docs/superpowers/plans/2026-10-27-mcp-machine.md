# MCP Machine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build MCP Machine: a single-container Go web app that runs MCP servers as sibling Podman containers from a hot-reloaded YAML file and proxies each one over Streamable HTTP, HTTP+SSE, and stdio.

**Architecture:** One binary. A `runtime.Engine` interface hides the Podman bindings (with an in-memory fake for tests). A `manager` reconciles the YAML's desired servers against labeled containers and drives each through a state machine. For every server a `proxy.Facade` mirrors the backend MCP session into an `mcp.Server` that the SDK's Streamable and SSE handlers serve; the same facade powers the `connect` stdio subcommand. A server-rendered htmx UI reads a status registry and writes back to the YAML through a comment-preserving writer.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk` v1.8.0, `go.podman.io/podman/v6` v6.1.4, `gopkg.in/yaml.v3` v3.0.1, `github.com/fsnotify/fsnotify` v1.10.1, `github.com/go-git/go-git/v5` v5.19.3, `mvdan.cc/sh/v3` v3.14.1 (shell word splitting), `honnef.co/go/tools` v0.8.2 (staticcheck, as a go tool), htmx 2.0.10, UBI9 images.

**Spec:** `docs/superpowers/specs/2026-10-27-mcp-machine-design.md`

## Global Constraints

- Module path `github.com/kenmoini/mcp-machine`; `go 1.26` in `go.mod` (Podman bindings require it; SDK needs ≥1.25).
- Every build and test of packages that import Podman bindings uses `CGO_ENABLED=0` and `-tags remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper`. The Makefile variable is `GOTAGS`.
- Only `internal/runtime/podman` imports `go.podman.io/...`. Only `internal/proxy`, `internal/adapters/...`, and `internal/manager` import the MCP SDK.
- Standard library first: `net/http` with Go 1.22 mux patterns, `log/slog`, `html/template`, `embed`, `crypto/sha256`, `flag`.
- Container naming and labels: name `mcpm-<server>`; labels `io.mcp-machine/managed=true`, `io.mcp-machine/server=<name>`, `io.mcp-machine/hash=<hash>`, `io.mcp-machine/transport=stdio|http`. Network `mcp-machine`. Built images tagged `localhost/mcp-machine/<name>:<sha7>`.
- Server names match `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$` and must not be `new` or `import` (route words).
- Routes exactly as the spec's route table. MCP adapters: `/servers/{name}/mcp` (Streamable, `Stateless: true`) and `/servers/{name}/sse`.
- Origin policy: `http.CrossOriginProtection` wrapping the MCP handlers; trusted origins = `allowed_origins` + the origin of `public_url`; SDK localhost protection stays enabled.
- Secrets only via `${VAR}`; a missing variable is a load error naming the variable; UI forms keep `${VAR}` text verbatim.
- Config defaults: `listen ":8080"`, `podman.socket "unix:///run/podman/podman.sock"`, `podman.network "mcp-machine"`, `podman.stop_timeout 10s`, `data_dir "/data"`, `workspace_dir "<data_dir>/workspace"`, runtimes `npx`/`node` → `docker.io/library/node:22-bookworm-slim`, `uvx` → `ghcr.io/astral-sh/uv:python3.12-bookworm-slim`, `python` → `docker.io/library/python:3.12-slim`, `serve.mode "stdio"`, `remote.transport "auto"`, `build.containerfile "Containerfile"`, `enabled true`, `log.level "info"`, `log.format "text"`.
- Lifecycle numbers: backoff 1s doubling to a 60s cap, 10 retries then sticky Failed; http probe timeout 60s with 500ms interval; watcher debounce 250ms; stop timeout from config.
- Images: builder `registry.access.redhat.com/ubi9/go-toolset:1.26`, runtime `registry.access.redhat.com/ubi9/ubi-minimal:9.8`. Image name `ghcr.io/kenmoini/mcp-machine`.
- htmx file `internal/web/static/htmx.min.js` is version 2.0.10, sha256 `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de`, 51238 bytes.
- Commit after every task with a conventional message. Run `make lint test` before each commit.

## Review Focus

1. A server name that equals a route word (`new`, `import`) would shadow the add/import pages: Validate rejects it (Task 3).
2. A `${VAR}` that is unset in MCP Machine's environment must fail loudly with the variable name, not become an empty string in a container (Task 2).
3. An editor saving the YAML in two writes (truncate, then content) must not reconcile against an empty file: invalid or empty YAML keeps the last good config (Task 5).
4. A backend MCP server that advertises no tools, resources, or prompts capability must proxy with empty lists instead of failing the whole facade (Task 12).
5. A command container that exits immediately (bad package name) must land in Failed with the exit code and last log lines visible, and must stop retrying after the retry cap (Task 25).

---

## File Structure

```
go.mod, go.sum, Makefile, Containerfile, compose.yaml, config.example.yaml, README.md, .gitignore, .containerignore
cmd/mcp-machine/main.go                     calls cli.Main
internal/buildinfo/buildinfo.go             Version, Commit, Date set by -ldflags
internal/cli/cli.go                         Main, Run, Options, usage; subcommand dispatch
internal/cli/serve.go                       serve subcommand wiring
internal/cli/connect.go                     connect subcommand
internal/cli/validate.go, import.go         validate and import subcommands
internal/config/config.go                   Config and spec types, defaults
internal/config/load.go                     Parse, Load, env expansion
internal/config/validate.go                 Validate
internal/config/hash.go                     Hash
internal/config/watcher.go                  Watcher
internal/config/writer.go                   Writer (yaml.Node editing, atomic save)
internal/runtime/runtime.go                 Engine interface and value types
internal/runtime/self.go                    DetectSelf
internal/runtime/fake/engine.go             fake Engine
internal/runtime/podman/engine.go           Podman Engine
internal/runtime/podman/spec.go             toSpecGen
internal/runtime/podman/attach.go           attach plumbing
internal/store/labels.go, registry.go       labels, Registry, State, Status
internal/sources/sources.go                 Resolved, Resolver, composite resolver
internal/sources/registryauth.go            RegistryAuthFor
internal/sources/clientconfig/parse.go      mcpServers JSON parser
internal/sources/dockerrun/parse.go         docker run line parser
internal/sources/command/resolve.go         command → ContainerSpec
internal/sources/remote/resolve.go          remote → Resolved
internal/sources/build/cloner.go, gogit.go, builder.go
internal/proxy/facade.go                    Facade
internal/proxy/dial.go                      DialIO, DialHTTP
internal/adapters/httpadapter/adapter.go    Mount, Options, origin handling
internal/adapters/stdio/bridge.go           Run
internal/manager/manager.go                 Manager, Options
internal/manager/runner.go                  per-server state machine
internal/manager/reconcile.go               Apply diff and adoption
internal/manager/endpoint.go, probe.go      http-mode endpoint and readiness probe
internal/manager/backoff.go, clock.go
internal/ops/ops.go                         healthz, readyz, metrics
internal/web/web.go                         New, Deps, interfaces, routes
internal/web/pages.go, partials.go, forms.go, importpage.go
internal/web/templates/*.html, internal/web/static/{htmx.min.js,app.css}
deploy/quadlet/mcp-machine.container, mcp-machine.network
deploy/deploy_test.go
docs/architecture.md, config.md, clients.md, state-machine.md
test/integration/podman_test.go
```

---

## M0: Scaffold

### Task 1: Module, CLI skeleton, Makefile

**Files:**
- Create: `go.mod`, `cmd/mcp-machine/main.go`, `internal/buildinfo/buildinfo.go`, `internal/cli/cli.go`, `Makefile`, `.gitignore`, `.containerignore`
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `cli.Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int`; `cli.Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options) int`; `type Options struct{ NewEngine func(ctx context.Context, socket string) (runtime.Engine, error) }` (field added in Task 18; declare the struct now, empty); `buildinfo.Version`, `buildinfo.Commit`, `buildinfo.Date string` (defaults `"dev"`, `"none"`, `"unknown"`).
- Exit codes: 0 ok, 1 runtime error, 2 usage error.

- [ ] **Step 1: Write the failing tests**

```go
func TestVersionPrintsVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := cli.Main([]string{"version"}, strings.NewReader(""), &out, &errb)
	if code != 0 || !strings.HasPrefix(out.String(), "mcp-machine ") { t.Fatalf("code=%d out=%q", code, out.String()) }
}
func TestUnknownCommandIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code := cli.Main([]string{"bogus"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "usage:") { t.Fatalf("code=%d err=%q", code, errb.String()) }
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestVersion|TestUnknown' -v`
Expected: FAIL (package does not exist)

- [ ] **Step 3: Implement**

`go mod init github.com/kenmoini/mcp-machine`, set `go 1.26`. `go get -tool honnef.co/go/tools/cmd/staticcheck@v0.8.2`. `main.go`: `os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))`. `cli.go`: `Main` calls `Run` with `context.Background()` and default `Options{}`; `Run` switches on `args[0]` (`version`, `serve`, `connect`, `validate`, `import`; the last four print "not implemented" and return 1 for now), prints `usage:` block to stderr on unknown or empty args. `Makefile` targets and variables per the spec:

```make
GOTAGS ?= remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper
ENGINE ?= podman
IMAGE  ?= ghcr.io/kenmoini/mcp-machine
TAG    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X github.com/kenmoini/mcp-machine/internal/buildinfo.Version=$(TAG) -X ...Commit=$(shell git rev-parse --short HEAD) -X ...Date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
build test test-integration lint fmt fmt-check run validate image push compose-up compose-down quadlet-install htmx-vendor clean
```
`test`: `CGO_ENABLED=0 go test -tags $(GOTAGS) -race -count=1 ./...`. `lint`: `go vet -tags $(GOTAGS) ./... && go tool staticcheck -tags $(GOTAGS) ./...`. `test-integration`: `go test -tags "$(GOTAGS) integration" -count=1 ./test/integration/...`. `htmx-vendor`: curl `https://unpkg.com/htmx.org@2.0.10/dist/htmx.min.js` to `internal/web/static/htmx.min.js` and verify the sha256 from Global Constraints. `.gitignore`: `bin/`, `*.test`, `.env`. `.containerignore`: `bin/`, `.git/`, `docs/`.

- [ ] **Step 4: Run tests and lint**

Run: `make lint test`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd internal Makefile .gitignore .containerignore
git commit -m "feat: scaffold module, CLI skeleton, and Makefile"
```

---

## M1: Config

### Task 2: Config types, Parse, Load, defaults, env expansion

**Files:**
- Create: `internal/config/config.go`, `internal/config/load.go`
- Test: `internal/config/load_test.go`

**Interfaces:**
- Produces:

```go
type Config struct {
	Listen         string            `yaml:"listen"`
	PublicURL      string            `yaml:"public_url"`
	TLS            TLSConfig         `yaml:"tls"`
	Podman         PodmanConfig      `yaml:"podman"`
	WorkspaceDir   string            `yaml:"workspace_dir"`
	DataDir        string            `yaml:"data_dir"`
	AllowedOrigins []string          `yaml:"allowed_origins"`
	Registries     []Registry        `yaml:"registries"`
	Runtimes       map[string]string `yaml:"runtimes"`
	Log            LogConfig         `yaml:"log"`
	Servers        []ServerSpec      `yaml:"servers"`
}
type TLSConfig struct{ CertFile string `yaml:"cert_file"`; KeyFile string `yaml:"key_file"` }
type PodmanConfig struct{ Socket string; Network string; StopTimeout time.Duration `yaml:"stop_timeout"` }
type Registry struct{ Registry, Username, Password string }
type LogConfig struct{ Level, Format string }
type ServerSpec struct {
	Name string; Type string; Enabled *bool
	Remote *RemoteSpec; Command *CommandSpec; Build *BuildSpec
	Serve *ServeSpec; Container *ContainerOverrides; Env map[string]string
}
type RemoteSpec struct{ URL string; Transport string; Headers map[string]string }
type CommandSpec struct{ Command string; Args []string; Env map[string]string; Run string }
type BuildSpec struct{ Repo, Ref, Subdir, Containerfile string; Args map[string]string; Auth *GitAuth }
type GitAuth struct{ Token string; SSHKeyFile string `yaml:"ssh_key_file"` }
type ServeSpec struct{ Mode string; Port int; Path string; Transport string }
type ContainerOverrides struct{ Image string; Volumes []string; Env map[string]string }

func (s ServerSpec) IsEnabled() bool            // nil → true
func (s ServerSpec) RuntimeImage(c *Config) string // image a command server will use (override, else runtimes[command])
func Parse(data []byte, lookup func(string) (string, bool)) (*Config, error)
func Load(path string) (*Config, error)       // Parse with os.LookupEnv, then MCP_MACHINE_PODMAN_SOCKET override
func ApplyDefaults(c *Config)
```
- Errors: unknown YAML key → error containing the key (yaml.v3 `KnownFields(true)`); missing variable → `config: ${NAME} is not set`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseAppliesDefaults(t *testing.T) {
	c, err := config.Parse([]byte("servers: []\n"), noEnv)
	// assert c.Listen == ":8080", c.Podman.Socket == "unix:///run/podman/podman.sock",
	// c.Podman.Network == "mcp-machine", c.Podman.StopTimeout == 10*time.Second,
	// c.DataDir == "/data", c.WorkspaceDir == "/data/workspace",
	// c.Runtimes["npx"] == "docker.io/library/node:22-bookworm-slim", c.Log.Level == "info"
}
func TestParseExpandsEnvInHeadersAndEnv(t *testing.T) {
	// yaml: one remote server with header "Bearer ${TOK}" and one command server env A: "${TOK}-x"
	// lookup returns "secret" for TOK; assert both expanded; assert c.Servers[0].Remote.Transport == "auto" default
}
func TestParseMissingEnvVarIsError(t *testing.T) { /* error contains "${TOK} is not set" */ }
func TestParseRejectsUnknownKeys(t *testing.T) { /* "lisen: x" → error contains "lisen" */ }
func TestLoadEnvOverridesSocket(t *testing.T) {
	t.Setenv("MCP_MACHINE_PODMAN_SOCKET", "unix:///tmp/p.sock")
	// write temp file "servers: []", Load → c.Podman.Socket == "unix:///tmp/p.sock"
}
func TestServerSpecDefaults(t *testing.T) {
	// build server with no ref/containerfile, command server with no serve → Serve.Mode "stdio", Build.Containerfile "Containerfile", IsEnabled true
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -v`
Expected: FAIL (undefined: config.Parse)

- [ ] **Step 3: Implement `config.go` and `load.go`**

Expansion: regex `\$\{([A-Za-z_][A-Za-z0-9_]*)\}` applied to every string field after decoding (walk `RemoteSpec.Headers`, all `Env` maps, `Registry.Username/Password`, `GitAuth.Token`, `BuildSpec.Args`); collect missing names and return one error.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): types, parse, defaults, env expansion"
```

### Task 3: Validate

**Files:**
- Create: `internal/config/validate.go`
- Test: `internal/config/validate_test.go`

**Interfaces:**
- Produces: `func Validate(c *Config) error` (all problems joined with `errors.Join`; each line starts with `server "<name>":` when server-scoped); `var NameRE = regexp.MustCompile(...)`; `var reservedNames = map[string]bool{"new": true, "import": true}`.

- [ ] **Step 1: Write the failing table test**

Table `TestValidate` with cases (name → yaml fragment → expected substring of error, or "" for valid):
`dup names` → `duplicate server name "a"`; `bad name "My_Server"` → `invalid name`; `reserved name "new"` → `reserved`; `type remote without remote block` → `remote section required`; `type command with both command and run` → `command.command and command.run are mutually exclusive`; `type command with neither` → `one of command.command or command.run`; `serve mode http without port` → `serve.port required`; `tls cert without key` → `tls.cert_file and tls.key_file must both be set`; `podman socket "http://x"` → `podman.socket must start with unix:// or tcp://`; `remote transport "grpc"` → `remote.transport must be http, sse, or auto`; `serve mode "udp"` → `serve.mode must be stdio or http`; `valid full config` → "".

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run TestValidate -v`
Expected: FAIL (undefined: config.Validate)

- [ ] **Step 3: Implement `Validate`**

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): validation rules"
```

### Task 4: Spec hash

**Files:**
- Create: `internal/config/hash.go`
- Test: `internal/config/hash_test.go`

**Interfaces:**
- Produces: `func Hash(s ServerSpec, runtimeImage string) string` (hex sha256 of canonical JSON: struct fields in declared order, maps with sorted keys, `Enabled` excluded, `runtimeImage` appended).

- [ ] **Step 1: Write the failing tests**

```go
func TestHashIgnoresMapOrderAndEnabled(t *testing.T) { /* two specs differing only in env key order and Enabled → equal */ }
func TestHashChangesWhenArgsChange(t *testing.T) { /* args differ → not equal */ }
func TestHashChangesWithRuntimeImage(t *testing.T) { /* same spec, different runtimeImage → not equal */ }
func TestHashIsStable(t *testing.T) { /* fixed spec → golden "..." (fill in after first run, then pin) */ }
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/config/ -run TestHash -v` → FAIL

- [ ] **Step 3: Implement `Hash`** using `encoding/json` on a copy with `Enabled = nil` (Go's `json` sorts map keys).

- [ ] **Step 4: Run tests** → PASS; pin the golden value.

- [ ] **Step 5: Commit** — `git commit -m "feat(config): deterministic spec hash"`

### Task 5: Watcher

**Files:**
- Create: `internal/config/watcher.go`
- Test: `internal/config/watcher_test.go`

**Interfaces:**
- Produces:

```go
type Watcher struct{ /* unexported */ }
func NewWatcher(path string, onChange func(*Config), onError func(error)) (*Watcher, error)
func (w *Watcher) Run(ctx context.Context) error   // blocks; fsnotify on filepath.Dir(path); events for filepath.Base(path) only; 250ms debounce
func (w *Watcher) Reload() error                   // Load + Validate; on success calls onChange, else onError; returns the error
```

- [ ] **Step 1: Write the failing tests** (each writes a temp dir file, runs the watcher in a goroutine, uses a channel from `onChange`/`onError` with a 3s timeout)

```go
func TestWatcherReloadsOnOverwrite(t *testing.T)     // os.WriteFile new content → onChange with Listen ":9090"
func TestWatcherReloadsOnAtomicRename(t *testing.T)  // write ".tmp" then os.Rename → onChange
func TestWatcherInvalidYAMLCallsOnErrorOnly(t *testing.T) // write "listen: [" → onError; onChange not called within 500ms
func TestWatcherEmptyFileCallsOnErrorOnly(t *testing.T)   // truncate to 0 bytes → onError (Review Focus 3)
func TestWatcherDebouncesBurst(t *testing.T)         // 5 writes 20ms apart → exactly 1 onChange within 1s
func TestReloadReturnsValidationError(t *testing.T)  // file with dup names → Reload() error, onError called
```

- [ ] **Step 2: Run to verify failure** → FAIL (undefined: config.NewWatcher)

- [ ] **Step 3: Implement `watcher.go`** with `fsnotify.NewWatcher`, `Add(dir)`, timer reset on Write/Create/Rename/Chmod events whose `Name` base matches; an empty file is treated as an error `config: file is empty`.

- [ ] **Step 4: Run tests** — `go test ./internal/config/ -race -v` → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(config): hot-reload watcher with debounce"`

### Task 6: Writer and example config

**Files:**
- Create: `internal/config/writer.go`, `config.example.yaml`
- Test: `internal/config/writer_test.go`, `internal/config/example_test.go`

**Interfaces:**
- Produces:

```go
type Writer struct{ Path string }
func (w Writer) AddServer(s ServerSpec) error        // error if name exists
func (w Writer) UpdateServer(name string, s ServerSpec) error // error if missing; replaces node in place
func (w Writer) DeleteServer(name string) error
```
Each: parse file into `yaml.Node`, edit the `servers` sequence, encode the whole tree, `Parse`+`Validate` the bytes with a lookup that accepts every variable (so `${VAR}` text is never resolved by the writer), then write `<path>.tmp` and `os.Rename`. Spec encoding uses a `yaml.Node` built by `yaml.Marshal`ing the `ServerSpec` with `omitempty` tags so nil sections are not emitted.

- [ ] **Step 1: Write the failing tests**

```go
func TestWriterAddPreservesCommentsAndOrder(t *testing.T) // file with "# top comment", two servers with inline comments; AddServer third → golden file equal byte-for-byte except appended block
func TestWriterUpdateKeepsVarPlaceholders(t *testing.T)   // existing header "Bearer ${TOK}"; UpdateServer with same text → "${TOK}" still literal in file
func TestWriterDeleteRemovesOnlyThatServer(t *testing.T)
func TestWriterRejectsInvalidWithoutTouchingFile(t *testing.T) // AddServer with dup name → error; file mtime/content unchanged; no .tmp left
func TestWriterTriggersSingleWatcherCallback(t *testing.T)     // watcher running; AddServer → exactly 1 onChange
func TestExampleConfigLoadsAndValidates(t *testing.T)          // Parse(config.example.yaml, lookup that returns "x" for any var) + Validate → nil
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement `writer.go` and write `config.example.yaml`** matching the spec's config shape (all three server types, registries, runtimes, comments).

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git add internal/config config.example.yaml && git commit -m "feat(config): comment-preserving YAML writer and example config"`

---

## M2: Runtime engine and store

### Task 7: Runtime types and fake engine

**Files:**
- Create: `internal/runtime/runtime.go`, `internal/runtime/self.go`, `internal/runtime/fake/engine.go`
- Test: `internal/runtime/fake/engine_test.go`, `internal/runtime/self_test.go`

**Interfaces:**
- Produces (`internal/runtime`):

```go
type Engine interface {
	Ping(ctx context.Context) error
	EnsureNetwork(ctx context.Context, name string) error
	ConnectNetwork(ctx context.Context, containerID, network string) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	PullImage(ctx context.Context, ref string, auth *RegistryAuth, progress io.Writer) error
	BuildImage(ctx context.Context, req BuildRequest) (imageID string, err error)
	CreateContainer(ctx context.Context, spec ContainerSpec) (id string, err error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (ContainerInfo, error)
	ListContainers(ctx context.Context, labels map[string]string) ([]ContainerInfo, error)
	Logs(ctx context.Context, id string, tail int) ([]LogLine, error)
	Attach(ctx context.Context, id string) (io.ReadWriteCloser, error) // call before StartContainer
	Wait(ctx context.Context, id string) (exitCode int, err error)
}
type ContainerSpec struct {
	Name, Image string; Entrypoint, Command []string; Env, Labels map[string]string
	Stdin bool; Network string; Ports []PortMapping; Mounts []Mount; WorkDir string; RestartPolicy string
}
type PortMapping struct{ HostIP string; HostPort, ContainerPort int; Protocol string }
type Mount struct{ Type string /* "bind"|"volume" */; Source, Target string; ReadOnly bool }
type ContainerInfo struct{ ID, Name, Image, State string; Labels map[string]string; ExitCode int; Ports []PortMapping }
type BuildRequest struct{ ContextDir, Containerfile, Tag string; Args, Labels map[string]string; Auth *RegistryAuth }
type RegistryAuth struct{ Username, Password string }
type LogLine struct{ Stream, Text string }
var ErrNotFound = errors.New("runtime: not found")
func DetectSelf(containerenvPath string) (id string, ok bool) // parses `id="..."` line of /run/.containerenv
```
- Produces (`internal/runtime/fake`): `func New() *Engine`; `*Engine` implements `runtime.Engine`; plus `Peer(id string) (io.ReadWriteCloser, bool)` (far end of the `net.Pipe` handed out by `Attach`), `Exit(id string, code int)` (marks exited, unblocks `Wait`, closes the pipe), `FailNext(method string, err error)`, `Calls() []string` (method names in call order, e.g. `"CreateContainer"`), `Seed(info runtime.ContainerInfo)` (pre-existing container for adoption tests), `SetLogs(id string, lines []runtime.LogLine)`, `Builds() []runtime.BuildRequest`, `Pulls() []string`.

- [ ] **Step 1: Write the failing tests**

```go
func TestFakeLifecycle(t *testing.T)          // Create→Inspect State "created"; Start→"running"; Stop→"exited"; Remove→Inspect returns runtime.ErrNotFound
func TestFakeListFiltersByLabels(t *testing.T)
func TestFakeAttachPipe(t *testing.T)         // Attach then Peer; write on peer, read on attach side and back
func TestFakeWaitUnblocksOnExit(t *testing.T) // Wait in goroutine; Exit(id, 3) → 3
func TestFakeBuildRegistersTag(t *testing.T)  // BuildImage{Tag:"t"} → ImageExists("t") true; Builds() has 1
func TestFakeFailNext(t *testing.T)           // FailNext("StartContainer", errBoom) → Start returns errBoom once, then nil
func TestDetectSelf(t *testing.T)             // temp file with `engine="podman-5.7.1"\nid="abc123"\n` → ("abc123", true); missing file → ("", false)
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement** `runtime.go`, `self.go`, `fake/engine.go` (mutex-guarded maps; `Attach` creates `net.Pipe()` and stores the peer).

- [ ] **Step 4: Run tests** — `go test ./internal/runtime/... -race -v` → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(runtime): engine interface, self detection, and fake engine"`

### Task 8: Podman engine

**Files:**
- Create: `internal/runtime/podman/engine.go`, `internal/runtime/podman/spec.go`, `internal/runtime/podman/attach.go`
- Test: `internal/runtime/podman/spec_test.go`

**Interfaces:**
- Consumes: `runtime.Engine` and value types from Task 7.
- Produces: `func New(ctx context.Context, socketURL string) (*Engine, error)` (calls `bindings.NewConnection`, keeps the returned ctx as the connection ctx); `func toSpecGen(s runtime.ContainerSpec) *specgen.SpecGenerator`; `func labelFilter(labels map[string]string) map[string][]string` (`{"label": ["k=v", ...]}`).
- Bindings used: `containers.CreateWithSpec/Start/Stop/Remove/Inspect/List/Logs/Attach/Wait`, `images.Exists/Pull/Build`, `network.Exists/Create/Connect`, `system` ping via `bindings.NewConnection` (Ping = `system.Version`). Auth: `images.PullOptions{Username, Password}`; build via `types.BuildOptions{BuildOptions: define.BuildOptions{ContextDirectory, Output: tag, Args, Labels, SystemContext: &imagetypes.SystemContext{DockerAuthConfig: &imagetypes.DockerAuthConfig{Username, Password}}}}` with `ContainerFiles: []string{containerfilePath}`. Network create: `network.Create(ctx, &nettypes.Network{Name: name, DNSEnabled: true})`. Connect: `network.Connect(ctx, name, containerID, &nettypes.PerNetworkOptions{})`.
- Attach: before `Start`, `Attach` builds `stdinR, stdinW := io.Pipe()` and `stdoutR, stdoutW := io.Pipe()`, starts `containers.Attach(attachCtx, id, stdinR, stdoutW, stderrDiscard, ready, &containers.AttachOptions{Stream: ptr(true)})` in a goroutine, waits for `ready`, and returns an rwc whose `Read` is `stdoutR`, `Write` is `stdinW`, and `Close` cancels `attachCtx` and closes both pipes. When the attach goroutine returns, it closes `stdoutW` so readers get EOF. stderr is routed to `slog.Debug` per line.

- [ ] **Step 1: Write the failing test**

```go
func TestToSpecGen(t *testing.T) {
	s := runtime.ContainerSpec{Name: "mcpm-a", Image: "img", Command: []string{"npx", "x"}, Env: map[string]string{"A": "1"},
		Labels: map[string]string{"l": "v"}, Stdin: true, Network: "mcp-machine",
		Ports: []runtime.PortMapping{{HostIP: "127.0.0.1", HostPort: 0, ContainerPort: 3000, Protocol: "tcp"}},
		Mounts: []runtime.Mount{{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true}}, RestartPolicy: "no"}
	g := toSpecGen(s)
	// assert g.Name, g.Image, g.Command, g.Env, g.Labels, *g.Stdin == true, *g.Terminal == false,
	// g.Networks has key "mcp-machine", g.PortMappings[0].ContainerPort == 3000, g.Mounts[0].Options contains "ro", g.RestartPolicy == "no"
}
func TestLabelFilter(t *testing.T) { /* {"a":"1"} → {"label": ["a=1"]} */ }
```

- [ ] **Step 2: Run to verify failure**

Run: `CGO_ENABLED=0 go test -tags remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper ./internal/runtime/podman/ -v`
Expected: FAIL (package missing). `go get go.podman.io/podman/v6@v6.1.4` first; if the module graph refuses to build under these tags, stop and record the exact error in the task notes: the fallback is a libpod REST client behind the same interface, which is a plan change.

- [ ] **Step 3: Implement `engine.go`, `spec.go`, `attach.go`**

- [ ] **Step 4: Run tests and lint**

Run: `make lint test`
Expected: PASS (confirms the tagged build works for the whole tree)

- [ ] **Step 5: Commit** — `git commit -m "feat(runtime): podman engine over libpod bindings"`

### Task 9: Podman integration test

**Files:**
- Create: `test/integration/podman_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: `podman.New`, `runtime.Engine`.

- [ ] **Step 1: Write the test** (skips with `t.Skip` when `MCP_MACHINE_PODMAN_SOCKET` is unset)

```go
func TestPodmanEngineEndToEnd(t *testing.T) {
	// New(ctx, socket); Ping ok
	// EnsureNetwork("mcpm-it-net") twice → no error
	// PullImage("docker.io/library/alpine:3.20", nil, io.Discard)
	// CreateContainer{Name: "mcpm-it-cat", Image: alpine, Command: ["cat"], Stdin: true, Network: net, Labels: {"io.mcp-machine/managed": "true"}}
	// rwc := Attach(id); StartContainer(id); rwc.Write("hello\n"); bufio read line == "hello"
	// ListContainers(labels) contains id; Logs(id, 10) no error
	// StopContainer(id, 2s); Wait(id) returns; RemoveContainer(id, true)
	// BuildImage{ContextDir: tempdir with "Containerfile": "FROM docker.io/library/alpine:3.20\nLABEL it=1\n", Containerfile: "Containerfile", Tag: "localhost/mcp-machine/it:test"} → ImageExists true
	// PullImage("ghcr.io/kenmoini/does-not-exist:1", &RegistryAuth{"u","p"}, io.Discard) → error (not a hang)
	// ConnectNetwork on a second created container → no error; Inspect shows the network
	// cleanup with t.Cleanup
}
```

- [ ] **Step 2: Run against the local podman machine**

Run: `MCP_MACHINE_PODMAN_SOCKET=unix://$HOME/.local/share/containers/podman/machine/podman.sock make test-integration`
Expected: PASS. If attach ordering or demuxing fails, fix `attach.go` here (this is the risk the task exists to retire).

- [ ] **Step 3: Commit** — `git commit -m "test: podman engine integration test"`

### Task 10: Store: labels and registry

**Files:**
- Create: `internal/store/labels.go`, `internal/store/registry.go`
- Test: `internal/store/labels_test.go`, `internal/store/registry_test.go`

**Interfaces:**
- Produces:

```go
const (LabelManaged = "io.mcp-machine/managed"; LabelServer = "io.mcp-machine/server"; LabelHash = "io.mcp-machine/hash"; LabelTransport = "io.mcp-machine/transport")
const ContainerPrefix = "mcpm-"
type LabelInfo struct{ Name, Hash, Transport string }
func Labels(name, hash, transport string) map[string]string
func ParseLabels(m map[string]string) (LabelInfo, bool)   // false unless managed=="true" and server non-empty
func ContainerName(server string) string                   // "mcpm-" + server
type State string
const (StateDefined State = "defined"; StateResolving = "resolving"; StateBuilding = "building"; StateImageReady = "image-ready"; StateCreating = "creating"; StateRunning = "running"; StateConnecting = "connecting"; StateReady = "ready"; StateStopped = "stopped"; StateFailed = "failed"; StateRemoving = "removing")
type Status struct{ Name string; State State; Hash, ContainerID, Image, Error string; Since time.Time; Restarts int; Paused bool }
type Registry struct{ /* unexported */ }
func NewRegistry() *Registry
func (r *Registry) Set(s Status)
func (r *Registry) Get(name string) (Status, bool)
func (r *Registry) List() []Status             // sorted by Name
func (r *Registry) Delete(name string)
func (r *Registry) Subscribe() (<-chan Status, func()) // buffered 16; drops when full; cancel func unsubscribes
func (r *Registry) CountByState() map[State]int
```

- [ ] **Step 1: Write the failing tests** — `TestLabelsRoundTrip`, `TestParseLabelsRejectsUnmanaged`, `TestRegistrySetGetListSorted`, `TestRegistrySubscribeReceivesAndUnsubscribes`, `TestRegistryCountByState`.

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** — `go test ./internal/store/ -race -v` → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(store): container labels and status registry"`

---

## M3: Proxy facade

### Task 11: Facade mirrors and forwards

**Files:**
- Create: `internal/proxy/facade.go`, `internal/proxy/dial.go`
- Test: `internal/proxy/facade_test.go`

**Interfaces:**
- Produces:

```go
type Facade struct{ /* name, server *mcp.Server, mu sync.RWMutex, sess *mcp.ClientSession, snap Snapshot, calls atomic.Uint64 */ }
type Snapshot struct{ Tools []*mcp.Tool; Resources []*mcp.Resource; ResourceTemplates []*mcp.ResourceTemplate; Prompts []*mcp.Prompt }
func New(name, version string) *Facade              // mcp.NewServer(&mcp.Implementation{Name: "mcp-machine/"+name, Version: version}, &mcp.ServerOptions{HasTools: true, HasResources: true, HasPrompts: true})
func (f *Facade) Server() *mcp.Server
func (f *Facade) Bind(ctx context.Context, sess *mcp.ClientSession) error // sets session, Resync
func (f *Facade) Unbind()                                                 // clears session; lists stay until next Bind
func (f *Facade) Resync(ctx context.Context) error                        // paginate ListTools/ListResources/ListResourceTemplates/ListPrompts; diff vs snapshot; Add*/Remove* on server
func (f *Facade) Snapshot() Snapshot
func (f *Facade) Calls() uint64
func (f *Facade) ClientOptions() *mcp.ClientOptions                       // the three ListChanged handlers call Resync(ctx)
var ErrBackendUnavailable = errors.New("proxy: backend unavailable")
func DialIO(ctx context.Context, rwc io.ReadWriteCloser, opts *mcp.ClientOptions) (*mcp.ClientSession, error) // mcp.NewClient(...).Connect(ctx, &mcp.IOTransport{Reader: rwc, Writer: rwc}, nil)
```
Forwarding handlers: tool → `sess.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})`; resource and template → `sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})`; prompt → `sess.GetPrompt(ctx, &mcp.GetPromptParams{Name, Arguments})`. Each increments `calls`. Nil session → `ErrBackendUnavailable`.

- [ ] **Step 1: Write the failing tests** (helper `newBackend(t)` builds an `mcp.Server` with tool `echo` (returns its `s` arg as text), tool `add`, resource `file:///a` (text "A"), resource template `file:///{p}`, prompt `greet`; `connectPair(t, server)` uses `mcp.NewInMemoryTransports`)

```go
func TestFacadeMirrorsLists(t *testing.T)         // after Bind, a client of facade.Server() ListTools names == ["add","echo"]; resources, templates, prompts likewise
func TestFacadeForwardsCallTool(t *testing.T)     // CallTool("echo", {"s":"hi"}) → Content[0].(*mcp.TextContent).Text == "hi"
func TestFacadeForwardsReadResourceAndPrompt(t *testing.T)
func TestFacadePaginates(t *testing.T)           // backend ServerOptions{PageSize: 2} with 5 tools → facade lists 5
func TestFacadeCountsCalls(t *testing.T)
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement `facade.go` and `dial.go`**

- [ ] **Step 4: Run tests** — `go test ./internal/proxy/ -race -v` → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(proxy): facade that mirrors and forwards a backend MCP session"`

### Task 12: Resync, rebind, capability-less backends

**Files:**
- Modify: `internal/proxy/facade.go`
- Test: `internal/proxy/facade_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestFacadeResyncOnToolListChanged(t *testing.T)   // Bind with facade.ClientOptions(); backend.AddTool("later"); facade client sees "later" within 2s; backend.RemoveTools("echo") → gone within 2s
func TestFacadeRebindAfterSessionLoss(t *testing.T)    // close first backend session; CallTool → ErrBackendUnavailable; Bind(new session) → CallTool works
func TestFacadeToleratesMissingCapabilities(t *testing.T) // backend mcp.NewServer with no features and nil options → Bind returns nil; Snapshot all empty (Review Focus 4)
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement** — in `Resync`, treat a `tools/list` (etc.) error whose JSON-RPC code is `-32601` or whose backend capability is absent as an empty list; `Unbind` on session close detection via `sess.Wait()` goroutine started in `Bind`.

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(proxy): resync on list changes, rebind, capability-less backends"`

### Task 13: Error propagation

**Files:**
- Modify: `internal/proxy/facade.go`
- Test: `internal/proxy/facade_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestFacadeKeepsIsErrorResults(t *testing.T)   // backend tool returns &mcp.CallToolResult{IsError: true, Content: text "bad"} → client sees IsError true, not a Go error
func TestFacadeWrapsBackendErrors(t *testing.T)    // backend handler returns errors.New("boom") → client gets error containing "boom"
func TestFacadeUnboundListsAreEmptyNoPanic(t *testing.T) // New() then client ListTools → empty, no panic
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(proxy): error propagation semantics"`

---

## M4: Adapters, ops, serve

### Task 14: Streamable HTTP adapter with Origin protection

**Files:**
- Create: `internal/adapters/httpadapter/adapter.go`
- Test: `internal/adapters/httpadapter/adapter_test.go`

**Interfaces:**
- Produces:

```go
type Lookup func(name string) (*mcp.Server, bool)
type Options struct{ AllowedOrigins []string; PublicURL string; Logger *slog.Logger }
func Mount(mux *http.ServeMux, lookup Lookup, opts Options) error // registers "/servers/{name}/mcp" and "/servers/{name}/sse"
func NewOriginProtection(opts Options) (*http.CrossOriginProtection, error) // AddTrustedOrigin for each allowed origin and the public_url origin; deny handler writes 403 with JSON-RPC error body
```
Handler per route: resolve `r.PathValue("name")` via lookup; 404 `{"error":"unknown server"}` when missing; else delegate to one shared `mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{Stateless: true, Logger: opts.Logger})` wrapped in `protection.Handler(...)`. `getServer` reads the name from the request path again.

- [ ] **Step 1: Write the failing tests** (facade from Task 11 bound to a backend; `httptest.NewServer(mux)`)

```go
func TestStreamableListTools(t *testing.T)   // mcp.StreamableClientTransport{Endpoint: srv.URL+"/servers/demo/mcp"} → ListTools names
func TestUnknownServerIs404(t *testing.T)    // POST /servers/nope/mcp → 404
func TestCrossOriginDenied(t *testing.T)     // POST with Origin "http://evil.example" and Sec-Fetch-Site "cross-site" → 403
func TestAllowedOriginAccepted(t *testing.T) // Options.AllowedOrigins ["http://evil.example"] → 200/202
func TestNoOriginAccepted(t *testing.T)      // plain client without Origin → works
func TestGetOnMcpIsRejected(t *testing.T)    // GET /servers/demo/mcp → 405 per the SDK's stateless mode; if the SDK answers another 4xx, pin that status, never 200
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement `adapter.go`** (SSE route registered in Task 15; register only `/mcp` now)

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(adapters): streamable HTTP adapter with origin protection"`

### Task 15: Legacy HTTP+SSE adapter

**Files:**
- Modify: `internal/adapters/httpadapter/adapter.go`
- Test: `internal/adapters/httpadapter/adapter_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestSSEListTools(t *testing.T)      // mcp.SSEClientTransport{Endpoint: srv.URL+"/servers/demo/sse"} → ListTools names
func TestSSEUnknownServerIs404(t *testing.T)
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement** — one shared `mcp.NewSSEHandler(getServer, &mcp.SSEOptions{})` wrapped in the same origin protection, registered at `/servers/{name}/sse` (GET opens the stream, POST carries messages with the SDK's `sessionid` query).

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(adapters): legacy HTTP+SSE adapter"`

### Task 16: stdio bridge and `connect` subcommand

**Files:**
- Create: `internal/adapters/stdio/bridge.go`, `internal/cli/connect.go`
- Modify: `internal/cli/cli.go`
- Test: `internal/adapters/stdio/bridge_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `func stdio.Run(ctx context.Context, url string, in io.ReadCloser, out io.WriteCloser, logger *slog.Logger) error` — connects `mcp.StreamableClientTransport{Endpoint: url}` with `facade.ClientOptions()`, binds a `proxy.Facade`, then `facade.Server().Run(ctx, &mcp.IOTransport{Reader: in, Writer: out})`; returns when ctx ends or either side closes. `connect <url>` subcommand calls it with `stdin`/`stdout`; logs go to stderr only.

- [ ] **Step 1: Write the failing tests**

```go
func TestBridgeListTools(t *testing.T)   // httptest server from Task 14; a, b := net.Pipe(); go Run(ctx, url, a, a, logger); client := mcp.NewClient(...).Connect(ctx, &mcp.IOTransport{Reader: b, Writer: b}, nil) → ListTools names
func TestBridgeStopsOnContextCancel(t *testing.T)
func TestConnectUsageError(t *testing.T) // cli.Main([]string{"connect"}) → 2
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(adapters): stdio bridge and connect subcommand"`

### Task 17: Ops endpoints

**Files:**
- Create: `internal/ops/ops.go`
- Test: `internal/ops/ops_test.go`

**Interfaces:**
- Produces: `type Probe func(ctx context.Context) error`; `type Metrics struct{ ServersByState map[store.State]int; ProxyCalls map[string]uint64; ConfigReloads uint64; ConfigReloadErrors uint64 }`; `func Mount(mux *http.ServeMux, ready Probe, metrics func() Metrics)` registering `GET /healthz` (always 200 `ok`), `GET /readyz` (200 or 503 with the probe error text), `GET /metrics` (Prometheus text: `mcp_machine_servers{state="..."} N`, `mcp_machine_proxy_calls_total{server="..."} N`, `mcp_machine_config_reloads_total N`, `mcp_machine_config_reload_errors_total N`).

- [ ] **Step 1: Write the failing tests** — `TestHealthz200`, `TestReadyz503WhenProbeFails`, `TestMetricsText` (contains `mcp_machine_servers{state="ready"} 2`).

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(ops): health, readiness, and metrics endpoints"`

### Task 18: `serve` wiring, TLS, graceful shutdown

**Files:**
- Create: `internal/cli/serve.go`, `internal/cli/validate.go`
- Modify: `internal/cli/cli.go`
- Test: `internal/cli/serve_test.go`

**Interfaces:**
- Consumes: `config.Load/Validate/NewWatcher`, `podman.New`, `fake.New`, `httpadapter.Mount`, `ops.Mount`; Manager and web come in later tasks, so `serve.go` wires them behind small funcs that Tasks 25 and 28 fill in (`newManager`, `newWeb`), stubbed now to return a mux that only has ops and adapters with an empty lookup.
- Produces: `Options.NewEngine func(ctx, socket string) (runtime.Engine, error)` (default `podman.New`); flags for `serve`: `-c` config path (default `/data/config.yaml`), `-listen` override. `validate -c path` prints `ok` or the validation error (exit 1).
- Server: `http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10s}`; `ListenAndServeTLS(cert, key)` when both set; on `SIGINT/SIGTERM` or ctx cancel: `Shutdown` with 15s timeout, then manager `Close`.

- [ ] **Step 1: Write the failing tests**

```go
func TestServeHealthzAndShutdown(t *testing.T)  // temp config "listen: 127.0.0.1:0"; opts.NewEngine returns fake; Run(ctx, ["serve","-c",path]) in goroutine; poll the bound address printed to stdout as "listening on http://127.0.0.1:PORT"; GET /healthz 200; cancel ctx → Run returns 0 within 5s
func TestServeTLS(t *testing.T)                 // generate self-signed cert into temp files (crypto/x509, ecdsa P-256); config tls set; https client with InsecureSkipVerify GET /healthz → 200
func TestServeInvalidConfigExits1(t *testing.T) // dup names → Run returns 1 and stderr names the server
func TestValidateCommand(t *testing.T)          // ["validate","-c",path] → 0 and "ok"
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement** — `serve.go` prints the listening URL after `net.Listen` (so port 0 works), uses `srv.Serve(ln)` / `srv.ServeTLS(ln, cert, key)`.

- [ ] **Step 4: Run tests** — `make lint test` → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(cli): serve wiring with TLS and graceful shutdown; validate command"`

---

## M5: Sources

### Task 19: Client-config parser and `import` subcommand

**Files:**
- Create: `internal/sources/clientconfig/parse.go`, `internal/cli/import.go`
- Test: `internal/sources/clientconfig/parse_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `func clientconfig.Parse(data []byte) ([]config.ServerSpec, error)`; `func clientconfig.SanitizeName(s string) string` (lowercase, non `[a-z0-9-]` → `-`, collapse, trim, truncate 63). Entry mapping: `command`+`args`+`env` → `Type: "command"`, `Command{Command, Args, Env}`; `type: http|streamable-http` or `url` with `type` absent → `Type: "remote"`, `Remote{URL, Transport: "http", Headers}`; `type: sse` → `Transport: "sse"`; unknown `type` → error naming it. `import <file|->` reads JSON, prints a YAML `servers:` list to stdout via `yaml.Marshal`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseCommandEntry(t *testing.T)      // {"mcpServers":{"airtable":{"command":"npx","args":["-y","airtable-mcp-server"],"env":{"K":"${AIRTABLE_API_KEY}"}}}}
func TestParseHttpAndSseEntries(t *testing.T) // headers carried; sse → Transport "sse"
func TestParseUnknownTypeIsError(t *testing.T)
func TestSanitizeName(t *testing.T)           // "My Server!" → "my-server"
func TestImportCommandPrintsYAML(t *testing.T)// golden stdout starts with "servers:\n- name: airtable\n  type: command\n"
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(sources): mcpServers JSON parser and import subcommand"`

### Task 20: `docker run` line parser

**Files:**
- Create: `internal/sources/dockerrun/parse.go`
- Test: `internal/sources/dockerrun/parse_test.go`

**Interfaces:**
- Produces:

```go
type Spec struct{ Image string; Args []string; Env map[string]string; PassEnv []string; Volumes []string; Ports []string; Name, Network string; Interactive, Remove bool }
func Parse(line string) (Spec, error) // tokens via shell.Fields (mvdan.cc/sh/v3/shell); leading "docker"/"podman" and "run" required
```
Flags: `-i/--interactive`, `--rm`, `-e/--env K=V` and `-e K` (→ PassEnv), `-v/--volume`, `-p/--publish`, `--name`, `--network`, `-w/--workdir`; `-it`, `-t`, `--tty` → error `tty is not supported`; `--env-file` → error `unsupported flag --env-file`; any other `-x`/`--x` → error `unknown flag <x>`; first non-flag token is the image, rest are args.

- [ ] **Step 1: Write the failing table test** `TestParse` with cases: full line with every supported flag and a quoted arg `"arg two"`; `podman run` prefix; `-e KEY` passthrough; `-it` error; `--env-file` error; unknown flag error names it; missing image error; `docker build ...` error `expected run`.

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(sources): docker run command line parser"`

### Task 21: Registry auth lookup and command resolver

**Files:**
- Create: `internal/sources/sources.go` (types only for now), `internal/sources/registryauth.go`, `internal/sources/command/resolve.go`
- Test: `internal/sources/registryauth_test.go`, `internal/sources/command/resolve_test.go`

**Interfaces:**
- Produces (`internal/sources`):

```go
type Kind int
const (KindRemote Kind = iota + 1; KindContainer)
type RemoteEndpoint struct{ URL, Transport string; Headers map[string]string }
type HTTPEndpoint struct{ Port int; Path, Transport string }
type BuildPlan struct{ Name, Repo, Ref, Subdir, Containerfile string; Args map[string]string; Auth *config.GitAuth; RegistryAuth *runtime.RegistryAuth }
type ImageSource struct{ Ref string; Build *BuildPlan; Auth *runtime.RegistryAuth }
type Resolved struct{ Kind Kind; Remote *RemoteEndpoint; Container *runtime.ContainerSpec; Image ImageSource; Transport string; HTTP *HTTPEndpoint }
type Resolver interface{ Resolve(ctx context.Context, cfg *config.Config, spec config.ServerSpec) (Resolved, error) }
func RegistryAuthFor(regs []config.Registry, imageRef string) *runtime.RegistryAuth // host of ref (default "docker.io"); longest matching Registry prefix wins; nil if none
```
- Produces (`internal/sources/command`): `func Resolve(cfg *config.Config, spec config.ServerSpec) (sources.Resolved, error)`. Rules: `Command.Run` set → `dockerrun.Parse`, image from it, args, env (+`PassEnv` resolved from `os.LookupEnv`, error if unset), volumes → bind mounts, ports → `PortMapping`; else `Command.Command` must be a key of `cfg.Runtimes` (error `unknown command "x"; add it to runtimes`), image `spec.RuntimeImage(cfg)`, container command `[command, args...]`, cache mount volume `mcpm-cache-npm:/root/.npm` for npx/node and `mcpm-cache-uv:/root/.cache/uv` for uvx/python. Always: `Name: store.ContainerName(spec.Name)`, `Stdin: true`, `Network: cfg.Podman.Network`, env merge order runtime env < `Command.Env` < `spec.Env` < `Container.Env`, `Container.Volumes` appended as bind mounts (`src:dst[:ro]`), `Container.Image` overrides image, `Transport: "stdio"`, `Labels` left empty (manager sets them), `Image.Auth = RegistryAuthFor(cfg.Registries, image)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRegistryAuthForLongestPrefix(t *testing.T) // regs ghcr.io and ghcr.io/kenmoini; "ghcr.io/kenmoini/x:1" → kenmoini creds; "docker.io/library/node" → nil; "node:22" (no host) → docker.io entry if present
func TestResolveNpx(t *testing.T)                   // image runtimes.npx, Command ["npx","-y","foo"], Stdin, Name "mcpm-a", npm cache mount, env merged
func TestResolveUvx(t *testing.T)
func TestResolveRunLine(t *testing.T)               // run "docker run -i --rm -e FOO=bar -v /h:/c:ro img --x" → Image "img", Command ["--x"], Env FOO, Mount ro
func TestResolveOverrides(t *testing.T)             // Container.Image wins; Container.Volumes appended
func TestResolveUnknownCommand(t *testing.T)        // "ruby" → error contains "runtimes"
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(sources): registry auth lookup and command resolver"`

### Task 22: Remote resolver and HTTP dialer

**Files:**
- Create: `internal/sources/remote/resolve.go`
- Modify: `internal/proxy/dial.go`
- Test: `internal/sources/remote/resolve_test.go`, `internal/proxy/dial_test.go`

**Interfaces:**
- Produces: `func remote.Resolve(cfg *config.Config, spec config.ServerSpec) (sources.Resolved, error)` (`Kind: KindRemote`, URL parsed and must be http/https, `Transport` default "auto"); `func proxy.DialHTTP(ctx context.Context, url, transport string, headers map[string]string, opts *mcp.ClientOptions) (*mcp.ClientSession, error)`: `http.Client` with a `RoundTripper` that adds headers; `transport == "sse"` → `SSEClientTransport`; `"http"` → `StreamableClientTransport`; `"auto"` → try Streamable, on error try SSE.

- [ ] **Step 1: Write the failing tests**

```go
func TestRemoteResolve(t *testing.T)           // url/headers mapped; "ftp://x" → error
func TestDialHTTPStreamable(t *testing.T)      // httptest from Task 14 → ListTools
func TestDialHTTPSSE(t *testing.T)             // /sse endpoint with transport "sse"
func TestDialHTTPAutoFallsBackToSSE(t *testing.T) // mux with only /sse mounted; transport "auto" with URL of /sse → works
func TestDialHTTPSendsHeaders(t *testing.T)    // httptest handler asserts "Authorization: Bearer t" before delegating
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(sources): remote resolver and HTTP dialer"`

### Task 23: Build source: cloner and builder

**Files:**
- Create: `internal/sources/build/cloner.go`, `internal/sources/build/gogit.go`, `internal/sources/build/builder.go`, `internal/sources/build/resolve.go`
- Test: `internal/sources/build/builder_test.go`, `internal/sources/build/gogit_test.go`, `internal/sources/build/resolve_test.go`

**Interfaces:**
- Produces:

```go
type Cloner interface{ Clone(ctx context.Context, repo, ref, dir string, auth *config.GitAuth) (sha string, err error) }
type GoGitCloner struct{}       // depth 1; ref "" → default branch; branch, tag, or full sha; token → &http.BasicAuth{Username: "x-access-token", Password: token}; ssh_key_file → ssh.NewPublicKeysFromFile("git", path, "")
func (GoGitCloner) authMethod(repo string, a *config.GitAuth) (transport.AuthMethod, error) // exported for test as AuthMethod
type Builder struct{ Workspace string; Engine runtime.Engine; Cloner Cloner }
func (b *Builder) Build(ctx context.Context, plan sources.BuildPlan, force bool) (imageRef string, err error)
// dir = <Workspace>/<name>/src (re-cloned each time); tag = "localhost/mcp-machine/<name>:" + sha[:7]
// if !force && ImageExists(tag) → return tag; else BuildImage{ContextDir: dir/<subdir>, Containerfile: plan.Containerfile, Tag, Args, Labels{io.mcp-machine/server, io.mcp-machine/commit: sha}, Auth: plan.RegistryAuth}
func Resolve(cfg *config.Config, spec config.ServerSpec) (sources.Resolved, error) // Kind container; Image.Build plan; Transport = Serve.Mode; HTTP endpoint when http; Container spec with Name, Network, Env, Stdin (stdio only), Ports (http: ContainerPort = Serve.Port, no host port)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestBuilderClonesAndBuilds(t *testing.T)   // fake cloner copies testdata into dir and returns "0123456789abcdef"; fake engine; Build → tag "localhost/mcp-machine/svc:0123456"; engine.Builds()[0].ContextDir ends with "/svc/src/server"; Args forwarded; Auth forwarded
func TestBuilderSkipsWhenImageExists(t *testing.T) // second Build same sha → no new build; force=true → builds
func TestGoGitCloneAtRef(t *testing.T)          // create bare repo in t.TempDir() with go-git, commit file "a.txt" on main and tag "v1"; Clone(url "file://...", "v1", dir) → file present, sha == tag commit
func TestGoGitAuthMethod(t *testing.T)          // token → *http.BasicAuth with Password token; ssh_key_file with generated ed25519 key → *ssh.PublicKeys; https repo with ssh key → error
func TestBuildResolveHTTPMode(t *testing.T)     // serve http port 3000 path /mcp → HTTP endpoint, Container.Ports[0].ContainerPort 3000, Stdin false
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(sources): git clone and image build pipeline"`

### Task 24: Composite resolver

**Files:**
- Modify: `internal/sources/sources.go`
- Test: `internal/sources/sources_test.go`

**Interfaces:**
- Produces: `func NewResolver() Resolver` dispatching on `spec.Type` to `remote.Resolve`, `command.Resolve`, `build.Resolve`; unknown type → error; every error wrapped `server "<name>": ...`.

- [ ] **Step 1: Write the failing tests** — `TestResolverDispatches` (three types → expected Kind/Transport), `TestResolverWrapsErrors`.

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(sources): composite resolver"`

---

## M5b: Manager

### Task 25: Runner lifecycle for stdio servers

**Files:**
- Create: `internal/manager/manager.go`, `internal/manager/runner.go`, `internal/manager/backoff.go`, `internal/manager/clock.go`
- Test: `internal/manager/runner_test.go`

**Interfaces:**
- Produces:

```go
type Clock interface{ Now() time.Time; After(d time.Duration) <-chan time.Time }
type EndpointFunc func(info runtime.ContainerInfo, ep sources.HTTPEndpoint) (string, error) // implemented in Task 27; nil means DefaultEndpoint
type Options struct{ Clock Clock; MaxRetries int /* 10 */; MaxBackoff time.Duration /* 60s */; ProbeTimeout time.Duration /* 60s */; InContainer bool; SelfID string; EndpointFunc EndpointFunc; Logger *slog.Logger; Version string }
type Builder interface{ Build(ctx context.Context, plan sources.BuildPlan, force bool) (string, error) }
type Manager struct{ /* engine, resolver, builder, registry, runners map[string]*runner, mu, cfg *config.Config */ }
func New(engine runtime.Engine, resolver sources.Resolver, builder Builder, reg *store.Registry, opts Options) *Manager
func (m *Manager) Apply(ctx context.Context, cfg *config.Config) error
func (m *Manager) Start(ctx context.Context, name string) error   // clears Paused
func (m *Manager) Stop(ctx context.Context, name string) error    // sets Paused
func (m *Manager) Restart(ctx context.Context, name string) error
func (m *Manager) Remove(ctx context.Context, name string) error  // stops, removes container, deletes runner and registry entry
func (m *Manager) Rebuild(ctx context.Context, name string) error // build type only; force build then Restart
func (m *Manager) Resync(ctx context.Context, name string) error
func (m *Manager) Facade(name string) (*proxy.Facade, bool)
func (m *Manager) Lookup(name string) (*mcp.Server, bool)        // only when state Ready
func (m *Manager) Logs(ctx context.Context, name string, tail int) ([]runtime.LogLine, error)
func (m *Manager) Close(ctx context.Context) error                // stop all runners, leave containers
```
Runner: goroutine per server, events `start|stop|remove|rebuild|exit(code)|sessionLost`, moves through the spec's states and writes `store.Status` on every transition. Stdio path: Resolve → image (build or pull if missing) → `EnsureNetwork` → `CreateContainer` (labels from `store.Labels(name, hash, transport)`, `Stdin: true`) → `Attach` → `StartContainer` → `go Wait` → `proxy.DialIO(rwc, facade.ClientOptions())` → `facade.Bind` → Ready. On exit or session loss: `Unbind`, `Failed` with error `container exited with code N; last log lines: ...` (uses `Logs(id, 5)`), then after backoff `RemoveContainer(force)` and re-run Creating; after `MaxRetries` stay Failed until `Start`. Stop: `Unbind`, `StopContainer(timeout)`, state Stopped (container kept). Start from Stopped with existing container and same hash: `Attach` → `StartContainer` (no create).

- [ ] **Step 1: Write the failing tests** (helpers: fake engine; stub resolver returning a stdio container `Resolved`; `serveBackend(t, eng, id)` waits for `eng.Peer(id)` and runs a real `mcp.Server` with tool `ping` over `mcp.IOTransport` on the peer; fake clock with `Advance(d)`)

```go
func TestRunnerReachesReady(t *testing.T)        // Apply(cfg with "a") → registry "a" Ready within 3s; Facade("a") ListTools has "ping"; Calls() order contains "CreateContainer","Attach","StartContainer" in that order
func TestRunnerStopAndStart(t *testing.T)        // Stop → Stopped, engine state exited, Lookup false, Status.Paused true; Start → Ready; no second "CreateContainer" in Calls()
func TestRunnerRemove(t *testing.T)              // Remove → registry entry gone, container removed
func TestRunnerRetriesAfterExit(t *testing.T)    // eng.Exit(id, 1) → Failed with "exited with code 1"; clock.Advance(1s) → Ready again, Restarts == 1
func TestRunnerStopsRetryingAfterMax(t *testing.T) // Options.MaxRetries 2; exit repeatedly → stays Failed after 2 retries (Review Focus 5); Start resets
func TestRunnerSurfacesStartError(t *testing.T)  // eng.FailNext("StartContainer", errBoom) → Failed with "boom"
func TestRunnerPullsMissingImage(t *testing.T)   // image not present → Pulls() contains it; present → no pull
func TestRunnerBuildsWhenPlanned(t *testing.T)   // Resolved.Image.Build != nil → fake Builder called, image set to its return
func TestRunnerRestart(t *testing.T)             // Restart → Calls() gains "StopContainer","Attach","StartContainer" and no "CreateContainer"; Ready again
func TestManagerResync(t *testing.T)             // backend adds a tool after Ready; Resync("a") → Facade lists it
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement `manager.go`, `runner.go`, `backoff.go`, `clock.go`** (backoff: `min(1s<<n, MaxBackoff)`).

- [ ] **Step 4: Run tests** — `go test ./internal/manager/ -race -count=3 -v` → PASS (count=3 to shake out races)

- [ ] **Step 5: Commit** — `git commit -m "feat(manager): per-server runner state machine"`

### Task 26: Reconcile, adoption, paused flag

**Files:**
- Create: `internal/manager/reconcile.go`
- Modify: `internal/manager/manager.go`
- Test: `internal/manager/reconcile_test.go`

**Interfaces:**
- `Apply` semantics: desired = enabled servers keyed by name with `config.Hash(spec, spec.RuntimeImage(cfg))`; first Apply lists `ListContainers({managed: true})` and for each parsed label: name in desired with equal hash → adopt (`http`: set ContainerID, go to Running→Connecting; `stdio`: set ContainerID, state Stopped, then Start path); otherwise `StopContainer`+`RemoveContainer`. Existing runner with changed hash → `Remove` then create. Name absent from config → `Remove`. `enabled: false` → `Stop` (not paused). Paused runners are not started by Apply unless their hash changed. A second Apply with the same config is a no-op (no engine calls).

- [ ] **Step 1: Write the failing tests**

```go
func TestApplyRecreatesOnHashChange(t *testing.T)   // change env → "RemoveContainer" then "CreateContainer"; new container label hash differs
func TestApplyRemovesDroppedServer(t *testing.T)
func TestApplyDisabledStops(t *testing.T)           // enabled:false → Stopped, Paused false
func TestApplyIsIdempotent(t *testing.T)            // second Apply → Calls() unchanged
func TestAdoptHttpContainerInPlace(t *testing.T)    // eng.Seed(running container labeled transport http, hash equal) → no CreateContainer; state reaches Connecting (dial fails in this test → Failed, fine) and ContainerID == seeded
func TestAdoptStdioContainerRestarts(t *testing.T)  // seeded stdio container → "Attach","StartContainer" without "CreateContainer"
func TestAdoptStaleHashRecreates(t *testing.T)
func TestPausedSurvivesReload(t *testing.T)         // Stop("a"); Apply(same cfg) → still Stopped; Apply(changed hash) → recreated and Ready
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement `reconcile.go`**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(manager): reconcile desired vs actual with adoption"`

### Task 27: HTTP-mode servers, remote servers, endpoint and probe

**Files:**
- Create: `internal/manager/endpoint.go`, `internal/manager/probe.go`
- Modify: `internal/manager/runner.go`
- Test: `internal/manager/http_test.go`

**Interfaces:**
- Produces: `type EndpointFunc func(info runtime.ContainerInfo, ep sources.HTTPEndpoint) (string, error)`; `func DefaultEndpoint(opts Options) EndpointFunc`: `InContainer` → `http://<info.Name>:<ep.Port><ep.Path>`; else find `info.Ports` mapping for `ep.Port` and return `http://127.0.0.1:<HostPort><ep.Path>` (error if none). `func probe(ctx, url string, timeout, interval time.Duration) error` — any HTTP response (even 4xx/5xx) counts as up; connection refused keeps polling.
- Runner http path: `CreateContainer` with `Stdin: false`, and when `!InContainer` add `PortMapping{HostIP: "127.0.0.1", HostPort: 0, ContainerPort: ep.Port}`; `StartContainer`; `Inspect` for ports; `probe`; `proxy.DialHTTP(url, ep.Transport, nil, facade.ClientOptions())`. Remote path: no engine calls; `DialHTTP(remote.URL, remote.Transport, remote.Headers, ...)`. On first Apply when `InContainer && SelfID != ""`: `ConnectNetwork(SelfID, cfg.Podman.Network)` once (errors logged, not fatal).

- [ ] **Step 1: Write the failing tests**

```go
func TestHttpModeUsesEndpointFuncAndProbe(t *testing.T) // httptest MCP server from Task 14; EndpointFunc returns its URL; Resolved http → no "Attach" call; Ready; ListTools works
func TestHttpModeProbeTimeoutFails(t *testing.T)       // EndpointFunc returns a closed port; ProbeTimeout 300ms → Failed with "probe"
func TestRemoteServerReady(t *testing.T)               // Kind remote → Ready with zero engine calls
func TestDefaultEndpointInContainer(t *testing.T)      // "http://mcpm-a:3000/mcp"
func TestDefaultEndpointPublished(t *testing.T)        // ports mapping 3000→41234 → "http://127.0.0.1:41234/mcp"; missing → error
func TestSelfJoinsNetworkOnce(t *testing.T)            // Options{InContainer: true, SelfID: "self"} → Calls() has exactly one "ConnectNetwork"
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(manager): http-mode and remote servers, endpoint resolution, readiness probe"`

### Task 28: Wire manager and watcher into `serve`

**Files:**
- Modify: `internal/cli/serve.go`
- Test: `internal/cli/serve_test.go`

**Interfaces:**
- `serve` builds `sources.NewResolver()`, `build.Builder{Workspace: cfg.WorkspaceDir, Engine, Cloner: build.GoGitCloner{}}`, `manager.New(...)` with `Options{InContainer, SelfID}` from `runtime.DetectSelf("/run/.containerenv")`, calls `Apply(cfg)`, starts `config.NewWatcher(path, onChange: Apply + reload counter, onError: record)` in a goroutine, passes `manager.Lookup` to `httpadapter.Mount`, and ops `readyz` = engine `Ping` and config loaded. Config state exposed through `type configState struct{ mu; current *config.Config; lastErr error; reloads, reloadErrs uint64 }` with `Current()`, `LastError()`, `Reload() error` (for the web layer in Task 29).

- [ ] **Step 1: Write the failing test**

```go
func TestServeReconcilesAndReloads(t *testing.T) // fake engine; config with one command server; serve; registry via /api/servers (added in Task 29 — for now assert via GET /servers/a/mcp returning non-404 within 3s); rewrite config without the server → /servers/a/mcp 404 within 3s
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** — `make lint test` → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(cli): wire manager, builder, and hot reload into serve"`

---

## M6: Web UI

### Task 29: Dashboard, partials, actions, static, JSON list

**Files:**
- Create: `internal/web/web.go`, `internal/web/pages.go`, `internal/web/partials.go`, `internal/web/templates/{layout,index,server}.html`, `internal/web/templates/partials/{_server_row,_status,_logs,_tools,_connect}.html`, `internal/web/static/app.css`, `internal/web/static/htmx.min.js` (via `make htmx-vendor`)
- Modify: `internal/cli/serve.go` (mount web)
- Test: `internal/web/web_test.go`

**Interfaces:**
- Produces:

```go
type ManagerAPI interface {
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Rebuild(ctx context.Context, name string) error
	Resync(ctx context.Context, name string) error
	Facade(name string) (*proxy.Facade, bool)
	Logs(ctx context.Context, name string, tail int) ([]runtime.LogLine, error)
}
type ConfigView interface{ Current() *config.Config; LastError() error; Reload() error }
type ConfigWriter interface{ AddServer(config.ServerSpec) error; UpdateServer(string, config.ServerSpec) error; DeleteServer(string) error }
type Deps struct{ Manager ManagerAPI; Registry *store.Registry; Config ConfigView; Writer ConfigWriter; PublicURL, Version string; Logger *slog.Logger }
func New(d Deps) http.Handler  // its own ServeMux with the UI routes from the spec; templates parsed once at New via embed.FS; panics on parse error
```
Templates: `layout.html` (nav, flash area, `hx-boost`), `index.html` (table: name, type, state badge, image, since, actions as `hx-post` buttons targeting the row), `server.html` (status partial polled `hx-trigger="every 2s"`, tools/resources/prompts tables, logs `<pre>` polled, connect snippets). Connect snippets use `PublicURL`: `claude mcp add --transport http <name> <PublicURL>/servers/<name>/mcp`, `claude mcp add <name> -- mcp-machine connect <PublicURL>/servers/<name>/mcp`, `npx mcp-remote <PublicURL>/servers/<name>/mcp`. `GET /api/servers` returns `[]store.Status` JSON.

- [ ] **Step 1: Write the failing tests** (fake `ManagerAPI` recording calls; registry pre-filled with two statuses)

```go
func TestIndexListsServersWithStates(t *testing.T)     // body contains "alpha", "beta", `class="badge ready"`
func TestStatusPartialIsFragment(t *testing.T)         // GET /servers/alpha/status with HX-Request: true → no "<html"
func TestActionCallsManagerAndReturnsRow(t *testing.T) // POST /servers/alpha/actions/stop → fake.Stopped == ["alpha"]; body contains `id="row-alpha"`
func TestUnknownActionIs400(t *testing.T)
func TestUnknownServerIs404(t *testing.T)
func TestStaticServedWithCacheControl(t *testing.T)    // GET /static/htmx.min.js → 200, Cache-Control contains "max-age"
func TestAPIServersJSON(t *testing.T)
func TestTemplatesParse(t *testing.T)                  // New(Deps{...}) does not panic
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement** and run `make htmx-vendor` (verify sha256 matches Global Constraints).

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(web): dashboard, partials, actions, static assets"`

### Task 30: Detail page, logs, config page

**Files:**
- Modify: `internal/web/pages.go`, `internal/web/partials.go`, templates
- Create: `internal/web/templates/config.html`
- Test: `internal/web/web_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestServerPageShowsToolsAndSnippets(t *testing.T) // facade bound to a backend with tool "ping" → body contains "ping" and "claude mcp add --transport http alpha http://mcp.lan:8080/servers/alpha/mcp" and "mcp-machine connect" and "npx mcp-remote"
func TestLogsPartialTail(t *testing.T)                 // fake Logs returns 10 lines; ?tail=5 → fake received 5; body has 5 lines
func TestConfigPageShowsReloadError(t *testing.T)      // ConfigView.LastError "server \"a\": duplicate" → body contains it
func TestConfigReloadPost(t *testing.T)                // POST /config/reload → fake.Reloaded true; redirect 303 to /config
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(web): server detail, logs, and config pages"`

### Task 31: Add, edit, delete forms and import

**Files:**
- Create: `internal/web/forms.go`, `internal/web/importpage.go`, `internal/web/templates/{form_remote,form_command,form_build,import}.html`
- Test: `internal/web/forms_test.go`

**Interfaces:**
- Form field names mirror YAML keys with dots: `name`, `type`, `enabled`, `remote.url`, `remote.transport`, `remote.headers` (textarea, one `Key: Value` per line), `command.command`, `command.args` (textarea, one per line), `command.env` (textarea `K=V` lines), `command.run`, `container.image`, `container.volumes` (lines), `build.repo`, `build.ref`, `build.subdir`, `build.containerfile`, `build.args` (lines), `build.auth.token`, `build.auth.ssh_key_file`, `serve.mode`, `serve.port`, `serve.path`, `serve.transport`, `env` (lines). `func specFromForm(v url.Values) (config.ServerSpec, error)` and `func formFromSpec(s config.ServerSpec) url.Values` are pure and round-trip. Validation runs `config.Validate` on a copy of the current config with the entry added or replaced; errors render the form again with messages (422). Success: `Writer.AddServer/UpdateServer`, redirect 303 to `/servers/<name>`. Delete: POST `/servers/{name}/delete` → `Writer.DeleteServer` → 303 to `/`. Import: `POST /servers/import` with field `text`; if it starts with `{` → `clientconfig.Parse`, else `dockerrun.Parse` wrapped as a command spec named from the image; renders a preview table with hidden fields; `confirm=1` adds every entry.

- [ ] **Step 1: Write the failing tests**

```go
func TestNewFormRendersPerType(t *testing.T)        // GET /servers/new?type=build → contains name="build.repo"
func TestCreateRemoteServerWritesAndRedirects(t *testing.T) // POST valid remote → fake Writer.Added[0].Remote.URL; 303 Location /servers/x
func TestCreateInvalidRendersErrors(t *testing.T)   // dup name → 422, body contains "duplicate", Writer not called
func TestEditPrefillsAndKeepsVars(t *testing.T)     // existing header "Bearer ${TOK}" → GET /servers/x/edit contains "${TOK}"; POST unchanged → Writer.Updated spec header still "${TOK}"
func TestDeleteCallsWriter(t *testing.T)
func TestSpecFormRoundTrip(t *testing.T)            // formFromSpec(specFromForm(v)) == v for all three types
func TestImportPreviewAndConfirm(t *testing.T)      // mcpServers JSON with 2 entries → preview lists both; confirm → Writer.Added len 2; docker run line → one command entry with Run set
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests** → PASS

- [ ] **Step 5: Commit** — `git commit -m "feat(web): add, edit, delete, and import servers"`

---

## M7: Packaging and docs

### Task 32: Containerfile, compose, Quadlet, README, docs

**Files:**
- Create: `Containerfile`, `compose.yaml`, `deploy/quadlet/mcp-machine.container`, `deploy/quadlet/mcp-machine.network`, `deploy/deploy_test.go`, `README.md`, `docs/architecture.md`, `docs/config.md`, `docs/clients.md`, `docs/state-machine.md`
- Modify: `Makefile` (`image`, `push`, `compose-up`, `compose-down`, `quadlet-install` targets finalized)

**Interfaces:**
- Containerfile: stage 1 `FROM registry.access.redhat.com/ubi9/go-toolset:1.26 AS build`, `WORKDIR /src`, copy `go.mod go.sum`, `go mod download`, copy source, `CGO_ENABLED=0 go build -tags $GOTAGS -ldflags "..." -o /tmp/mcp-machine ./cmd/mcp-machine`; stage 2 `FROM registry.access.redhat.com/ubi9/ubi-minimal:9.8`, `COPY --from=build /tmp/mcp-machine /usr/local/bin/mcp-machine`, `VOLUME /data`, `EXPOSE 8080`, `ENTRYPOINT ["/usr/local/bin/mcp-machine"]`, `CMD ["serve","-c","/data/config.yaml"]`. Labels `org.opencontainers.image.source=https://github.com/kenmoini/mcp-machine`.
- compose.yaml: service `mcp-machine`, image `ghcr.io/kenmoini/mcp-machine:latest`, `ports: ["8080:8080"]`, volumes `/run/podman/podman.sock:/run/podman/podman.sock` and `./data:/data`, `security_opt: ["label=disable"]`, `restart: unless-stopped`.
- Quadlet: `mcp-machine.network` (`[Network] NetworkName=mcp-machine`), `mcp-machine.container` (`[Container] Image=ghcr.io/kenmoini/mcp-machine:latest`, `PublishPort=8080:8080`, `Volume=/run/podman/podman.sock:/run/podman/podman.sock`, `Volume=/srv/mcp-machine:/data`, `Network=mcp-machine.network`, `SecurityLabelDisable=true`, `AutoUpdate=registry`; `[Service] Restart=always`; `[Install] WantedBy=multi-user.target default.target`).
- README: what it is, quick start (`podman run` one-liner, compose, Quadlet), config overview linking `docs/config.md`, client setup linking `docs/clients.md`, Makefile targets, limitations (v1 non-goals from the spec), dev setup (`MCP_MACHINE_PODMAN_SOCKET` for integration tests).

- [ ] **Step 1: Write the failing test** `deploy/deploy_test.go`

```go
func TestComposeMountsSocketAndData(t *testing.T) // yaml.Unmarshal compose.yaml; services["mcp-machine"].volumes contains "/run/podman/podman.sock:/run/podman/podman.sock" and an entry ending ":/data"
func TestQuadletUnits(t *testing.T)                // .container contains "[Container]", "Image=ghcr.io/kenmoini/mcp-machine", "Network=mcp-machine.network", "Volume=/run/podman/podman.sock"; .network contains "NetworkName=mcp-machine"
func TestContainerfileStages(t *testing.T)         // first FROM line contains "ubi9/go-toolset:1.26"; last FROM contains "ubi9/ubi-minimal:9.8"; contains `CMD ["serve","-c","/data/config.yaml"]`
```

- [ ] **Step 2: Run to verify failure** → FAIL

- [ ] **Step 3: Write the files**

- [ ] **Step 4: Run tests, then build the image**

Run: `make lint test && make image`
Expected: PASS; image builds (requires the podman machine running).

- [ ] **Step 5: Smoke test**

Run: `mkdir -p $TMPDIR/mm && cp config.example.yaml $TMPDIR/mm/config.yaml` (trim to one `npx` server), then `podman run --rm -p 8080:8080 -v /run/podman/podman.sock:/run/podman/podman.sock -v $TMPDIR/mm:/data --security-opt label=disable ghcr.io/kenmoini/mcp-machine:dev` and `curl -s localhost:8080/healthz` → `ok`; `curl -s localhost:8080/api/servers` shows the server reaching `ready`.

- [ ] **Step 6: Commit**

```bash
git add Containerfile compose.yaml deploy docs README.md Makefile
git commit -m "feat: container packaging, compose, quadlet, and docs"
```

---

## Verification (whole plan)

1. `make lint test` green on a clean checkout.
2. `MCP_MACHINE_PODMAN_SOCKET=... make test-integration` green.
3. Image smoke test from Task 32 step 5.
4. `claude mcp add --transport http fs http://localhost:8080/servers/filesystem/mcp && claude mcp list` shows `fs` connected.
5. `mcp-machine connect http://localhost:8080/servers/filesystem/mcp` answers a `tools/list` JSON-RPC line on stdin.
6. `curl -N localhost:8080/servers/filesystem/sse` prints an `event: endpoint` line.
7. Edit `config.yaml` to add a remote server; dashboard shows it ready within a few seconds. Add another through the UI form; the YAML keeps its comments.
8. On a Fedora or RHEL box: `make quadlet-install && systemctl start mcp-machine` serves the same dashboard.
