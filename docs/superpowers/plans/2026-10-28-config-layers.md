# Layered Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Load configuration from an admin-owned base file plus two drop-in directories, and make every UI write land in `/data/config.d/<name>.yaml` instead of the admin's files.

**Architecture:** `internal/config` gains a `Layout` (where the files are), a `Set` (the merged result with per-server `Origin`), a deep-merge for admin layers, a watcher over three directories, and a `DirWriter` that only ever touches the user drop-in directory. `internal/cli` resolves the layout from `-c`, `--system-config-d`, and `--config-d`, keeps the `Set` in `configState`, and passes provenance to the web layer, which shows it and offers Delete, Revert, or nothing per server. The manager learns to keep unrecognised containers when no configuration was found at all. The old in-place YAML `Writer` is deleted: no code path can write an admin file.

**Tech Stack:** Go 1.26 (toolchain pinned in `go.mod`), `gopkg.in/yaml.v3`, `github.com/fsnotify/fsnotify`, `html/template` + htmx (existing), standard `testing`.

**Spec:** `docs/superpowers/specs/2026-10-28-config-layers-design.md` (decisions), with `docs/superpowers/specs/2026-10-28-config-dropins-assessment.md` as the touchpoint inventory.

## Global Constraints

- Layout and load order: `<root>/config.yaml` (base), `<root>/config.d/*.yaml` sorted bytewise (system), `/data/config.d/*.yaml` sorted bytewise (user). Only `.yaml` and `.yml` are read; names starting with `.` and subdirectories are ignored. Symlinks to regular files are followed.
- Global keys (`listen`, `public_url`, `tls`, `podman`, `workspace_dir`, `data_dir`, `allowed_origins`, `registries`, `runtimes`, `log`) are accepted in the base file only. A drop-in decodes as `{servers: [...]}` with unknown keys rejected.
- Admin layers (base, system) deep-merge per server name in load order; the user layer replaces the accumulated definition wholesale. `ApplyDefaults` and `${VAR}` expansion run exactly once, on the merged result.
- An existing but empty or whitespace-only file is an error (existing `errEmptyFile`). Duplicate names inside one file are an error naming the file. Every load error names the file it came from.
- Reload is whole-set atomic: any failing file rejects the whole reload and the last good set stays in effect.
- UI writes go only to the user drop-in directory, one server per `<name>.yaml`, via unique dot-prefixed temp files plus rename, serialized by the existing process-wide `editMu`. Base and system files are never written; the old `config.Writer` is removed.
- Defaults: `serve -c /etc/mcp-machine`, `--system-config-d <root>/config.d` (none in file mode unless given), `--config-d /data/config.d`. Legacy fallback base: `/data/config.yaml`, with a deprecation warning.
- No config found anywhere: `serve` starts with defaults, warns naming the paths searched, shows the warning on the config page, and the reconciler keeps labeled containers it does not recognise. `validate` exits 1 in that case.
- Build and test with the repo's existing tags and sandbox recipe: `go test -tags remote,containers_image_openpgp,exclude_graphdriver_btrfs,exclude_graphdriver_devicemapper ./...`, `make lint`, `make fmt-check`. Listener-based tests (`internal/cli` serve tests, `internal/web` is recorder-based and fine) run outside the sandbox; the controller runs them before each review.
- Depguard boundaries in `.golangci.yml` stay as they are: `internal/config` imports no SDK or podman packages.
- Commit messages use the repo's existing `type: subject` style (`feat:`, `test:`, `docs:`, `build:`).

## Review Focus

