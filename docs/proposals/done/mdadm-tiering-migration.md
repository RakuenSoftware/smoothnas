# Proposal: mdadm Tiering — Phase 2: Migration and Spillover

**Status:** Implemented
**Implements:** mdadm-tiering
**Depends on:** mdadm-tiering-infrastructure

---

## Problem

Once tiers exist and LVs can be placed on them, two gaps remain before the system is useful in practice: data must be able to move between tiers online without unmounting, and the system must handle the case where a tier is full. Without a migration workflow and spillover logic, a full tier blocks LV creation entirely, and moving data to a different tier requires manual `pvmove` invocations outside the UI.

This phase validates the pvmove migration machinery and adds spillover, making it safe for a tier to fill up and enabling the policy engine in Phase 3 to drive migrations automatically.

---

## Goals

- Implement online tier migration via `pvmove` targeting specific physical extent ranges, with full state tracking.
- Automatically place new LV extents on the next colder tier when the intended tier is full (spillover), and promote them back when capacity recovers.
- Recover cleanly from a crash during migration.
- Expose migration progress and spillover state in the UI.

---

## Non-goals

- Heat measurement or automatic policy-driven movement (Phase 3).
- Manual operator-triggered migration as a UI action. Migration in this phase is triggered by spillover and, later, the Phase 3 policy engine. There is no "move this volume" button.
- Pinning (Phase 3).

---

## Architecture

### Extent placement with spillover

When a new LV is created or extended, `tierd` first checks whether the target tier is at or above its `full_threshold` (default 90%). If so, allocation falls to the next colder active tier — cascade order NVME → SSD → HDD. If all tiers are full, creation fails with a clear capacity error.

An LV that lands on a colder tier than intended records both:
- `intended_tier`: the tier it should have gone to
- `actual_tier`: the tier it actually landed on
- `spilled: true`

When `tierd` monitors tier free space and the intended tier drops below `full_threshold`, the spilled region is queued for promotion back to its intended tier.

### Migration mechanism

All tier migrations use `pvmove` targeting specific physical extent (PE) ranges:

```
pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>
```

`tierd` maps each LV region to the LVM PEs that back it (via `lvs -o seg_pe_ranges`), then issues the scoped `pvmove` command to move only those extents online, without unmounting the filesystem or interrupting I/O.

The region-to-PE mapping is derived from `lvs -o seg_pe_ranges` and updated whenever an LV is extended or resized. This mapping is the sole migration path — there is no manual extent copy or dm suspend/resume approach.

### Migration state machine

`tierd` tracks each region migration as a state machine:

```
idle → queued → migrating → verifying → complete
                    ↓                       ↑
               cancelling ────────────────→ (aborted)
                    ↓
                 failed
```

Steps:

1. Validate destination tier has free capacity above reserve (default: 10% of destination tier capacity).
2. Mark the region `migrating` in the database.
3. Resolve the region's PEs from `lvs -o seg_pe_ranges`.
4. Run `pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>` with an I/O rate cap applied.
5. Monitor `pvmove` progress; update `bytes_moved` / `bytes_total` in the database.
6. On completion, verify the region's PEs are now on the destination tier's PV.
7. Update the region's `current_tier`; mark `complete` or `failed`.

Only one migration runs at a time per host. Queued migrations wait until the current one finishes.

### Throttling

`pvmove` is run with a configurable MB/s cap. `tierd` monitors overall host I/O utilisation and pauses or slows migration when the host exceeds a configurable high-water mark.

### Crash recovery

On startup, `tierd` reconciles DB migration state against actual PE placement via `lvs -o seg_pe_ranges` and `pvs`. If a region is recorded as `migrating` but its PE placement is inconsistent with both source and destination tiers, `tierd` surfaces the discrepancy in the UI as a recoverable error. The operator can re-queue the migration or manually mark it complete if extents are already on the destination.

---

## Data Model

**LV Region** (one row per region per LV — regions are introduced in this phase; heat fields are added in Phase 3):
- `lv_id` (foreign key)
- `region_index` (integer — 0-based offset within LV)
- `region_offset_bytes` (integer)
- `region_size_bytes` (integer — configurable, default 256 MB; must be a multiple of LVM PE size)
- `current_tier` (nullable — NVME | SSD | HDD)
- `migration_state` (idle | queued | migrating | cancelling | verifying | complete | failed)
- `migration_triggered_by` (nullable — policy | spillover)
- `migration_dest_tier` (nullable — NVME | SSD | HDD)
- `migration_bytes_moved` (integer)
- `migration_bytes_total` (integer)
- `migration_started_at` (timestamp)
- `migration_ended_at` (nullable timestamp)
- `migration_failure_reason` (nullable string)
- `last_movement_reason` (nullable — policy | spillover)
- `last_movement_at` (nullable timestamp)

