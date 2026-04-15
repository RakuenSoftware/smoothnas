# Proposal: mdadm Heat Engine — API Surface

**Status:** Pending
**Date:** 2026-04-10
**Part of:** mdadm-complete-heat-engine (Step 7 of 9)
**Depends on:** mdadm-heat-engine-05-migration-engine, mdadm-heat-engine-06-policy-engine

---

## Problem

The heat engine has no operator-facing API. Without API endpoints for volumes, policy configuration, and tier-level management, operators cannot inspect heat state, configure policy thresholds, manage volumes, or extend tier definitions. The UI and any external tooling depend on these endpoints existing first.

---

## Specification

### Router changes

Extend the existing API router to add the following route groups:

- `/api/volumes` — managed volume operations
- `/api/tiers/policy` — global heat engine policy config
- `/api/tiers/{name}/levels` — ranked tier level management

All new endpoints return JSON. All error responses follow the existing error envelope format used in the rest of the API.

---

### Volumes API

#### `GET /api/volumes`

Returns all managed volumes across all tier instances. Optional query parameter: `?pool={name}` to filter by tier instance.

Response body:

```json
[
  {
    "id": 1,
    "pool_name": "fast",
    "vg_name": "tier-fast",
    "lv_name": "data",
    "mount_point": "/mnt/fast",
    "filesystem": "ext4",
    "size_bytes": 107374182400,
    "pinned": false,
    "bytes_by_tier": { "NVME": 53687091200, "SSD": 53687091200 },
    "spilled_bytes": 0,
    "active_migration": null,
    "region_count": 400,
    "created_at": "2026-04-10T00:00:00Z",
    "updated_at": "2026-04-10T00:00:00Z"
  }
]
```

`bytes_by_tier` comes from the placement summary cache (Step 3). `active_migration` is non-null if any region is in `in_progress` state and includes `region_index`, `migration_dest_tier`, `migration_bytes_moved`, `migration_bytes_total`.

#### `POST /api/volumes`

Create a new managed volume. See mdadm-heat-engine-02-managed-volumes for the full create specification.

Request body:

```json
{
  "pool_name":  "fast",
  "lv_name":    "archive",
  "size_mb":    51200,
  "filesystem": "ext4"
}
```

Returns 201 with the created volume record on success. Returns 409 if `lv_name` already exists in the VG.

#### `GET /api/volumes/{id}`

Returns one managed volume with full region detail.

Response includes all fields from `GET /api/volumes` plus:

```json
{
  "regions": [
    {
      "region_index": 0,
      "region_offset_bytes": 0,
      "region_size_bytes": 268435456,
      "current_tier": "NVME",
      "intended_tier": null,
      "spilled": false,
      "heat_score": 142.3,
      "heat_sampled_at": "2026-04-10T00:05:00Z",
      "migration_state": "idle",
      "migration_dest_tier": null,
      "migration_bytes_moved": 0,
      "migration_bytes_total": 0,
      "last_movement_reason": null,
      "last_movement_at": null
    }
  ]
}
```

#### `PUT /api/volumes/{id}`

Resize a volume. See mdadm-heat-engine-02-managed-volumes for the resize specification.

Request body:

```json
{ "size_mb": 102400 }
```

Returns 200 with the updated volume record. Returns 422 if the new size is smaller than the current size.

#### `DELETE /api/volumes/{id}`

Delete a volume. Returns 204 on success. Returns 409 if the volume is pinned, has active migrations, or is the default `data` volume.

#### `PUT /api/volumes/{id}/pin`

Pin a volume. Returns 200.

#### `DELETE /api/volumes/{id}/pin`

Unpin a volume. Returns 200.

---

### Policy API

#### `GET /api/tiers/policy`

Returns the single global policy config row.

```json
{
  "poll_interval_minutes": 5,
  "rolling_window_hours": 24,
  "evaluation_interval_minutes": 15,
  "consecutive_cycles_before_migration": 3,
  "migration_reserve_pct": 10,
  "migration_iops_cap_mb": 50,
  "migration_io_high_water_pct": 80,
  "updated_at": "2026-04-10T00:00:00Z"
}
```

