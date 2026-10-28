# Layered configuration design

Date: 2026-10-28. Owner: Ken Moini. Status: approved decisions recorded, builder-check gaps settled (see "Calls made after the builder check"); implementation planned separately. Supersedes the "Config location" ruling and the "Config write-back" paragraph of `2026-10-27-mcp-machine-design.md`. Builds on `2026-10-28-config-dropins-assessment.md`.

## Intent

Separate configuration the administrator owns from configuration the application writes. Today one file holds both and the UI rewrites it. After this change the admin provisions a base file and system drop-ins that MCP Machine only reads, while everything added or changed through the UI lands in a drop-in directory under the state volume. Hot reload and the reconciler see one merged set.

## Decisions (as Ken decided them)

- **Global keys live in the base file only.** Drop-ins may not set `listen`, `public_url`, `tls`, `podman`, `workspace_dir`, `data_dir`, `allowed_origins`, `registries`, `runtimes`, or `log`. No per-server runtime overrides for now.
- **Servers deep-merge across admin layers; the user layer replaces.** Within the admin layers (base, then system drop-ins) a later definition of the same name refines the earlier one field by field (rules below), so a hand-written override can change one field. A user drop-in (`/data/config.d`) replaces the admin definition of that name wholesale, so what is saved in the UI is exactly what runs and removed fields stay removed.
- **Base-defined servers cannot be deleted from the UI.** Admin-owned servers can be overridden, never removed; the UI offers "Revert to admin definition" when an override exists.
- **UI-written files live in `/data/config.d/`.** Admin files live in `/etc/mcp-machine/config.yaml` and `/etc/mcp-machine/config.d/`.

## Layout and load order

```
/etc/mcp-machine/config.yaml        base: globals + servers   (admin, read-only to the app)
/etc/mcp-machine/config.d/*.yaml    system drop-ins: servers  (admin, read-only to the app)
/data/config.d/*.yaml               user drop-ins: servers    (app-owned; the UI writes here)
```

