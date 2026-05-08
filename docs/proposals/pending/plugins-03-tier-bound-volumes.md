# Proposal: SmoothNAS Plugins — Tier-bound Volumes

**Status:** Pending
**Part of:** smoothnas-plugins (Step 3 of 9)
**Depends on:** plugins-02-runtime-integration, mdadm-heat-engine-01-schema

---

## Problem

A plugin manifest can declare a tier-bound volume — "the `models`
volume lives on the NVME slot of tier `media`" — but phase 02 only
supports flat volumes under `/var/lib/smoothnas/plugins/<name>/`,
expressed as Docker `HostConfig.Binds` entries pointing at
`/var/lib/...` paths. This phase resolves tier-bound entries to
real paths on a chosen tier so they can be expressed as the same
kind of bind mount, preflights the placement at install time, and
prevents an operator from removing a tier whose slot a plugin is
using.

---

## Specification

### Install-time tier choice

A tier-bound manifest entry names the **slot** (`NVME` / `SSD` /
`HDD`, or a custom-named tier level from `tier_levels`), not a
specific tier instance. The operator picks the tier instance at
install time, because the same manifest needs to install on hosts
where tiers are named differently.

The CLI gains a `--tier <pool>` flag and the API gains a
`tier_assignments` field on the install request:

```
tierd-cli plugin install llama.yaml llama.raw --tier media
```

```http
POST /api/plugins/install
{
  "manifest_url": "...",
  "artifact_url": "...",
  "tier_assignments": { "media": ["models"] }
}
```

For multi-volume plugins each volume can land on a different tier
(`{"media": ["models"], "fast-scratch": ["cache"]}`); a volume not
named in `tier_assignments` falls back to a single `--tier`
default if provided, otherwise fails preflight.

A flat-mode volume ignores any tier assignment for its name and
warns.

### Path resolution

`tierd/internal/plugin/volume.go` resolves a tier-bound volume to:

```
/mnt/<tier-pool>/.plugins/<plugin-name>/<volume-name>/
```

The `.plugins/` prefix puts plugin data inside the tier mount so
it shares the tier's failure domain, performance characteristics,
and snapshot scope, while the leading dot keeps it out of casual
SMB/NFS browsing of the share root. tierd does **not** create a
separate managed volume for the plugin in this phase — the data
lives in a directory under the tier root, on whatever managed
volume / smoothfs placement the heat engine chooses.

This is deliberately the simple path. A future "give this plugin
its own pinned managed volume on the NVME tier level" mode is a
follow-up; the manifest's `slot` becomes a *placement hint* once
smoothfs grows per-directory pinning, with no manifest changes.

For now, `slot: NVME` on a tier-bound volume is recorded in
`plugin_volumes.slot` for forward compatibility but does not
influence the host path.

### Preflight checks

`Install` runs these gates before attaching the image (additions
to phase 02's flow, inserted after manifest validation, before
`portablectl attach`):

1. **Tier exists.** Each named tier pool resolves to a row in
   `tier_instances`. Unknown pool → `ErrTierNotFound`.
2. **Slot exists.** Each volume's `slot` exists in `tier_levels`
   for that pool, or is one of the seeded defaults
   (`NVME`/`SSD`/`HDD`). Unknown slot → `ErrTierSlotNotFound`.
3. **Tier mounted.** `/mnt/<pool>` is a mountpoint and
   `internal/tier` reports `state = mounted`. Otherwise →
   `ErrTierNotReady`.
4. **Free space.** `statvfs("/mnt/<pool>")` reports
   `free >= max(volume.minSize, 1 GiB)` for *each* tier-bound
   volume on that pool. Aggregated per-pool, summed across
   volumes. Warn-only if `slot` is below the tier's fastest level
   and the operator selected it anyway.
5. **No path conflict.** `/mnt/<pool>/.plugins/<plugin-name>/`
   does not already exist. Stale-leftover directory from a failed
   prior install is treated as conflict; operator removes
   manually or chooses a different name.

