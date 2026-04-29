# Proposal: mdadm Heat Engine — Managed Volume CRUD

**Status:** Pending
**Part of:** mdadm-complete-heat-engine (Step 2 of 9)
**Depends on:** mdadm-heat-engine-01-schema

---

## Problem

Tier instances currently create one LV named `data`, format it, and mount it at `/mnt/{tier}`. That LV is not tracked in the database as a managed object. There is no way to create additional volumes inside a tier, and the tier root filesystem is treated as a special-case rather than as the first entry in a managed-volume model. The heat engine cannot operate on untracked volumes.

---

## Specification

### Default volume on tier creation

When a tier instance is created (`POST /api/tiers`), after the VG is set up, the provisioning path must:

1. Accept an optional `region_size_mb` in the request body (integer, default 256). Validate it is a multiple of 4 (the minimum LVM PE size in MiB). Store it in `tier_instances.region_size_mb`.
2. Create an LV named `data` at `100%FREE` in the tier's VG using ranked PV ordering (per mdadm-tiering-infra-12).
3. Format the LV with ext4.
4. Mount the LV at `/mnt/{tier}`.
5. Insert a row into `managed_volumes`:
   - `pool_name` = tier instance name
   - `vg_name` = tier VG name
   - `lv_name` = `data`
   - `mount_point` = `/mnt/{tier}`
   - `filesystem` = `ext4`
   - `size_bytes` = actual LV size in bytes from `lvs`

This replaces the current ad-hoc LV creation in the tier provisioning path; both the LV create and the DB insert happen in the same operation.

### Mount-point convention

```
/mnt/{tier}                      -> default auto-created volume (lv_name = data)
/mnt/volumes/{tier}/{lv_name}    -> additional operator-created volumes
```

### Managed volume CRUD

Add a `ManagedVolumeStore` to the DB layer with:

```go
func (s *Store) CreateManagedVolume(ctx context.Context, v ManagedVolume) (int64, error)
func (s *Store) GetManagedVolume(ctx context.Context, id int64) (ManagedVolume, error)
func (s *Store) ListManagedVolumes(ctx context.Context, poolName string) ([]ManagedVolume, error)
func (s *Store) UpdateManagedVolumeSize(ctx context.Context, id int64, sizeBytes int64) error
func (s *Store) SetManagedVolumePin(ctx context.Context, id int64, pinned bool) error
func (s *Store) DeleteManagedVolume(ctx context.Context, id int64) error
```

Add an `lvm.ManagedVolumeOps` set of wrappers:

```go
func CreateManagedLV(ctx context.Context, vgName, lvName string, sizeBytes int64, pvDevices []string) error
func ExtendManagedLV(ctx context.Context, vgName, lvName string, additionalBytes int64, pvDevices []string) error
func RemoveManagedLV(ctx context.Context, vgName, lvName string) error
func GetManagedLVSizeBytes(ctx context.Context, vgName, lvName string) (int64, error)
```

All LV operations must use ranked PV ordering (smallest rank first) matching the pool's `tier_levels` table.

### Create volume

`POST /api/volumes` body:

```json
{
  "pool_name": "fast",
  "lv_name":   "archive",
  "size_mb":   51200,
  "filesystem": "ext4"
}
```

Steps:

1. Validate `pool_name` exists and `lv_name` is unique within the VG.
2. Validate there is enough free space across the pool's PVs to satisfy `size_mb`.
3. Call `lvcreate` with ranked PV ordering.
4. Format with specified filesystem.
5. Mount at `/mnt/volumes/{pool_name}/{lv_name}`.
6. Insert into `managed_volumes`.
7. Trigger region inventory creation (Step 3) for the new volume.

Return 201 with the created managed volume record.

### Resize volume

`PUT /api/volumes/{id}` body:

```json
{ "size_mb": 102400 }
```

Steps:

1. Validate new size is larger than current size (shrink is not supported).
2. Call `lvextend` with ranked PV ordering (new extents only go to the highest-ranked PV with free space).
3. Resize the filesystem online (`resize2fs` for ext4).
4. Update `managed_volumes.size_bytes`.
5. Trigger region inventory reconciliation for the volume (appends new region rows).

### Delete volume

`DELETE /api/volumes/{id}`

Steps:

1. Reject if `pinned = true`.
2. Reject if any region has `migration_state` of `in_progress` or `queued`.
3. Unmount the volume.
4. Remove dm-stats registration for the volume (Step 4).
5. Remove the LV.
6. Delete the `managed_volumes` row (cascades to `managed_volume_regions` and `dmstats_regions`).

The default `data` volume (lv_name = `data`, mount_point = `/mnt/{tier}`) may not be deleted while the tier instance exists. Return 409 if attempted.

### Pin and unpin

`PUT /api/volumes/{id}/pin` — set `managed_volumes.pinned = 1`.

`DELETE /api/volumes/{id}/pin` — set `managed_volumes.pinned = 0`.

Pin state is checked by the migration engine before scheduling any region movement.

### Startup reconciliation

On `tierd` startup, for each row in `managed_volumes`:

1. Check that the LV exists in LVM (`lvs`). If missing, log ERROR and mark the volume with a reconciliation error note (future proposal may add an explicit error column).
2. Check the mount point is mounted. If not, attempt remount; if remount fails, log ERROR.
3. Confirm `size_bytes` matches the live LV size; update the DB row if it differs.

---

## Acceptance Criteria

- [ ] `POST /api/tiers` accepts `region_size_mb` and stores it on the tier instance.
- [ ] Tier creation inserts a `managed_volumes` row for the default `data` volume.
- [ ] The default `data` volume is mounted at `/mnt/{tier}` and is tracked identically to any other managed volume.
- [ ] `GET /api/tiers/{name}` returns `region_size_mb` and the count of managed volumes.
- [ ] `POST /api/volumes` creates an LV with ranked PV ordering, formats and mounts it, and inserts the DB row.
- [ ] `PUT /api/volumes/{id}` extends the LV and filesystem online and updates `size_bytes`.
- [ ] `DELETE /api/volumes/{id}` unmounts and removes the LV and all DB rows, and rejects deletion of the default `data` volume.
- [ ] `PUT /api/volumes/{id}/pin` and `DELETE /api/volumes/{id}/pin` set the pin flag.
- [ ] Startup reconciliation verifies each managed volume's LV existence and mount, and corrects `size_bytes` if drifted.
- [ ] All LV operations use ranked PV ordering matching `tier_levels`.
