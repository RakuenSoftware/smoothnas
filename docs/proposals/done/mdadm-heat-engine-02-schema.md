# Proposal: mdadm Heat Engine — 02 Schema

**Status:** Done
**Implements:** mdadm-complete-heat-engine (Work Package 1 — storage-model convergence, DB layer)
**Depends on:** mdadm-tiering-infrastructure

---

## Summary

Adds the database schema and Go data-access layer required by the complete
mdadm heat engine described in `mdadm-complete-heat-engine.md`. This proposal
covers only the persistence layer; provisioning logic, policy evaluation, and
UI are deferred to later work packages.

---

## Changes

### Schema migrations (18–24)

| # | Change |
|---|--------|
| 18 | `ALTER TABLE tier_pools ADD COLUMN region_size_mb INTEGER NOT NULL DEFAULT 256` |
| 19–20 | Bookkeeping no-ops (columns added via `repairTierSchema` to survive schema-drifted upgrades) |
| 21 | Create `managed_volumes` table |
| 22 | Create `managed_volume_regions` table |
| 23 | Create `tier_policy_config` single-row table |
| 24 | Create `dmstats_regions` table |

`repairTierSchema` also gains `ensureColumn` calls for `target_fill_pct` and
`full_threshold_pct` on the `tiers` table so that existing databases upgraded
from pre-18 versions receive the columns regardless of migration-path.

### New columns on existing tables

**`tier_pools.region_size_mb`**  
Fixed region size in MiB for all managed volumes inside the pool. Defaults to
256. Set at pool-creation time and immutable thereafter.

**`tiers.target_fill_pct`**  
Steady-state occupancy target for a tier (default 50%). The policy engine
drains data downward when a tier exceeds this percentage of its total capacity.

**`tiers.full_threshold_pct`**  
Hard-fill threshold above which no new migrations or writes are directed into
a tier (default 90%).

### New tables

**`managed_volumes`**  
One row per LV managed by the heat engine inside a pool's VG. Every pool
auto-creates a default `data` volume at provisioning time.

**`managed_volume_regions`**  
Per-region heat, placement, and migration state for each managed LV. The
region size is derived from the owning pool's `region_size_mb`.

**`tier_policy_config`**  
Single-row global settings for the heat sampler and migration throttle.
Seeded with conservative defaults; operators can override via
`UpsertTierPolicyConfig`.

**`dmstats_regions`**  
Last-seen dm-stats counters per region, used by the heat sampler to compute
deltas across restarts without losing continuity.

### Go API additions

- `db.DefaultRegionSizeMB` constant (256)
- `db.TierInstance.RegionSizeMB` field
- `db.TierSlot.TargetFillPct` and `db.TierSlot.FullThresholdPct` fields
- `db.CreateTierPool` gains a `regionSizeMB int` parameter (0 → default 256)
- `db.ManagedVolume`, `db.ManagedVolumeRegion`, `db.TierPolicyConfig`, `db.DmstatsRegion` types
- `db.RegionMigrationState*` constants
- CRUD methods: `CreateManagedVolume`, `GetManagedVolume`, `ListManagedVolumes`, `UpdateManagedVolumeSize`, `SetManagedVolumePin`, `DeleteManagedVolume`
- Region methods: `CreateVolumeRegions`, `ListVolumeRegions`
- Policy methods: `GetTierPolicyConfig`, `UpsertTierPolicyConfig`

### API surface

`POST /api/tiers` now accepts `region_size_mb` in the request body. All tier
detail responses include `region_size_mb` on the pool and `target_fill_pct` /
`full_threshold_pct` on each tier-slot entry.

---

## Acceptance criteria satisfied

- [x] Database schema migration adds the managed-volume, region, and policy state required by the heat engine.
- [x] Tier creation accepts `region_size_mb`, default `256`, and persists it on the tier pool.
- [x] Each tier slot has a configurable `target_fill_pct` (default 50) and `full_threshold_pct` (default 90).
- [x] Operators can create and manage managed volumes inside a tier pool.
- [x] Region rows can be created and listed for a managed volume.
- [x] Region rows cascade-delete when their parent volume is deleted.
- [x] `CreateVolumeRegions` is idempotent across repeated calls with overlapping indexes.
- [x] `GetTierPolicyConfig` returns defaults when no config row has been written.
- [x] Existing databases upgraded through the drifted-schema repair path receive all new columns.