Preflight is exposed as a separate verb so the UI (phase 05) can
preview the result before committing:

```http
POST /api/plugins/preflight
{ "manifest": "<yaml>", "tier_assignments": {...} }
→ 200 { "ok": true,  "placements": [...], "warnings": [...] }
→ 400 { "ok": false, "errors":     [...] }
```

### Volume creation

For each tier-bound volume, after preflight passes:

1. `os.MkdirAll("/mnt/<pool>/.plugins/<plugin-name>", 0750)`.
2. `os.Mkdir("/mnt/<pool>/.plugins/<plugin-name>/<volume-name>",
   0750)`.
3. Update `plugin_volumes` row: `tier_pool = <pool>`,
   `host_path = <resolved>`.
4. The container `HostConfig.Binds` entry rendered by phase 02
   now sees the populated `host_path` and points the container
   at the tier-backed directory in the form
   `<host_path>:<bind_path>`.

Volume directory ownership is `root:root` mode `0750`. The
container's default user (image USER for `oci-image`, root for
`lxc-distro`) reads/writes through its own UID. A future
per-plugin UID story may relax this.

Re-rendering on tier reconfiguration: if `internal/tier` changes
the on-disk path of a tier (e.g., the operator renames a tier
instance), tierd must update the affected `plugin_volumes.host_path`
rows and recreate the affected containers (a Docker container's
binds are immutable after create — we stop, remove, recreate from
the manifest with the new paths, and start). This is rare; v1
documents it but does not automate it — the operator runs
`tierd-cli plugin restart <name>` after the rename.

### Tier-removal interaction

`internal/tier`'s tier-removal path gains a single check before it
unmounts a tier or destroys its VG: query
`SELECT plugin_name, volume_name FROM plugin_volumes
WHERE tier_pool = ?`. If any rows match, the removal fails with
a structured error listing the blocking plugins. The operator
must uninstall the plugins first (or, eventually, reassign their
volumes — out of scope for v1).

The tier package gains one new exported function:

```go
// internal/plugin/blockers.go (registered into internal/tier at startup)
func TierRemovalBlockers(pool string) ([]string, error)
```

`internal/tier` calls it via a small interface to avoid importing
the plugin package directly; the wiring lives in the API server
where both packages are already imported.

### Uninstall extension

Phase 02's uninstall flow gains a step between "delete flat
volume directories" and "delete cached `.raw`":

- For each tier-bound volume, `os.RemoveAll(host_path)` and then
  attempt `os.Remove("/mnt/<pool>/.plugins/<plugin-name>")` — the
  parent dir, only if empty. Failure to remove the parent is
  ignored (another concurrent install or stale state); failure to
  remove the volume itself is fatal and returned to the operator.

This honours the parent-level "uninstall is all-or-none" decision
including the persistent volume.

---

## Out of scope

- Per-plugin pinned managed volumes on a specific tier level
  (future, when smoothfs grows per-directory pinning).
- Volume migration between tier instances (operator uninstalls
  and reinstalls).
- Quotas — `minSize` is advisory only.
- Snapshot integration — plugin data is included in any tier-wide
  snapshot the operator takes.

---

## Acceptance

- `go test ./internal/plugin/...` covers: each preflight gate
  failure mode, successful resolution to the documented host
  path, slot-mismatch warning, and no-overwrite of an existing
  `.plugins/<name>` directory.
- `tierd-cli plugin install llama.yaml llama.raw --tier media`
  on a host with a mounted `media` tier creates
  `/mnt/media/.plugins/llama-cpp/models/`, populates
  `plugin_volumes.host_path`, and the plugin's drop-in binds
  that path at `/models` inside the unit.
- `tierd-cli tier delete media` while the llama-cpp plugin is
  installed fails with a message naming the plugin; uninstalling
  the plugin then unblocks the tier deletion.