#### `PUT /api/tiers/policy`

Update policy config. All fields are optional; omitted fields retain their current values.

```json
{
  "poll_interval_minutes": 10,
  "consecutive_cycles_before_migration": 5
}
```

Validation:

- `poll_interval_minutes`: 1–1440
- `rolling_window_hours`: 1–720
- `evaluation_interval_minutes`: 1–1440
- `consecutive_cycles_before_migration`: 1–100
- `migration_reserve_pct`: 0–50
- `migration_iops_cap_mb`: 1–10000
- `migration_io_high_water_pct`: 10–100

After a successful PUT, the sampler and policy engine pick up the new values on their next wake cycle. No restart is required.

Returns 200 with the full updated config. Returns 422 for validation errors.

---

### Tier levels API

#### `GET /api/tiers/{name}`

Extend the existing tier detail endpoint to include:

```json
{
  "name": "fast",
  "region_size_mb": 256,
  "levels": [
    {
      "id": 1,
      "name": "NVME",
      "rank": 1,
      "array_path": "/dev/md0",
      "target_fill_pct": 50,
      "full_threshold_pct": 90,
      "capacity_bytes": 1073741824000,
      "used_bytes": 536870912000,
      "free_bytes": 536870912000
    }
  ],
  "managed_volume_count": 2,
  "queued_migrations": 3,
  "active_migration": true
}
```

`capacity_bytes`, `used_bytes`, and `free_bytes` per level are computed from `pvs` at request time (not cached in the DB).

#### `POST /api/tiers/{name}/levels`

Add a new ranked tier level to a tier instance. The new level must have a `rank` not already in use for this tier instance and a `name` not already in use.

```json
{
  "name": "TAPE",
  "rank": 4,
  "array_path": "/dev/md3",
  "target_fill_pct": 50,
  "full_threshold_pct": 95
}
```

Returns 201. Returns 409 if the rank or name conflicts.

The PV at `array_path` must already be a member of the tier's VG (assigned via the existing array assignment flow).

#### `PUT /api/tiers/{name}/levels/{level_name}`

Update `target_fill_pct` or `full_threshold_pct` on an existing level. Rank and name are not mutable.

```json
{
  "target_fill_pct": 60,
  "full_threshold_pct": 95
}
```

Returns 200 with the updated level.

#### `DELETE /api/tiers/{name}/levels/{level_name}`

Remove a tier level. Rejected if any regions currently have `current_tier = level_name`. Operators must migrate data off the tier before removing it.

Returns 204. Returns 409 if regions remain on this tier.

---

## Acceptance Criteria

- [ ] `GET /api/volumes` returns all managed volumes with `bytes_by_tier`, `spilled_bytes`, and `active_migration` fields.
- [ ] `POST /api/volumes` creates a volume and returns 201.
- [ ] `GET /api/volumes/{id}` returns per-region detail including heat score and migration state.
- [ ] `PUT /api/volumes/{id}` resizes the volume and returns 200; returns 422 if the new size is smaller.
- [ ] `DELETE /api/volumes/{id}` succeeds and returns 204; returns 409 if pinned, has active migrations, or is the default volume.
- [ ] `PUT /api/volumes/{id}/pin` and `DELETE /api/volumes/{id}/pin` toggle the pin flag.
- [ ] `GET /api/tiers/policy` returns the full policy config.
- [ ] `PUT /api/tiers/policy` updates only the provided fields and validates all ranges; returns 422 for invalid values.
- [ ] `GET /api/tiers/{name}` includes `region_size_mb`, `levels` with per-level capacity stats, and `queued_migrations`.
- [ ] `POST /api/tiers/{name}/levels` adds a new tier level and returns 201; returns 409 on rank or name conflict.
- [ ] `PUT /api/tiers/{name}/levels/{level_name}` updates fill and threshold values.
- [ ] `DELETE /api/tiers/{name}/levels/{level_name}` rejects deletion when regions still reside on that tier.
