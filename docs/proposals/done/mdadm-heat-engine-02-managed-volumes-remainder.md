# Proposal: mdadm Heat Engine — Managed Volume CRUD (Remainder)

**Status:** Pending
**Part of:** mdadm-complete-heat-engine (follow-up to Step 2 of 9)
**Depends on:** mdadm-heat-engine-02-managed-volumes (in done/), mdadm-heat-engine-03-region-inventory

---

## Background

`mdadm-heat-engine-02-managed-volumes.md` was moved to `done/` because the database layer (`tierd/internal/db/managed_volumes.go`), the `managed_volumes` schema (migrations 21–24), and the basic `/api/volumes` CRUD surface (`tierd/internal/api/volumes.go:58–210`) have all landed. The default `data` volume is registered on tier creation in `tierd/internal/tier/tier.go:203`. The original proposal's CRUD spec is satisfied at the API and DB level.

However, several behaviors required by that proposal were never wired up. This follow-up tracks the remainder.

---

## Remaining work

### 1. Region inventory population on volume create

After a `managed_volumes` row is inserted (both for the default `data` volume on tier create and for new volumes via `POST /api/volumes`), the code must call into the inventory package (see proposal `mdadm-heat-engine-03-region-inventory`) to:

- Compute the volume's region count from `size_bytes / region_size_bytes`.
- Insert one `managed_volume_regions` row per region with `current_tier` resolved from the LV's PE → PV mapping.

Currently `db.CreateVolumeRegions` exists (`tierd/internal/db/managed_volumes.go:277-310`) but is not called from any API or provisioning path.

### 2. Volume resize endpoint (`PUT /api/volumes/{id}`)

Add a handler that:

1. Reads the existing managed volume row.
2. Validates the new `size_mb` is a multiple of `region_size_mb` and is a strict increase (no shrink).
3. Calls `lvextend` against the LV using ranked PV ordering for the volume's tier (proposal `mdadm-tiering-infra-12`). The same ranking logic used by `lv.BuildCreateLVArgsForPVs` should apply to the extension PE allocation.
4. Runs the appropriate online filesystem resize for ext4 (`resize2fs`). Only ext4 is in scope for this follow-up.
5. Updates `managed_volumes.size_bytes` via `UpdateManagedVolumeSize`.
6. Calls the inventory package to insert the new region rows for the appended range.
7. Returns the updated volume.

### 3. Ranked PV ordering on `POST /api/volumes`

The volume create handler in `tierd/internal/api/volumes.go` must use the ranked PV ordering helper (`lv.BuildCreateLVArgsForPVs`) so new LVs land on the highest-tier PVs first, matching the spec in mdadm-tiering-infra-12. Today the create path does not use ranked ordering.

### 4. Default `data` volume protection

`DELETE /api/volumes/{id}` must reject deletion of a managed volume whose `lv_name == "data"` and whose `pool_name` matches an active tier instance, returning HTTP 409 with a clear error message. The database layer already exposes `GetManagedVolume`; the check belongs in the handler.

### 5. Startup reconciliation for managed volumes

On `tierd` start, before the monitor goroutines launch, walk every row in `managed_volumes` and:

- Verify the LV exists in LVM (`lvs --noheadings -o lv_name,vg_name`).
- Verify the mount point matches `mount_point` (`/proc/mounts`). Re-mount if missing.
- Log and mark a managed volume as `degraded` (new column or status field — to be added in this proposal) if the LV is missing entirely.

This is the "Volumes are reconciled at startup" acceptance criterion from the original proposal that was never implemented. The reconciliation pass should be wired in from `tierd/internal/monitor/monitor.go` or a new `tierd/internal/reconcile` package, and must run before any heat-engine sampler/evaluator/runner goroutine starts.

### 6. Unit / integration tests

- Test that creating a volume populates `managed_volume_regions` rows.
- Test that resize extends the LV, runs `resize2fs`, and adds the right number of region rows.
- Test that delete refuses the default `data` volume with 409.
- Test that startup reconciliation re-mounts a missing mount and logs degraded state for a missing LV.

---

## Acceptance criteria

- Creating any managed volume produces region rows in `managed_volume_regions` with the correct count and `current_tier` values.
- `PUT /api/volumes/{id}` extends an LV, grows ext4 online, updates `size_bytes`, and adds new region rows; shrink returns 400.
- Volume create on a multi-PV tier places extents on the highest-ranked PVs first.
- `DELETE /api/volumes/{id}` returns 409 when targeting the default `data` volume of a live tier.
- Restarting `tierd` re-mounts any managed volume whose mount went away and logs a degraded state for any volume whose LV no longer exists.
- All new behavior covered by tests in `tierd/internal/api` and (as needed) `tierd/internal/db` or a new `tierd/internal/reconcile` package.

---

## Out of scope

- Filesystems other than ext4.
- Heat-engine migration logic (covered by `mdadm-heat-engine-05-migration-engine`).
- Region inventory implementation itself (covered by `mdadm-heat-engine-03-region-inventory`).