1. **Symlinked drop-ins.** An admin symlinks `/etc/mcp-machine/config.d/x.yaml` to a file elsewhere; it must load like a regular file. Pinned in Task 4.
2. **Restart with the admin root unmounted.** A `podman run` that forgot `-v /etc/mcp-machine:...` must not remove the running `mcpm-*` containers. Pinned in Task 7 (manager) and Task 8 (serve end to end).
3. **Editor atomic saves.** vim/VS Code write `.x.yaml.swp` or `x.yaml.tmp` then rename into `x.yaml`; only the final name may trigger a reload, and one burst reloads once. Pinned in Task 5.
4. **Secrets in overrides.** Editing an admin server whose headers hold `${GITHUB_TOKEN}` writes the placeholder, never the value, into `/data/config.d/<name>.yaml`. Pinned in Task 6.
5. **Hand-placed multi-server files in `/data/config.d`.** Their servers load, show as foreign, cannot be deleted, and an edit writes `<name>.yaml`, which wins. Pinned in Task 4 (canonical rule) and Task 6 (writer refusals).

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/config/layout.go` (new) | `Layout`, `LayoutOptions`, `ResolveLayout`, `listDropIns`: where files are, nothing about their contents. |
| `internal/config/load.go` (modify) | `Parse` split into `decode` + `finish`; `decodeDropIn`; `Load(path)` becomes the file-mode wrapper over `Layout`. |
| `internal/config/merge.go` (new) | `mergeServer` and per-section helpers: the admin deep-merge rules. |
| `internal/config/set.go` (new) | `Set`, `Origin`, `OriginFile`, `Layer`; `Layout.Load`, `Layout.LoadRaw`, `Layout.load`. |
| `internal/config/watcher.go` (modify) | `Watcher` over a `Layout`: three directories, late directories, name filter, one debounce. |
| `internal/config/dirwriter.go` (new) | `DirWriter`, `ErrAdminOwned`, `ErrForeignFile`; keeps `editMu`, `notFound`, `writeAtomic` moved from `writer.go`. |
| `internal/config/writer.go` (delete) | The in-place YAML node editor and its tests go away. |
| `internal/manager/manager.go`, `reconcile.go` (modify) | `SetKeepUnknown`; `adopt` skips unknown names when set. |
| `internal/cli/validate.go`, `serve.go`, `serve_state.go`, `cli.go` (modify) | Flags, layout resolution, `configState` over a `Set`, warnings, user-dir creation, `DirWriter` wiring, `validate -v`. |
| `internal/web/web.go`, `pages.go`, `forms.go`, `templates/*` (modify) | `ConfigView` provenance methods, `EditNotice`, delete/revert variants, 409 mapping, config page file list and warning. |
| `Containerfile`, `compose.yaml`, `deploy/quadlet/mcp-machine.container`, `deploy/deploy_test.go`, `Makefile` (modify) | Image default root, read-only admin mount, `CONFIG`/`CONFIG_D` make variables. |
| `config.example.yaml`, `config.example.d/filesystem.yaml` (new), `internal/config/example_test.go` (modify) | Base example plus one drop-in, both loaded by the example test. |
| `docs/config.md`, `README.md`, `docs/architecture.md`, `docs/Parked.md` (modify) | Layers, flags, mounts, migration note, parked-item updates. |

---

### Task 1: Layout resolution

**Files:**
- Create: `internal/config/layout.go`
- Test: `internal/config/layout_test.go`

**Interfaces:**
- Produces:
  ```go
  type LayoutOptions struct {
      Path       string // -c: the base file, or the admin root directory
      SystemDir  string // --system-config-d; "" means <root>/config.d in root mode, none in file mode
      UserDir    string // --config-d; "" means no user layer
      LegacyFile string // upgrade fallback base, e.g. /data/config.yaml; "" disables the fallback
  }
  type Layout struct {
      BaseFile  string   // "" when there is no base file
      SystemDir string   // "" when there is no system drop-in directory
      UserDir   string   // "" when there is no user drop-in directory
      Fallback  string   // == BaseFile when the legacy file is in use, else ""
      Searched  []string // every path consulted, for the "no configuration found" warning
  }
  func ResolveLayout(o LayoutOptions) (Layout, error)
  func listDropIns(dir string) ([]string, error) // sorted full paths; nil, nil when dir does not exist
  ```

Resolution rules (the spec's "CLI and packaging" section):
- `Path` is a directory when `os.Stat` says so, or when it does not exist and has no `.yaml`/`.yml` extension. Root mode: `BaseFile = Path/config.yaml`, `SystemDir = SystemDir or Path/config.d`.
- Otherwise file mode: `BaseFile = Path`, `SystemDir = SystemDir` as given (may be empty).
- Upgrade fallback, root mode only: when `BaseFile` does not exist and `listDropIns(SystemDir)` is empty and `LegacyFile` exists, set `BaseFile = LegacyFile` and `Fallback = LegacyFile`.
- `Searched` lists, in order: the root-mode base path (or file-mode path), `SystemDir` when set, `LegacyFile` when consulted, `UserDir` when set.
- Any `os.Stat` error other than not-exist is returned as `config: stat <path>: <err>`.
- `listDropIns` uses `os.ReadDir`, keeps entries whose name has `.yaml` or `.yml` extension, does not start with `.`, and that `os.Stat` (following symlinks) reports as a regular file; returns them sorted bytewise (`os.ReadDir` order) as full paths.

- [ ] **Step 1: Write the failing tests**

```go
func TestResolveLayoutRootMode(t *testing.T)        // dir exists with config.yaml → BaseFile, SystemDir=<root>/config.d, UserDir passed through, Fallback ""
func TestResolveLayoutMissingRootIsRootMode(t *testing.T) // Path "/nonexistent/mcp-machine" → root mode, BaseFile "/nonexistent/mcp-machine/config.yaml"
func TestResolveLayoutFileMode(t *testing.T)        // Path ".../config.yaml" (exists) → BaseFile=Path, SystemDir "" when not given, given value otherwise
func TestResolveLayoutMissingYamlIsFileMode(t *testing.T) // Path ".../missing.yml" → file mode with that BaseFile
func TestResolveLayoutLegacyFallback(t *testing.T)  // root has no base and empty config.d, LegacyFile exists → BaseFile==Fallback==LegacyFile
func TestResolveLayoutNoFallbackWhenSystemDropInExists(t *testing.T) // root/config.d/a.yaml exists → Fallback ""
func TestResolveLayoutSearchedOrder(t *testing.T)   // Searched == [root/config.yaml, root/config.d, legacy, user]
func TestListDropInsFilters(t *testing.T)           // a.yaml, b.yml, .hidden.yaml, c.txt, sub/d.yaml, symlink e.yaml→regular file → [a.yaml, b.yml, e.yaml]; missing dir → nil, nil
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestResolveLayout|TestListDropIns' -v`
Expected: FAIL, `undefined: config.ResolveLayout`

- [ ] **Step 3: Implement `layout.go`**

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestResolveLayout|TestListDropIns' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/layout.go internal/config/layout_test.go
git commit -m "feat(config): resolve the base file and drop-in directories into a Layout"
```

---

### Task 2: Split `Parse` and add drop-in decoding

**Files:**
- Modify: `internal/config/load.go:31-43`
- Test: `internal/config/load_test.go`

**Interfaces:**
- Produces:
  ```go
  func decode(data []byte) (*Config, error)                 // KnownFields, EOF tolerated, no defaults, no expansion
  func finish(c *Config, lookup func(string) (string, bool)) error // ApplyDefaults then expandEnv
  func decodeDropIn(data []byte) ([]ServerSpec, error)      // decodes {servers: [...]} with KnownFields; a global key is an error whose text contains the key
  ```
- `Parse(data, lookup)` keeps its signature and becomes `decode` + `finish` (its existing tests must still pass unchanged).

- [ ] **Step 1: Write the failing tests**

```go
func TestDecodeDropInServersOnly(t *testing.T)   // "servers:\n  - {name: a, type: remote, remote: {url: https://x/}}\n" → one spec, Remote.Transport still "" (no defaults)
func TestDecodeDropInRejectsGlobalKey(t *testing.T) // "listen: :1\nservers: []\n" → error containing "listen"
func TestDecodeDropInEmptyServers(t *testing.T)  // "servers: []\n" → empty slice, nil error
func TestParseStillAppliesDefaultsAndExpands(t *testing.T) // Parse of a remote server with headers {A: "${X}"} and lookup X→"1" → Transport "auto", header "1"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestDecodeDropIn|TestParseStill' -v`
Expected: FAIL, `undefined: decodeDropIn`

- [ ] **Step 3: Implement the split in `load.go`**

`decodeDropIn` decodes into `struct{ Servers []ServerSpec \`yaml:"servers"\` }` with `dec.KnownFields(true)`.

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/config/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/load.go internal/config/load_test.go
git commit -m "refactor(config): split Parse into decode and finish; decode drop-in files"
```

---

### Task 3: Admin deep-merge

**Files:**
- Create: `internal/config/merge.go`
- Test: `internal/config/merge_test.go`

**Interfaces:**
- Produces:
  ```go
  // mergeServer refines a with b (a later admin definition of the same name).
  // It returns a fresh spec sharing no maps, slices, or section pointers with
  // a or b. Error text on a type mismatch: `server "x": type "command" does not match "remote"`.
  func mergeServer(a, b ServerSpec) (ServerSpec, error)
  func cloneServer(s ServerSpec) ServerSpec   // deep copy, used by the user-layer replace
  ```

Rules (spec "Deep-merge rules"), applied field by field:
- `Type`: equal when both set, else error; `b.Type` when `a.Type == ""`.
- Strings and ints: `b` wins when non-zero.
- `Enabled *bool`: `b.Enabled` when non-nil.
- Maps (`Env`, `Remote.Headers`, `Command.Env`, `Container.Env`, `Build.Args`): new map, `a`'s entries then `b`'s; nil when both nil.
- Slices (`Command.Args`, `Container.Volumes`): copy of `b` when `b != nil`, else copy of `a`.
- Sections (`Remote`, `Command`, `Build`, `Serve`, `Container`, `Build.Auth`): recursive merge when both present, deep copy of the present one otherwise.

Write one small helper per shape (`mergeString`, `mergeInt`, `mergeMap`, `mergeSlice`) and one function per section; no reflection.

- [ ] **Step 1: Write the failing tests**

```go
func TestMergeServerScalarsNonZeroWins(t *testing.T)   // Remote.URL set only in a stays; b.Remote.Transport "sse" overrides "http"; b.Serve.Port 0 leaves a's 3000
func TestMergeServerEnabledPointer(t *testing.T)       // a Enabled nil, b false → false; a false, b nil → false; a false, b true → true
func TestMergeServerMapsKeyWise(t *testing.T)          // Env {A:1,B:2} + {B:3,C:4} → {A:1,B:3,C:4}; result map is not a's map (mutating result leaves a unchanged)
func TestMergeServerSlicesReplace(t *testing.T)        // Args [x,y] + nil → [x,y]; [x,y] + [] → []; [x,y] + [z] → [z]
func TestMergeServerSectionsRecursive(t *testing.T)    // a has Build{Repo, Auth{Token}}, b has Build{Ref, Auth{SSHKeyFile}} → all four present
func TestMergeServerTypeMismatch(t *testing.T)         // a remote, b command → error text `server "x": type "command" does not match "remote"`
func TestMergeServerDoesNotAlias(t *testing.T)         // after merge, modify result.Remote.Headers and result.Command.Args; a and b unchanged
func TestCloneServerDeep(t *testing.T)                 // clone, mutate clone's maps/slices/sections; original unchanged
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestMergeServer|TestCloneServer' -v`
Expected: FAIL, `undefined: mergeServer`

- [ ] **Step 3: Implement `merge.go`**

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestMergeServer|TestCloneServer' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/merge.go internal/config/merge_test.go
git commit -m "feat(config): deep-merge server definitions across admin layers"
```

---

### Task 4: Layered load with provenance

**Files:**
- Create: `internal/config/set.go`
- Modify: `internal/config/load.go:53-69` (`Load` becomes the file-mode wrapper)
- Test: `internal/config/set_test.go`, `internal/config/hash_test.go` (add one test)

**Interfaces:**
- Consumes: `Layout`, `listDropIns` (Task 1); `decode`, `finish`, `decodeDropIn`, `errEmptyFile` (Task 2); `mergeServer`, `cloneServer` (Task 3).
- Produces:
  ```go
  type Layer int
  const (
      LayerBase Layer = iota
      LayerSystem
      LayerUser
  )
  func (l Layer) String() string // "base", "system", "user"

  type OriginFile struct {
      Path  string
      Layer Layer
  }
  // Origin lists the files that define a server, in the order they were
  // applied. The last LayerUser entry, if any, is the effective user definition.
  type Origin struct{ Files []OriginFile }
  func (o Origin) Admin() bool      // any file with Layer != LayerUser
  func (o Origin) UserFile() string // path of the last LayerUser entry, "" if none
  func (o Origin) String() string   // see below

  type Set struct {
      Config   *Config           // merged, defaulted, expanded
      Files    []string          // every file loaded, in load order (empty when nothing was found)
      Origins  map[string]Origin // per server name
      Fallback string            // Layout.Fallback
      Searched []string          // Layout.Searched
  }
  func (l Layout) Load() (*Set, error)    // real environment; applies MCP_MACHINE_PODMAN_SOCKET like Load does today
  func (l Layout) LoadRaw() (*Set, error) // lookup returns "${NAME}" for every NAME; no socket override
  func (l Layout) load(lookup func(string) (string, bool)) (*Set, error)
  ```
- `Load(path string) (*Config, error)` keeps its signature: `Layout{BaseFile: path}.Load()` returning `set.Config`; its existing tests pass unchanged.

`Origin.String()` copy, exact: admin files joined with `", "`, each as `<path> (base)` or `<path> (system)`, prefixed `Defined in `; when a user file exists and there are admin files, append `; overridden by <path> (user)`; when only a user file exists, `Defined in <path> (user)`.

Load algorithm (the spec's "Layout and load order"):
1. Base: if `BaseFile` exists, read once; empty → `config: <file>: file is empty` (wrap `errEmptyFile`); `decode`; a decode error is `config: <file>: <err>`. If it does not exist, start from `&Config{}`.
2. For each drop-in file of `SystemDir` then `UserDir` (`listDropIns`): read, empty check, `decodeDropIn`, errors prefixed `config: <file>: `.
3. Per file, duplicate names within the file → `config: <file>: duplicate server "x"`.
4. Apply in order. Admin layers: first definition is `cloneServer`, later ones `mergeServer`; a mismatch error becomes `config: <fileB>: <err> (defined in <fileA>, ...)`. User layer: `cloneServer` replaces; **canonical rule**: within the user layer, a definition of `name` found in the file whose base name is exactly `name + ".yaml"` is applied after all other user files, so `<name>.yaml` wins regardless of sort order; `Origin.Files` records application order.
5. Keep the base file's server order, appending new names in the order first seen (deterministic; `Validate` does not care, the UI does).
6. `finish(cfg, lookup)` once on the merged config; in `Load`, then apply the `MCP_MACHINE_PODMAN_SOCKET` override.
7. `Files` lists only files that were read; `Origins` is keyed by name.

- [ ] **Step 1: Write the failing tests** (helpers: a `layoutDir(t, files map[string]string) Layout` that writes files under `t.TempDir()` with `config.yaml`, `config.d/`, `user.d/`)

```go
func TestLoadBaseOnly(t *testing.T)                 // base with globals + one server → Files [base], Origin "Defined in <base> (base)"
func TestLoadOrderBaseSystemUser(t *testing.T)      // base a; config.d/{10-x.yaml,20-y.yaml}; user.d/z.yaml → Files in that order; server order a,x,y,z
func TestLoadAdminMergeRefines(t *testing.T)        // base a remote transport http headers{A:1}; system a {transport: sse, headers{B:2}} → sse, {A:1,B:2}; Origin.Files has both, Admin() true, UserFile() ""
func TestLoadUserReplacesWholesale(t *testing.T)    // base a with headers{A:1}; user.d/a.yaml a without headers → Headers nil; Origin.String() == "Defined in <base> (base); overridden by <user>/a.yaml (user)"
func TestLoadUserCanonicalWins(t *testing.T)        // user.d/zzz.yaml defines a (transport sse) and b; user.d/a.yaml defines a (transport http) → a http; Origin(a).UserFile()==a.yaml; Origin(b).UserFile()==zzz.yaml
func TestLoadUserTypeChangeAllowed(t *testing.T)    // base a remote; user a command → no error, Type command
func TestLoadAdminTypeMismatch(t *testing.T)        // base a remote; system a command → error containing the system file path and `does not match`
func TestLoadDuplicateInOneFile(t *testing.T)       // system file with a twice → error `config: <file>: duplicate server "a"`
func TestLoadGlobalKeyInDropIn(t *testing.T)        // system file "listen: :1" → error containing file path and "listen"
func TestLoadEmptyDropIn(t *testing.T)              // system file "  \n" → error containing file path and "file is empty"
func TestLoadMissingBaseIsFine(t *testing.T)        // no base, one system drop-in → Config.Listen ":8080" (defaults), one server
func TestLoadNothingFound(t *testing.T)             // nothing exists → Files empty, Config defaults, Searched from Layout, nil error
func TestLoadSymlinkedDropIn(t *testing.T)          // config.d/x.yaml → symlink to a regular file elsewhere → loaded (Review Focus 1)
func TestLoadRawKeepsPlaceholders(t *testing.T)     // header "${TOK}" → LoadRaw keeps "${TOK}"; Load with TOK unset errors; Load with TOK=1 expands
func TestLoadSocketEnvOverride(t *testing.T)        // t.Setenv(MCP_MACHINE_PODMAN_SOCKET) → Load applies it, LoadRaw does not
func TestLoadFileWrapperUnchanged(t *testing.T)     // config.Load(path) of a one-file config equals Layout{BaseFile: path}.Load().Config
```

In `hash_test.go`:

```go
func TestHashStableAcrossLayers(t *testing.T) // the same final server from (base only) vs (base: name/type/remote.url; system: transport+headers) → equal config.Hash(spec, spec.RuntimeImage(cfg))
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestLoad|TestHashStable' -v`
Expected: FAIL, `undefined: Set` / method `Load` on `Layout`

- [ ] **Step 3: Implement `set.go` and rewrite `Load` in `load.go`**

- [ ] **Step 4: Run the package**

Run: `go test ./internal/config/`
Expected: PASS (including the pre-existing `load_test.go`, `watcher_test.go`, `writer_test.go`)

- [ ] **Step 5: Commit**

```bash
git add internal/config/set.go internal/config/set_test.go internal/config/load.go internal/config/hash_test.go
git commit -m "feat(config): load base, system and user layers into one Set with provenance"
```

---

### Task 5: Watcher over the layout

**Files:**
- Modify: `internal/config/watcher.go` (whole file)
- Modify: `internal/config/watcher_test.go` (update the three `NewWatcher` call sites; add tests)
- Modify: `internal/cli/serve.go:102` (compile fix only: pass `config.Layout{BaseFile: path}` and take `*config.Set`; Task 8 does the real wiring)

**Interfaces:**
- Consumes: `Layout`, `Set`, `Layout.Load`, `Validate`.
- Produces:
  ```go
  func NewWatcher(l Layout, onChange func(*Set), onError func(error)) (*Watcher, error)
  func (w *Watcher) Reload() error            // Load + Validate, then onChange or onError
  func (w *Watcher) Run(ctx context.Context) error
  func (w *Watcher) Close() error
  ```

Behavior:
- Watch directories: `filepath.Dir(BaseFile)` when `BaseFile != ""`, `SystemDir`, `UserDir`, deduplicated. A directory that does not exist yet is recorded as pending and its parent is watched (if the parent exists); on an event whose name equals a pending directory that now exists, `fsw.Add` it and reset the debounce timer.
- An event is relevant when `ev.Name == BaseFile`, or its directory is `SystemDir`/`UserDir` and its base name has a `.yaml`/`.yml` extension and does not start with `.`, or it names a pending directory.
- `NewWatcher` errors when all three paths are empty: `config: watcher has nothing to watch`.
- One `debounceInterval` (250 ms) timer for everything else as today; callbacks run on `Run`'s goroutine.

- [ ] **Step 1: Update the existing tests to the new signature and write the failing ones**

```go
func TestWatcherReloadsOnDropInCreate(t *testing.T)       // base + empty config.d; create config.d/x.yaml → onChange with 2 servers
func TestWatcherIgnoresTempAndHiddenNames(t *testing.T)   // write config.d/.x.yaml.swp and config.d/x.yaml.tmp → no reload within 2×debounce; rename x.yaml.tmp→x.yaml → exactly one reload (Review Focus 3)
func TestWatcherImportBurstReloadsOnce(t *testing.T)      // create 5 user drop-ins within 50 ms → onChange called once (count after 3×debounce)
func TestWatcherLateUserDir(t *testing.T)                 // UserDir does not exist at NewWatcher; mkdir it, then create a.yaml → reload with that server
func TestWatcherInvalidDropInKeepsLastGood(t *testing.T)  // write config.d/bad.yaml "listen: :1" → onError with the file path; onChange not called; fix file → onChange
func TestWatcherNothingToWatch(t *testing.T)              // Layout{} → error "config: watcher has nothing to watch"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestWatcher' -v`
Expected: FAIL (compile error on the new signature)

- [ ] **Step 3: Implement the layout watcher; adjust the `NewWatcher` call in `serve.go` so the tree compiles** (`state.loaded(next.Config)` for now)

- [ ] **Step 4: Run config and cli packages**

Run: `go test ./internal/config/ && go build ./... && go vet ./internal/cli/`
Expected: PASS / no output

- [ ] **Step 5: Commit**

```bash
git add internal/config/watcher.go internal/config/watcher_test.go internal/cli/serve.go
git commit -m "feat(config): watch the base file and both drop-in directories with one debounce"
```

---

### Task 6: DirWriter replaces the in-place YAML writer

**Files:**
- Create: `internal/config/dirwriter.go`, `internal/config/dirwriter_test.go`
- Delete: `internal/config/writer.go`, `internal/config/writer_test.go`
- Modify: `internal/cli/serve.go:313` (compile fix: `config.DirWriter{Dir: ..., Layout: ...}`; Task 8 finalises)

**Interfaces:**
- Consumes: `Layout.LoadRaw`, `Origin`, `decodeDropIn`, `ApplyDefaults`, `Validate`, `cloneServer`.
- Produces:
  ```go
  var ErrAdminOwned  = errors.New("config: server is defined by the administrator and cannot be deleted")
  var ErrForeignFile = errors.New("config: server is defined in a file the UI does not manage")

  type DirWriter struct {
      Dir    string // the user drop-in directory; files are written as <Dir>/<name>.yaml
      Layout Layout // the full layout, for validating the merged set before a write
  }
  func (w DirWriter) AddServer(s ServerSpec) error
  func (w DirWriter) UpdateServer(name string, s ServerSpec) error
  func (w DirWriter) DeleteServer(name string) error
  func (w DirWriter) path(name string) string // filepath.Join(w.Dir, name+".yaml")
  ```
- Keep from `writer.go`, moved into `dirwriter.go`: `editMu`, `notFound`, `writeAtomic` (change its temp pattern to `".<base>-*.tmp"` so the watcher filter ignores it).

Rules (spec "UI write path"):
- Every method takes `editMu`, then `w.Layout.LoadRaw()`; a load error is returned as is.
- **canonical check** `ownFile(name) (exists bool, exclusive bool, err error)`: reads `path(name)` if it exists and decodes it; `exclusive` when it defines exactly one server named `name`. A file that cannot be decoded counts as not exclusive.
- `AddServer`: name in `set.Origins` → `config: server "x" already exists`; `path(name)` exists → `config: %s already exists`; else validate and write.
- `UpdateServer(name, s)`: `s.Name != name` → `config: server name cannot change`; `name` not in `set.Origins` → `notFound`; `path(name)` exists and is not exclusive → `fmt.Errorf("%w: %s", ErrForeignFile, path)`; else validate and write (creates or replaces the file).
- `DeleteServer(name)`: not in `set.Origins` → `notFound`; `o.UserFile() == ""`: `o.Admin()` → `ErrAdminOwned`, else `notFound`; `o.UserFile() != path(name)` or not exclusive → `fmt.Errorf("%w: %s", ErrForeignFile, o.UserFile())`; else `os.Remove(path(name))`.
- Validation before a write: copy `set.Config`, replace or append `s` in `Servers`, `cloneServer` each, `ApplyDefaults`, `Validate`; any error is returned and nothing is written.
- File content: `yaml.v3` encoder with indent 2 of `struct{ Servers []ServerSpec \`yaml:"servers"\` }{[]ServerSpec{s}}`; mode `0o640`; `os.MkdirAll(w.Dir, 0o750)` before the first write.

- [ ] **Step 1: Write the failing tests** (reuse the `layoutDir` helper from Task 4; export nothing new)

```go
func TestDirWriterAddWritesOneFile(t *testing.T)          // Add gamma → user.d/gamma.yaml exists, mode 0640, decodes to exactly gamma; base bytes unchanged (compare before/after)
func TestDirWriterAddRefusesExistingName(t *testing.T)    // base has alpha → Add alpha → error "already exists"; no file written
func TestDirWriterAddRefusesExistingFile(t *testing.T)    // user.d/gamma.yaml defines delta → Add gamma → error naming the file
func TestDirWriterAddKeepsPlaceholders(t *testing.T)      // spec header "${TOK}" → file bytes contain "${TOK}" (Review Focus 4)
func TestDirWriterAddValidatesMergedSet(t *testing.T)     // Add a spec with an invalid name → Validate error, no file
func TestDirWriterUpdateOverridesAdmin(t *testing.T)      // base alpha; Update alpha with transport sse → user.d/alpha.yaml written; LoadRaw shows sse and Origin "overridden by"
func TestDirWriterUpdateReplacesWholesale(t *testing.T)   // base alpha headers{A}; Update alpha without headers → merged Headers nil
func TestDirWriterUpdateRefusesForeignOwnFile(t *testing.T) // user.d/alpha.yaml defines alpha and beta → Update alpha → ErrForeignFile naming the file
func TestDirWriterUpdateNameChange(t *testing.T)          // Update("alpha", spec named "beta") → error "name cannot change"
func TestDirWriterUpdateNotFound(t *testing.T)
func TestDirWriterDeleteUserOnly(t *testing.T)            // user.d/gamma.yaml only → Delete gamma → file gone
func TestDirWriterDeleteRevertsOverride(t *testing.T)     // base alpha + user.d/alpha.yaml → Delete alpha → override removed, LoadRaw shows base alpha, Origin.Admin() true, UserFile() ""
func TestDirWriterDeleteAdminRefused(t *testing.T)        // base alpha, no override → errors.Is(err, ErrAdminOwned); base bytes unchanged
func TestDirWriterDeleteForeignRefused(t *testing.T)      // user.d/zzz.yaml defines gamma → Delete gamma → ErrForeignFile and message contains "zzz.yaml"; file still there (Review Focus 5)
func TestDirWriterConcurrentAdds(t *testing.T)            // 10 goroutines Add distinct names → 10 files, LoadRaw has all 10, no error
func TestDirWriterCreatesDir(t *testing.T)                // Dir missing → Add creates it with 0750
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestDirWriter' -v`
Expected: FAIL, `undefined: DirWriter`

- [ ] **Step 3: Implement `dirwriter.go`; delete `writer.go` and `writer_test.go`; fix the `serve.go` call site** (temporary until Task 8: `config.DirWriter{Dir: filepath.Join(filepath.Dir(*path), "config.d"), Layout: config.Layout{BaseFile: *path}}`)

- [ ] **Step 4: Run config and cli packages, lint**

Run: `go test ./internal/config/ && go build ./... && make lint`
Expected: PASS, 0 issues (the `unused` linter will flag anything left over from `writer.go`)

- [ ] **Step 5: Commit**

```bash
git add -A internal/config internal/cli/serve.go
git commit -m "feat(config): DirWriter writes one server per file under the user drop-in directory; drop the in-place YAML writer"
```

---

### Task 7: Manager keeps unknown containers when asked

**Files:**
- Modify: `internal/manager/manager.go` (field + setter), `internal/manager/reconcile.go:37-80,242-281`
- Test: `internal/manager/reconcile_test.go` (add)

**Interfaces:**
- Produces:
  ```go
  // SetKeepUnknown makes Apply leave labeled containers of servers that are
  // not in the configuration alone (logged at info) instead of removing them.
  // Stale containers of known servers are still replaced.
  func (m *Manager) SetKeepUnknown(keep bool)
  ```
- `adopt` gains an `inConfig map[string]bool` parameter (already computed in `Apply`).

- [ ] **Step 1: Write the failing tests** (use the fake engine and the existing reconcile test helpers)

```go
func TestApplyKeepsUnknownContainersWhenSet(t *testing.T)   // fake engine has a labeled container for "ghost" (not in config); SetKeepUnknown(true); Apply → container still listed; log contains "keeping unknown container"
func TestApplyRemovesUnknownContainersByDefault(t *testing.T) // same without SetKeepUnknown → removed (pins today's behavior)
func TestApplyKeepUnknownStillReplacesStaleKnown(t *testing.T) // SetKeepUnknown(true); labeled container for "a" with a stale hash → replaced
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/manager/ -run 'TestApplyKeep|TestApplyRemovesUnknown' -v`
Expected: FAIL, `undefined: SetKeepUnknown`

- [ ] **Step 3: Implement** (`atomic.Bool` on `Manager`; in `adopt`, before the stale-removal branch: `if m.keepUnknown.Load() && !inConfig[lbl.Name] { log info "manager: keeping unknown container"; continue }`)

- [ ] **Step 4: Run the manager package**

Run: `go test ./internal/manager/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/manager
git commit -m "feat(manager): optionally keep labeled containers of unknown servers"
```

---

### Task 8: CLI wiring: flags, layout, state, warnings, validate -v

**Files:**
- Modify: `internal/cli/validate.go` (whole file), `internal/cli/serve.go:84-120,234-320`, `internal/cli/serve_state.go` (whole file), `internal/cli/cli.go:66-78` (usage text)
- Test: `internal/cli/serve_test.go`, `internal/cli/serve_state_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `config.ResolveLayout`, `Layout.Load/LoadRaw`, `Set`, `Origin`, `NewWatcher(Layout, ...)`, `DirWriter`, `Manager.SetKeepUnknown`.
- Produces (used by Task 9 through the `web.ConfigView` interface):
  ```go
  const (
      defaultConfigPath = "/etc/mcp-machine"
      defaultUserConfigD = "/data/config.d"
      legacyConfigPath  = "/data/config.yaml"
  )
  type configFlags struct{ path, systemDir, userDir string }
  func addConfigFlags(fs *flag.FlagSet) *configFlags // -c, --system-config-d, --config-d with the defaults above
  func (f *configFlags) layout() (config.Layout, error) // ResolveLayout with LegacyFile: legacyConfigPath
  func loadSet(l config.Layout) (*config.Set, error)   // Load + Validate

  // configState additions
  func (s *configState) Files() []string
  func (s *configState) Origin(name string) (config.Origin, bool)
  func (s *configState) Warning() string
  func (s *configState) Raw() (*config.Config, error) // now s.layout.LoadRaw().Config
  ```

Behavior:
- `serve`: resolve layout; `loadSet`; `--listen` override as today. Warnings (logged at warn and returned by `Warning()`), exact copy:
  - fallback: `configuration read from the deprecated location /data/config.yaml; move it to /etc/mcp-machine/config.yaml` (paths from the layout).
  - nothing found: `no configuration found (searched: p1, p2, ...); running with defaults`.
- `mgr.SetKeepUnknown(len(set.Files) == 0)` at startup and inside the watcher's `onChange`.
- User directory: when `UserDir != ""`, `os.MkdirAll(UserDir, 0o750)` then a write probe (`os.CreateTemp(UserDir, ".probe-*")`, removed). On failure: log warn, `Writer: nil`, `web.Deps.EditNotice = "editing is disabled: <UserDir> is not writable (<err>)"`. On success: `Writer: config.DirWriter{Dir: UserDir, Layout: l}`.
- `configState` holds `layout config.Layout` and `set *config.Set` (replacing `path`); `loaded(set)`; `Current()` returns `set.Config`.
- `validate`: same flags plus `-v`; nothing found → stderr `mcp-machine validate: no configuration found (searched: ...)`, exit 1; with `-v`, after `ok` print `files:` then one line per file, then `servers:` then one `  <name>: <Origin.String()>` per server (text, not JSON).
- Usage text in `cli.go`: `serve      run the proxy and web UI (-c /etc/mcp-machine, --system-config-d, --config-d /data/config.d)` and the same note for `validate`.
- `TestServeMissingConfigExits1` changes meaning: a missing explicit file now starts with defaults; rewrite it as `TestServeNoConfigStartsWithWarning`.

- [ ] **Step 1: Write the failing tests** (these bind listeners; the controller runs them outside the sandbox)

```go
// serve_test.go
func TestServeRootModeLoadsDropIns(t *testing.T)       // -c <dir> with config.yaml (listen only) and config.d/a.yaml (remote a) → /servers/a/mcp serves tools
func TestServeHotReloadsUserDropIn(t *testing.T)       // --config-d <dir>; after start, write <dir>/b.yaml → /servers/b/mcp serves within 5s; remove it → 404 within 5s
func TestServeInvalidDropInKeepsLastGood(t *testing.T) // write config.d/bad.yaml "listen: :1" → /config page shows the file path in "Last reload failed"; a still served
func TestServeNoConfigStartsWithWarning(t *testing.T)  // -c /nonexistent --config-d <tmp>/config.d → healthz 200; stderr contains "no configuration found (searched:"; GET /config contains the same text
func TestServeKeepsContainersWithoutConfig(t *testing.T) // startServeWith a fake engine pre-seeded with a labeled container; no config → container still present after readyz (Review Focus 2)
func TestServeLegacyFallbackWarns(t *testing.T)        // root dir empty, legacy file at <tmp>/data/config.yaml (override legacyConfigPath via a package var for tests, see note) → served; stderr contains "deprecated location"
func TestServeReadOnlyUserDirDisablesEditing(t *testing.T) // --config-d pointing at a file (not a dir) → GET / has no "Add server" link; GET /servers/new → 503; GET /config shows "editing is disabled"
func TestValidateVerboseListsFilesAndOrigins(t *testing.T) // validate -v -c <dir> → stdout has "ok", "files:", both paths, "servers:", "  a: Defined in ..."
func TestValidateNoConfigExits1(t *testing.T)
// serve_state_test.go
func TestConfigStateRawKeepsPlaceholders(t *testing.T) // existing test, now via a Layout; unchanged assertion
func TestConfigStateOriginAndFiles(t *testing.T)       // after loaded(set): Files() == set.Files, Origin("a") found, Origin("zz") not
```

Note on the legacy path: make `legacyConfigPath` a package-level `var` so `TestServeLegacyFallbackWarns` can point it into a temp dir with `t.Cleanup` restoring it; it is still only set by tests.

- [ ] **Step 2: Run the tests to verify they fail**

Run (outside the sandbox): `go test ./internal/cli/ -run 'TestServeRootMode|TestServeHotReloadsUser|TestServeInvalidDropIn|TestServeNoConfig|TestServeKeeps|TestServeLegacy|TestServeReadOnly|TestValidateVerbose|TestValidateNoConfig|TestConfigStateOrigin' -v`
Expected: FAIL (unknown flags / undefined symbols)

- [ ] **Step 3: Implement the wiring** (`web.Deps.EditNotice` and the three `ConfigView` methods are added to the `web` package in this task as interface members and a field only; Task 9 renders them. The `fakeConfig` in `internal/web/web_test.go` must gain the three methods so `web` still compiles: `Files() []string { return c.files }`, `Origin(name) (config.Origin, bool)` from a `origins map[string]config.Origin`, `Warning() string { return c.warning }`.)

- [ ] **Step 4: Run cli, web, and the full tree**

Run (outside the sandbox): `go build ./... && go test ./internal/cli/ ./internal/web/ && make lint`
Expected: PASS, 0 issues

- [ ] **Step 5: Commit**

```bash
git add internal/cli internal/web/web.go internal/web/web_test.go
git commit -m "feat(cli): serve and validate read the admin root, system and user drop-ins; warn on fallback or missing config"
```

---

### Task 9: Web: provenance, delete/revert variants, notices

**Files:**
- Modify: `internal/web/web.go` (`layoutData.EditNotice`), `internal/web/pages.go` (`specView.Provenance`, `configData.Files/Warning`, `configServer.Origin`), `internal/web/forms.go:392-560` (delete handler, `formData.Delete`), `internal/web/templates/{layout.html,server.html,config.html,partials/_form_delete.html}`
- Test: `internal/web/web_test.go`, `internal/web/forms_test.go`

**Interfaces:**
- Consumes: `ConfigView.Files/Origin/Warning`, `Deps.EditNotice` (Task 8), `config.ErrAdminOwned`, `config.ErrForeignFile` (Task 6).
- Produces:
  ```go
  type deleteAction struct {
      Mode string // "delete", "revert", or "none"
      Note string // shown under the button, or the reason when Mode is "none"
  }
  func deleteActionFor(o config.Origin, found bool) deleteAction
  ```
  - user only → `{Mode: "delete", Note: "Removes <userfile>. Its container is stopped and removed."}`
  - admin with override → `{Mode: "revert", Note: "Removes <userfile>; <name> goes back to the administrator's definition."}`
  - admin without override → `{Mode: "none", Note: "Defined by the administrator in <admin files>; it cannot be deleted here, only overridden by saving changes."}`
  - not found → `{Mode: "none"}`

Behavior:
- `deleteServer`: look up `u.Config.Origin(name)`; `Mode == "none"` → `409 Conflict` with the note as the body before touching the writer. Writer errors: `errors.Is(err, config.ErrAdminOwned) || errors.Is(err, config.ErrForeignFile)` → 409 rendering the edit form with the error; other errors stay 500.
- `_form_delete.html`: button label `Delete {{.Name}}` or `Revert to admin definition`, `hx-confirm` accordingly; `Mode "none"` renders the note in a muted paragraph with no form.
- `server.html` configuration card: `<dt>Defined in</dt><dd class="mono">{{.Provenance}}</dd>` where `specView.Provenance = strings.TrimPrefix(origin.String(), "Defined in ")`; empty when the origin is unknown.
- `config.html`: a `warn banner` div for `.Warning`; a "Files" section listing `.Files` in order (`<ol class="mono">`), `No files loaded.` when empty; the servers table gains a "Defined in" column.
- `layout.html`: when `.EditNotice` is non-empty, a `warn banner` div right after the nav.
- Import page: unchanged code path; add one test that an import of two servers calls `AddServer` twice and `Reload` once (pins "one reload afterwards").

- [ ] **Step 1: Write the failing tests** (extend `fakeConfig` with `files`, `origins`, `warning`; extend `fakeWriter.err` usage)

```go
func TestServerPageShowsProvenance(t *testing.T)            // origins[alpha] base+user → body contains "overridden by"
func TestConfigPageListsFilesAndWarning(t *testing.T)       // files [a,b], warning "no configuration found (searched: x)" → both in body, in order
func TestEditFormDeleteVariantUser(t *testing.T)            // origin user only → form contains `Delete alpha` and action /servers/alpha/delete
func TestEditFormDeleteVariantRevert(t *testing.T)          // base+user → `Revert to admin definition`
func TestEditFormDeleteVariantNone(t *testing.T)            // base only → no delete form; note contains "cannot be deleted here"
func TestDeleteAdminOwnedIs409(t *testing.T)                // POST /servers/alpha/delete with base-only origin → 409; writer.Deleted empty; no reload
func TestDeleteWriterAdminErrorIs409(t *testing.T)          // origin says user, writer returns ErrAdminOwned → 409
func TestDeleteForeignFileIs409WithPath(t *testing.T)       // writer returns fmt.Errorf("%w: /data/config.d/zzz.yaml", ErrForeignFile) → 409, body contains zzz.yaml
func TestDeleteUserFileSucceeds(t *testing.T)               // → 303 to /, writer.Deleted == [alpha], reloaded
func TestEditNoticeShownWhenEditingDisabled(t *testing.T)   // Deps{Writer: nil, EditNotice: "editing is disabled: /data/config.d is not writable"} → GET / contains the notice; GET /servers/new → 503
func TestImportReloadsOnce(t *testing.T)                    // confirm import of two entries → Added len 2, fakeConfig reload count 1 (add a counter to fakeConfig)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web/ -run 'TestServerPageShowsProvenance|TestConfigPageLists|TestEditFormDelete|TestDelete|TestEditNotice|TestImportReloadsOnce' -v`
Expected: FAIL

- [ ] **Step 3: Implement the view data, handlers, and templates**

- [ ] **Step 4: Run the web package and lint**

Run: `go test ./internal/web/ && make lint`
Expected: PASS, 0 issues

- [ ] **Step 5: Commit**

```bash
git add internal/web
git commit -m "feat(web): show where each server is defined; delete, revert, or refuse per layer; editing notice"
```

---

### Task 10: Packaging and example files

**Files:**
- Modify: `Containerfile:46-51`, `compose.yaml`, `deploy/quadlet/mcp-machine.container`, `Makefile:85-89`, `config.example.yaml`
- Create: `config.example.d/filesystem.yaml`
- Modify: `internal/config/example_test.go`, `deploy/deploy_test.go`

Changes:
- `Containerfile`: `RUN mkdir -p /etc/mcp-machine/config.d` before `VOLUME /data`; comment updated; `CMD ["serve","-c","/etc/mcp-machine"]`.
- `compose.yaml`: volumes add `./config:/etc/mcp-machine:ro` (keep `./data:/data`); header comment explains `./config/config.yaml` + `./config/config.d/` are the admin files and `./data/config.d/` is written by the UI.
- Quadlet: add `Volume=/etc/mcp-machine:/etc/mcp-machine:ro`; comment updated the same way.
- `Makefile`: `CONFIG ?= /etc/mcp-machine`, `CONFIG_D ?= /data/config.d`; `run: ./bin/mcp-machine serve -c $(CONFIG) --config-d $(CONFIG_D)`; `validate: ./bin/mcp-machine validate -c $(CONFIG) --config-d $(CONFIG_D)`.
- `config.example.yaml`: move the `filesystem` server (with its comments) into `config.example.d/filesystem.yaml` under a `servers:` key; add a comment at the top of the base explaining the three locations.
- `example_test.go`: `t.Setenv` `GHCR_USER`, `GHCR_TOKEN`, `GITHUB_TOKEN` to `x`, then `config.Layout{BaseFile: "../../config.example.yaml", SystemDir: "../../config.example.d"}.Load()` and `Validate`; assert 4 servers and `Origins["filesystem"].Files[0].Layer == config.LayerSystem`.

- [ ] **Step 1: Write the failing tests**

```go
// deploy_test.go
func TestContainerfileCreatesAdminRootAndDefaultsToIt(t *testing.T) // contains `mkdir -p /etc/mcp-machine/config.d` and `"serve","-c","/etc/mcp-machine"`
func TestComposeMountsAdminRootReadOnly(t *testing.T)               // a volume ending in `:/etc/mcp-machine:ro`
func TestQuadletMountsAdminRootReadOnly(t *testing.T)               // `Volume=/etc/mcp-machine:/etc/mcp-machine:ro`
func TestMakeRunPassesConfigFlags(t *testing.T)                     // Makefile contains `serve -c $(CONFIG) --config-d $(CONFIG_D)`
// example_test.go
func TestExampleConfigLoadsAndValidates(t *testing.T)               // as described above
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./deploy/ ./internal/config/ -run 'TestContainerfileCreates|TestComposeMountsAdmin|TestQuadletMountsAdmin|TestMakeRun|TestExampleConfig' -v`
Expected: FAIL

- [ ] **Step 3: Make the packaging and example changes**

- [ ] **Step 4: Run the tests, then build the image (controller, outside the sandbox)**

Run: `go test ./deploy/ ./internal/config/` then `make image TAG=dev`
Expected: PASS; image builds; `podman run --rm ghcr.io/kenmoini/mcp-machine:dev validate` prints `mcp-machine validate: no configuration found (searched: /etc/mcp-machine/config.yaml, /etc/mcp-machine/config.d, /data/config.yaml, /data/config.d)` and exits 1.

- [ ] **Step 5: Commit**

```bash
git add Containerfile compose.yaml deploy Makefile config.example.yaml config.example.d internal/config/example_test.go
git commit -m "build: default the image to /etc/mcp-machine with a read-only admin mount; example drop-in"
```

---

### Task 11: Documentation

**Files:**
- Modify: `docs/config.md` (replace "Write-back from the UI", extend "Hot reload"), `README.md` (quick start, compose, quadlet, "Configuration" section, make table), `docs/architecture.md` (config section), `docs/Parked.md`

Content to cover, each in the doc's existing voice:
- `docs/config.md`, new section **Configuration layers** before "Hot reload": the three locations and load order, what each may contain, admin deep-merge rules with one example (base server + system drop-in refining one field), user-layer replace, one server per file in `/data/config.d`, provenance in the UI and `validate -v`, Delete vs Revert vs refused, foreign files, the no-blanking-a-scalar limitation, the deprecated `/data/config.yaml` fallback and the hand migration: move UI-written servers into `/data/config.d/<name>.yaml` and the rest into `/etc/mcp-machine/config.yaml`.
- "Hot reload": the watcher covers all three locations; temp and dot-files are ignored; an invalid drop-in rejects the whole reload and names the file.
- Remove "Write-back from the UI"; replace with **Writes from the UI**: always `/data/config.d/<name>.yaml`, whole spec, placeholders kept, comments not preserved there, editing disabled when the directory is not writable.
- README: quick start mounts `-v /etc/mcp-machine:/etc/mcp-machine:ro -v /srv/mcp-machine:/data`; the minimal config goes to `/etc/mcp-machine/config.yaml`; mention the `/etc/mcp-machine` mount is optional; `make run CONFIG=config.example.yaml CONFIG_D=./data/config.d` for local runs; `validate -c /etc/mcp-machine -v`.
- `docs/architecture.md`: one paragraph on Layout → Set → DirWriter.
- `docs/Parked.md`: remove "Saving an edit drops an explicitly written default key" (no longer applies: user files are app-owned and written whole); rewrite "First run with a missing or empty config exits" to say missing config now starts with defaults and only an empty file still exits; add "Blanking a scalar from a later admin layer" (not possible in v1; a tombstone or explicit-null syntax would be needed) under "Minor hardening and polish".

- [ ] **Step 1: Write the docs**

- [ ] **Step 2: Verify every path, flag, and command in the docs against the code**

Run: `grep -rn "data/config.yaml" README.md docs/*.md` and `./bin/mcp-machine serve -h`
Expected: the only `/data/config.yaml` mentions are in the migration/fallback text; the flags printed match the docs.

- [ ] **Step 3: Commit**

```bash
git add README.md docs
git commit -m "docs: configuration layers, mounts, migration from /data/config.yaml"
```

---

## Self-review notes

- Spec coverage: layout/order (T1, T4), optional base and dirs (T1, T4), fallback (T1, T8), nothing found (T4, T7, T8, T9), empty file (T4), drop-in decoding (T2), dup names (T4), merge rules (T3, T4), defaults once / hash stability (T4), atomic reload (T5), provenance (T4, T8, T9), DirWriter rules incl. foreign files and revert (T6, T9), import one reload (T9), unwritable user dir (T8, T9), watcher (T5), CLI flags and `validate -v` (T8), image/mounts/examples (T10), docs and non-goals (T11).
- Open ruling recorded here, not in the spec: within the user layer, `<name>.yaml` beats any other user file for `name` regardless of sort order (Task 4 canonical rule). Without it the spec's "editing writes `<name>.yaml` (which wins)" would be false for a foreign file sorting after it.
- Open ruling: `serve -c <missing file>` in file mode also starts with defaults and a warning (spec says "anywhere"); `validate` stays strict.
- Open ruling: deleting the base file while running stops its servers like any config removal; the keep-unknown protection is for containers the process never owned.