- Load order: base, then system drop-ins sorted bytewise by file name, then user drop-ins sorted bytewise by file name. `.yaml` and `.yml` are read; names starting with `.` and subdirectories are ignored (this also hides the writer's temp files).
- The base file is optional. Both drop-in directories are optional.
- Upgrade fallback: when the admin root has no base file and no system drop-ins but `/data/config.yaml` exists, that file is used as the base and a deprecation warning names it (existing deployments keep working; servers the UI previously wrote there become admin-owned until moved by hand into `/data/config.d/<name>.yaml`).
- If no configuration file is found anywhere, the server starts with defaults, logs a warning naming the paths searched, shows the same warning on the config page, and the reconciler keeps (does not remove) labeled containers it does not recognise, so a forgotten mount cannot delete running servers.
- An existing but empty (or whitespace-only) file is an error, as today.
- Drop-ins decode as `{servers: [...]}` with unknown keys rejected; a global key in a drop-in is an error naming the file and key.
- Duplicate names within one file are an error naming the file. The same name across files is a merge, logged at info with the contributing files.
- `ApplyDefaults` and `${VAR}` expansion run once on the merged result, so a server's spec hash is identical whether it comes from one file or several.
- Reload is whole-set atomic: any unreadable, unparseable, or invalid file rejects the reload, the error names the file, and the last good set stays in effect.

## Deep-merge rules (admin layers only; per server name, in load order)

Apply within the base file and system drop-ins. A user drop-in definition replaces the accumulated admin spec of that name entirely (no merge). Given an accumulated spec `A` and a later admin definition `B` of the same name:

- `type` must be equal when both are set; a mismatch is an error naming both files. If `A` has no type (impossible for valid files) `B` wins.
- Scalars (strings, ints): `B`'s value wins when it is non-zero; a zero value in `B` leaves `A`'s value (there is no way to blank a scalar from a later layer in v1; document it).
- `enabled` (`*bool`): wins when set in `B`.
- Maps (`env`, `remote.headers`, `command.env`, `container.env`, `build.args`): merged key by key, `B` wins per key.
- Slices (`command.args`, `container.volumes`): replaced wholesale when `B`'s slice is non-nil.
- Sections (`remote`, `command`, `build`, `serve`, `container`, `build.auth`): merged recursively when both present; otherwise the present one is used.
- Merging never mutates the loaded documents; the UI's `Raw()` view shows the merged result with placeholders intact.

## Provenance

The loader returns, per server, the ordered list of contributing files and whether any of them is admin-owned (base or system drop-in). The UI shows "Defined in config.yaml", "Defined in config.d/x.yaml (system)", "Defined in config.yaml, overridden by /data/config.d/x.yaml", and so on. The config page lists every file loaded, in order.

## UI write path

All writes go through a `DirWriter` that owns `/data/config.d/`. The base file and system drop-ins are never written; there is no code path that can.

- **Add**: write `/data/config.d/<name>.yaml` containing the complete spec. Refused if the name already exists in the merged set.
- **Edit**: write the complete effective spec (as shown in the prefilled form) to `/data/config.d/<name>.yaml`, creating or replacing the file. For an admin-defined server this file is an override that replaces the admin definition wholesale. Hand-edited comments in a user drop-in are not preserved (these files are app-owned). Secrets stay as `${VAR}` placeholders because the form is prefilled from the raw (unexpanded) view.
- **One server per file in the user layer**: the UI always reads and writes `/data/config.d/<name>.yaml`. Hand-placed files there that do not follow the convention are loaded, but their servers are "foreign": editing writes `<name>.yaml` (which wins), deleting is refused with a message naming the file, and adding a name whose `<name>.yaml` already exists is refused.
- **Delete**: if the server is defined only by `/data/config.d/<name>.yaml`, remove that file. If it is admin-defined with a user override, the action is "Revert to admin definition" and removes the override only. If it is admin-defined without an override, delete is not offered and a posted request is refused with 409 and an explanation.
- **Import**: one file per imported server, same rules as Add; one reload afterwards.
- Validation before any write runs on the merged set with the new or changed file substituted, plus the existing unset-`${VAR}` check. The write is refused on any error and the form re-renders with the message.
- If `/data/config.d/` cannot be created or written at startup, the UI's add/edit/delete/import are disabled with a visible notice; everything else keeps working.
- Writes stay serialized by one process-wide mutex and use unique temp files plus rename.

## Watcher

Watches the base file's directory, the system drop-in directory, and the user drop-in directory (adding a directory that appears later). Events count only for the base file name and for `*.yaml`/`*.yml` entries directly inside a drop-in directory, excluding dot-prefixed names. One 250 ms debounce covers all three locations, so an import of N servers reloads once.

## CLI and packaging

- `serve -c PATH` where PATH is the base file or the admin root directory (default `/etc/mcp-machine`); `--system-config-d DIR` (default `<root>/config.d`); `--config-d DIR` for the user drop-ins (default `/data/config.d`). `validate` takes the same flags and reports file-prefixed errors; `validate -v` lists the files loaded and the per-server provenance.
- Backwards compatibility: `-c /data/config.yaml` keeps working as a base file; in this file mode there is no system drop-in directory unless `--system-config-d` is given (otherwise `/data/config.d` would be read twice and counted as admin-owned); the user drop-ins still default to `/data/config.d`.
- Image: `RUN mkdir -p /etc/mcp-machine/config.d`, `VOLUME /data` unchanged, `CMD ["serve","-c","/etc/mcp-machine"]`. At startup the user drop-in directory is created if missing.
- Mounts: `-v /etc/mcp-machine:/etc/mcp-machine:ro -v /srv/mcp-machine:/data --security-opt label=disable`; compose and Quadlet carry the same two mounts; `/etc/mcp-machine` is optional.
- `config.example.yaml` becomes the base example; `config.example.d/filesystem.yaml` shows a drop-in; both are loaded by the example test.

## Non-goals (v1 of this change)

Tombstones that delete an admin server from a lower layer; global-key overrides in drop-ins; blanking a scalar from a later layer; preserving comments in app-owned files; an environment variable for the config root.

## Calls left to the builder

Exact Go names for the layout, set, origin, and merge helpers; the shape of the provenance text; the warning wording; how `Writer` is reused inside `DirWriter` (it may be used for user drop-ins or bypassed in favour of plain marshaling since those files are app-owned); whether `validate -v` is JSON or text.

## Calls made after the builder check

1. Upgrade fallback to `/data/config.yaml` and keep-unknown-containers when nothing is found (protects existing deployments from the new image default).
2. Deep merge is limited to admin layers; the user layer replaces wholesale (a UI-written full copy could not otherwise remove fields, and secrets copied into `/data` stay as placeholders).
3. File mode has no default system drop-in directory (avoids reading `/data/config.d` twice).
4. No migration tool for servers the UI wrote into `/data/config.yaml` before this change; documented hand migration.
5. The user layer is strictly one server per `<name>.yaml`; foreign files there are read-only to the UI.