**LV additions (to Phase 1 schema):**
- `intended_tier` (nullable — NVME | SSD | HDD; set when spilled)
- `spilled` (boolean, default false)

**Policy config additions:**
- `migration_reserve_pct` (default 10 — free capacity to maintain on each tier before blocking migration in)
- `migration_iops_cap` (MB/s)
- `migration_io_high_water_pct` (host I/O utilisation percentage at which to back off)

---

## API

### Tiers — policy config (partial)

Extends the policy config introduced in Phase 3 prep; only migration-specific fields are active in this phase:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/tiers/policy` | GET | Current policy configuration (migration reserve %, I/O cap, high-water mark). Heat threshold fields are returned as null until Phase 3. |
| `/api/tiers/policy` | PUT | Update migration policy configuration. |

### Volumes — spillover and migration state

`GET /api/volumes` response gains per-volume:
- `intended_tier` (null unless spilled)
- `spilled` (boolean)
- Per-region migration state and progress (array of region objects with `migration_state`, `bytes_moved`, `bytes_total`)

`GET /api/tiers` response gains per tier:
- `migration_queue_depth` (number of regions queued or migrating)

---

## UI Pages

### Tiers page

The Tiers page gains:
- A highlight on tiers at or near `full_threshold`.
- Migration queue depth per tier.

### Volumes page

The volume list gains:
- `intended_tier` alongside `actual_tier` when they differ (visually distinct for spilled volumes).
- Per-volume migration progress (bytes moved / bytes total) while a migration is active.

---

## Acceptance Criteria

### Spillover

- [ ] New extents target the fastest active tier; if that tier is at or above `full_threshold`, allocation falls to the next colder active tier.
- [ ] Spillover cascades: NVME full → SSD; SSD also full → HDD.
- [ ] If all tiers are full, LV creation or extension fails with a clear capacity error.
- [ ] A spilled LV records `intended_tier` and `actual_tier` and is marked `spilled: true`.
- [ ] A spilled LV is queued for promotion when its intended tier drops below `full_threshold`.

### Migration

- [ ] Region-to-PE mapping is derived from `lvs -o seg_pe_ranges` and kept current on LV create/extend/resize.
- [ ] Migration runs online via `pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>` with no downtime.
- [ ] Only one migration runs at a time per host.
- [ ] Migration respects the configured I/O cap.
- [ ] Migration backs off when host I/O utilisation exceeds the high-water mark.
- [ ] Migration progress (bytes moved / total) is tracked and visible in the UI.
- [ ] On restart after a crash mid-migration, `tierd` detects PE placement inconsistency and surfaces it in the UI.
- [ ] After a failed migration, the region's PEs are in a consistent state on either source or destination tier.

### UI

- [ ] Spilled volumes are visually distinct in the volume list with `actual_tier` and `intended_tier` shown.
- [ ] Tiers at or near `full_threshold` are highlighted on the Tiers page.
- [ ] Migration queue depth is visible per tier on the Tiers page.
- [ ] Active migration progress is visible per volume on the Volumes page.

---

## Owner and Effort

- **Owner:** SmoothNAS storage backend
- **Effort:** M

---

## Rollout

Depends on Phase 1 (mdadm-tiering-infrastructure) being deployed. Spillover activates automatically once migration machinery is in place. If no tier is full, spillover never fires. No feature flag required.

---

## Test Plan

- [ ] Unit tests for spillover logic: target tier full → next colder tier selected.
- [ ] Unit tests for cascade spillover: NVME and SSD both full → HDD.
- [ ] Unit test for all-tiers-full: creation fails with a clear error.
- [ ] Unit tests for region-to-PE mapping derivation from `lvs` output.
- [ ] Unit tests for `pvmove` argument construction (correct PE range format).
- [ ] Integration tests for spillover: LV placed on next tier, promoted when intended tier recovers.
- [ ] Integration tests for `pvmove` migration completing successfully.
- [ ] Integration tests for migration throttling: I/O cap is respected.
- [ ] Failure injection: crash `tierd` mid-`pvmove`, verify restart detects and surfaces the inconsistency.
- [ ] Failure injection: destination tier fills during migration, verify `failed` state and clean recovery.
- [ ] Failure injection: degraded source or destination array during migration.
- [ ] Test that `actual_tier` and `intended_tier` are updated correctly on promotion completion.
- [ ] Test that `spilled` flag is cleared after successful promotion.
