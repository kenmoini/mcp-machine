# Assessment: layered config (base `config.yaml` + app-owned `config.d/`)

Date: 2026-10-28. Status: assessment only, not yet approved or implemented. Produced by a read-only architecture pass over HEAD `798c945`.

## Summary and recommendation

The single-file assumption is shallow in the Go code (one path string threaded through `cli` → `config.Watcher`/`config.Writer`/`configState.Raw()`) and broad in packaging and docs. Recommended design: keep the existing single-file `Writer` and `Parse` intact and add one layer above them.

- `config.Layout{Base, DropInDir}` resolved from `-c` (file or directory) plus an optional `--config-d` override. Default root in the image: `/etc/mcp-machine` (`config.yaml` + `config.d/`). `/data` stays the state volume.
- `Layout.Load()` reads the base, then `config.d/*.yaml|*.yml` sorted by name; servers merge by name with a later file replacing an earlier one wholesale; provenance is recorded per server; `ApplyDefaults`/`expandEnv` run once on the merged result so spec hashes are unchanged when a server moves between files. Global keys are allowed in the base only. Reload is whole-set atomic: any bad file rejects the reload and the last good set stays (skipping a bad file would make the reconciler delete that file's containers).
- `config.DirWriter` routes UI writes: add → `config.d/<name>.yaml`; edit → rewrite the server's effective source file if it is a drop-in, else copy-on-write into `config.d/<name>.yaml` (the base is never written); delete → remove from its drop-in (removing an emptied file), "revert" when an override shadows a base entry, refused with `ErrBaseOwned` for base-only servers. Validation before write runs on the merged set with the edited file substituted.
- Watcher watches both directories with the same 250 ms debounce; hidden/temp names are ignored; a late-created `config.d` is picked up.
- Disable from the UI = a full override copy with `enabled: false` (hash unchanged, so the container is stopped, not recreated). No tombstones in v1: deleting a base server from a drop-in is refused and left as an extension.
- Backwards compatibility: `-c /data/config.yaml` keeps working (file mode; a sibling `config.d/` is auto-discovered). The image default becomes `serve -c /etc/mcp-machine`; a startup warning and config-page banner cover an old `/data`-only mount meeting the new default.
- Hardened variant: `-v /etc/mcp-machine:/etc/mcp-machine:ro --config-d /data/config.d`. If the drop-in dir is not writable, the UI's editing is disabled cleanly (nil Writer).

## Touchpoint inventory (single-file assumptions)

CLI/state: `internal/cli/validate.go:12,15-24,31`; `internal/cli/serve.go:84,99,102-112,237,248,292,313`; `internal/cli/serve_state.go:23,43,48-54`.
Config: `internal/config/load.go:31-43,46,53-69`; `validate.go:48-55`; `watcher.go:19-24,35,49-55,78,98,111`; `writer.go:24,85-128,89-92,120-126,264-301`; `hash.go:18-31` (no file dependency); `config.go:117-125`.
Manager: `internal/manager/reconcile.go:52-64,78-86`; `runner.go:107-109`.
Web: `internal/web/web.go:45-56,59-63,65-81,227,238,273-286`; `forms.go:231-238,251-261,289-328,346-379,437,449-460,511,521-538,545-549`; `importpage.go:198-224`; `pages.go:86-101,106-121`; templates `_form_delete.html`, `server.html`, `config.html`, `_form_head.html`.
Packaging/docs: `Containerfile:46-47,51`; `compose.yaml:3,13,17-18`; `deploy/quadlet/mcp-machine.container:12-14,25`; `deploy/deploy_test.go:33-47,50-61,102-104`; `README.md:13-28,36,47,56,136,139,143,160-161,198`; `Makefile:85-89`; `config.example.yaml:1-3`; `docs/config.md:3-5,204-230,255-271`; spec `:17,60,96,202,207`; `docs/architecture.md:31,116-126`.
Tests pinning today's behaviour: `internal/config/load_test.go:190-209,285-301`; `watcher_test.go:31-62,143-155`; `writer_test.go` (all; stays valid); `internal/cli/serve_test.go:55-62,219-226,306-353,382-405`; `serve_state_test.go:26-46,134-158`; `internal/web/forms_test.go:22-52,238-256,587-639`; `web_test.go:80-105`; `example_test.go`; `deploy/deploy_test.go`.

## Design decisions in detail

Layout options considered: (1) `/data/config.yaml` + `/data/config.d` (no packaging churn, but the base cannot be kernel-read-only without a single-file mount, which breaks hot reload); (2) `/etc/mcp-machine` tree (FHS-shaped, recommended default); (3) base under `/etc/mcp-machine:ro` and drop-ins under `/data/config.d` (simplest `:ro` story; reachable via `--config-d`). Image: `RUN mkdir -p /etc/mcp-machine/config.d`, no `VOLUME` for it (an anonymous volume would silently hold UI files and vanish on `podman rm`).

Merge: load order base → `config.d/*.yaml|*.yml` sorted bytewise; skip dot-files and subdirectories; globals base-only (`registries`, `runtimes` included; drop-ins decode as `{servers: [...]}` with `KnownFields`); duplicates within a file are an error, across files an override logged at info; empty existing file stays an error; missing base in directory mode is fine.

Validation/`Raw()`: `configState` holds the `Layout`; `Raw()` loads the merged raw set with the placeholder lookup; `check()`, `rawSpec`, `stripDefaults`, `UnsetVars`, `rawRegistries` are unchanged. The loader returns `Set{Config, Origins map[name]Origin{File, Owned, Shadowed}, Files}`.

UI provenance: `ConfigView.Origin(name)`; detail page shows "Defined in config.yaml" / "Defined in config.d/x.yaml" / "overridden by ..."; config page gains a Source column; edit form warns that saving a base-owned server writes an override; delete form has Delete / Revert to base / read-only variants; 409 on `ErrBaseOwned`; 422 (not 500) when `Raw()` fails because a hand-written file is broken.

CLI: `serve -c PATH [--config-d DIR]`, `validate -c PATH [--config-d DIR]` share `ResolveLayout`; `import` unchanged (its output is directly usable as a drop-in).

## Packaging changes

Containerfile: keep `VOLUME /data`; `RUN mkdir -p /etc/mcp-machine/config.d`; `CMD ["serve","-c","/etc/mcp-machine"]`. compose: `./config:/etc/mcp-machine` and `./data:/data`, with the hardened `:ro` + `--config-d /data/config.d` variant in a comment. Quadlet: `Volume=/etc/mcp-machine:/etc/mcp-machine` plus `/srv/mcp-machine:/data`. `deploy_test.go` assertions updated accordingly. README quick start, `config.example.yaml` header (plus an example drop-in and `example_test.go` coverage), `Makefile` `run`/`validate` defaults, `docs/config.md` ("Files and load order", rewritten Hot reload and Write-back), `docs/architecture.md`, and a spec addendum superseding the "Config location" ruling.

## Task breakdown (ordered, test-first)

| # | Task | Size |
|---|---|---|
| 1 | `config.Layout` + `ResolveLayout(cPath, configD)` | S |
| 2 | Split `Parse` into `decode` + `finish`; add `decodeDropIn` with file-prefixed errors | S |
| 3 | `Layout.Load(lookup) (*Set, error)`: ordering, filters, per-file dup error, cross-file override + origins, defaults/env once on the merged config; `config.Load` becomes the file-mode wrapper | M |
| 4 | Hash-stability test: same spec from base vs drop-in → same hash and runtime image | S |
| 5 | Watcher over `Layout`: two watches, late `config.d`, filter rule, file-named errors, whole-set atomic | M |
| 6 | `config.DirWriter`: routing, copy-on-write, revert, `ErrBaseOwned`, file creation/removal, merged-set validation hook; "base bytes never change" test; concurrency test | L |
| 7 | `serve`/`configState`/`validate` wiring: flags, layout in state, `Raw()` via layout, `Origin()`, `MkdirAll` + nil-Writer fallback, "no config found" warning; serve tests for directory mode, drop-in hot reload, invalid drop-in keeps last good, read-only root → 503 forms | M |
| 8 | Web: provenance, delete/revert variants, 409/422 paths | M |
| 9 | Packaging: Containerfile, compose, Quadlet, `deploy_test.go`, Makefile | S |
| 10 | Docs: config.md, README, example files, architecture, spec addendum, Parked updates | M |

Risks: reconcile hash stability (pinned by task 4); watcher storms on imports (hidden temp names + one debounce timer); override shadowing of later admin edits to a base server (provenance text and Revert mitigate); the silent-empty-config case for old mounts (startup warning); virtiofs inotify limitation applies to both directories as today.

## Effect on `docs/Parked.md`

Resolves "first run with a missing or empty config exits". Improves "saving an edit drops an explicitly written default key" (the base is never rewritten). Leaves "UI writes block for the whole reconcile", "concurrent UI writes", and "import preview echoes secrets" unchanged. "Remove action semantics" must route through `DirWriter` and respect `ErrBaseOwned`. New parked candidates: tombstones, global-key overrides in drop-ins, `validate -v` provenance listing, an optional `MCP_MACHINE_CONFIG` env default.
