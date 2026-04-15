# Proposal: mdadm Heat Engine — Schema Migration

**Status:** Pending
**Date:** 2026-04-10
**Part of:** mdadm-complete-heat-engine (Step 1 of 9)
**Depends on:** mdadm-tiering-infra-04-data-model

---

## Problem

The database has no schema for managed volumes, region inventory, tier-level definitions, heat-engine policy, or dm-stats counter tracking. The current `tiers` table models fixed three-slot NVME/SSD/HDD assignments and lacks the fields needed for configurable target-fill, full-threshold, and per-instance region sizing. Nothing can be built on top of the heat engine until these tables and columns exist.

---

## Specification

### New column: `tier_instances.region_size_mb`

Add `region_size_mb INTEGER NOT NULL DEFAULT 256` to the `tier_instances` table.

- Set at tier instance creation time.
- Applies to all managed volumes in that tier instance.
- Must be a multiple of the LVM PE size (default 4 MiB); validated at create time, not migration time.

### New table: `tier_levels`

Replaces the role of the current fixed-slot `tiers` table for heat-engine tier ordering. A tier level belongs to one tier instance and defines one performance rank within that instance.

```sql
CREATE TABLE tier_levels (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pool_name   TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    rank        INTEGER NOT NULL,
    array_path  TEXT,
    target_fill_pct   INTEGER NOT NULL DEFAULT 50,
    full_threshold_pct INTEGER NOT NULL DEFAULT 90,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (pool_name, name),
    UNIQUE (pool_name, rank)
);
```

`rank` is an integer where lower values are faster (rank 1 = fastest). The migration seeds three rows per existing tier instance:

| name | rank |
|------|------|
| NVME | 1    |
| SSD  | 2    |
| HDD  | 3    |

Operators may insert additional rows with higher rank values.

### New table: `managed_volumes`

```sql
CREATE TABLE managed_volumes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pool_name   TEXT    NOT NULL,
    vg_name     TEXT    NOT NULL,
    lv_name     TEXT    NOT NULL,
    mount_point TEXT    NOT NULL,
    filesystem  TEXT    NOT NULL DEFAULT 'ext4',
    size_bytes  INTEGER NOT NULL,
    pinned      INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (vg_name, lv_name),
    UNIQUE (mount_point)
);
```

### New table: `managed_volume_regions`

```sql
CREATE TABLE managed_volume_regions (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    volume_id                   INTEGER NOT NULL REFERENCES managed_volumes(id) ON DELETE CASCADE,
    region_index                INTEGER NOT NULL,
    region_offset_bytes         INTEGER NOT NULL,
    region_size_bytes           INTEGER NOT NULL,
    current_tier                TEXT    NOT NULL,
    intended_tier               TEXT,
    spilled                     INTEGER NOT NULL DEFAULT 0,
    heat_score                  REAL    NOT NULL DEFAULT 0,
    heat_sampled_at             TEXT,
    consecutive_wrong_tier_cycles INTEGER NOT NULL DEFAULT 0,
    migration_state             TEXT    NOT NULL DEFAULT 'idle',
    migration_triggered_by      TEXT,
    migration_dest_tier         TEXT,
    migration_bytes_moved       INTEGER NOT NULL DEFAULT 0,
    migration_bytes_total       INTEGER NOT NULL DEFAULT 0,
    migration_started_at        TEXT,
    migration_ended_at          TEXT,
    migration_failure_reason    TEXT,
    last_movement_reason        TEXT,
    last_movement_at            TEXT,
    created_at                  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at                  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (volume_id, region_index)
);
```

`migration_state` is an enum: `idle`, `queued`, `in_progress`, `verifying`, `done`, `failed`.

### New table: `tier_policy_config`

Single-row table for global heat engine settings. The migration inserts the one default row.

```sql
CREATE TABLE tier_policy_config (
    id                              INTEGER PRIMARY KEY CHECK (id = 1),
    poll_interval_minutes           INTEGER NOT NULL DEFAULT 5,
    rolling_window_hours            INTEGER NOT NULL DEFAULT 24,
    evaluation_interval_minutes     INTEGER NOT NULL DEFAULT 15,
    consecutive_cycles_before_migration INTEGER NOT NULL DEFAULT 3,
    migration_reserve_pct           INTEGER NOT NULL DEFAULT 10,
    migration_iops_cap_mb           INTEGER NOT NULL DEFAULT 50,
    migration_io_high_water_pct     INTEGER NOT NULL DEFAULT 80,
    updated_at                      TEXT    NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO tier_policy_config (id) VALUES (1);
```

### New table: `dmstats_regions`

Reconciliation table for dm-stats counter continuity across restarts.

```sql
CREATE TABLE dmstats_regions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    volume_id           INTEGER NOT NULL REFERENCES managed_volumes(id) ON DELETE CASCADE,
    dmstats_region_id   INTEGER NOT NULL,
    region_index        INTEGER NOT NULL,
    last_read_ios       INTEGER NOT NULL DEFAULT 0,
    last_write_ios      INTEGER NOT NULL DEFAULT 0,
    last_sampled_at     TEXT,
    UNIQUE (volume_id, dmstats_region_id)
);
```

### Migration version

This migration is a new schema version. The migration runner must:

1. Add `region_size_mb` to `tier_instances`.
2. Create `tier_levels` and seed one `NVME/SSD/HDD` row per existing tier instance.
3. Create `managed_volumes`.
4. Create `managed_volume_regions`.
5. Create `tier_policy_config` and insert the default row.
6. Create `dmstats_regions`.

The migration does not drop or alter the existing `tiers` table. That table remains in place until the managed-volume provisioning proposal (Step 2) establishes the new provisioning path and the old tier-root model is retired.

---

## Acceptance Criteria

- [ ] `tier_instances` has a `region_size_mb` column with default 256.
- [ ] `tier_levels` table exists with all specified columns, constraints, and the seeded NVME/SSD/HDD rows per existing tier instance.
- [ ] `managed_volumes` table exists with all specified columns and constraints.
- [ ] `managed_volume_regions` table exists with all specified columns, constraints, and the full `migration_state` enum set.
- [ ] `tier_policy_config` table exists with all specified columns and a single default row (id = 1).
- [ ] `dmstats_regions` table exists with all specified columns and constraints.
- [ ] The migration is idempotent — re-running against an already-migrated database does not error.
- [ ] The existing `tiers` table is not dropped or altered by this migration.
- [ ] The migration version constant is bumped in the migration runner.
