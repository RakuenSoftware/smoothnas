# Proposal: mdadm Heat Engine — Migration Engine

**Status:** Done
**Date:** 2026-04-10
**Part of:** mdadm-complete-heat-engine (Step 5 of 9)
**Depends on:** mdadm-heat-engine-03-region-inventory

---

## Problem

The codebase has a low-level `pvmove` wrapper but no mechanism to queue, execute, track, or recover region migrations. Without a migration engine, the policy engine (Step 6) has nowhere to send its placement decisions. The migration engine is the operational core of the heat tiering system.

---

## Specification

### Constraints

- At most one migration runs per host at a time.
- A migration is a single `pvmove` call targeting one contiguous PE range within one managed volume region.
- A region in `migration_state = in_progress` or `queued` is not eligible for re-queuing.
- A pinned volume's regions are never migrated.
- Migrations run entirely online; the managed volume remains mounted and accessible throughout.

### Migration record lifecycle

```
idle -> queued -> in_progress -> verifying -> done
                             -> failed
```

State transitions are written to `managed_volume_regions` before and after each step. Every write is a single DB update with a specific `migration_state` value.

### Migration queue

The migration queue is an in-memory priority queue, not a separate DB table. The policy engine (Step 6) populates it by scanning `managed_volume_regions` and ranking candidate regions.

On restart, the queue is rebuilt from `managed_volume_regions` where `migration_state = queued`. Regions with `migration_state = in_progress` at startup are treated as interrupted and moved to `failed` with `migration_failure_reason = 'interrupted_by_restart'`. The policy engine re-queues them on the next evaluation cycle if still needed.

### Spillover-aware LV allocation

Before the migration queue is consulted, new LV allocations use spillover logic:

1. Identify the highest-ranked tier (lowest `rank`) with free space below `full_threshold_pct`.
2. If the intended tier is at or above `full_threshold_pct`, try the next colder tier.
3. If all tiers are above `full_threshold_pct`, return an allocation error.
4. When an LV extent lands on a tier that is not the intended tier (rank difference > 0), mark the corresponding region as `spilled = true` and set `intended_tier` to the originally desired tier.

Spillover detection runs during region inventory creation (Step 3) by comparing `current_tier` rank to `tier_levels.rank` ordering. The default tier for new data is always rank 1 (fastest available).

### Migration execution

The migration runner is a single goroutine started by the monitor (Step 9).

**Dequeue:** Pop the highest-priority region from the queue.

**Resolve PE range:**

```
lvs -o seg_pe_ranges,devices --noheadings {vg}/{lv}
```

Find the segment containing the region's starting PE. Extract `start_pe` and `end_pe`.

**Resolve destination PV:** Look up `migration_dest_tier` in `tier_levels` to get `array_path` (the PV device).

**Check destination headroom:** Confirm the destination PV has enough free PEs to accept the region. If not, mark the region `failed` with reason `destination_full` and skip.

**Write in_progress:** Update `migration_state = in_progress`, `migration_started_at = now()`, `migration_bytes_total` = region size.

**Execute pvmove:**

```
pvmove {src_pv}:{pe_start}-{pe_end} {dst_pv}
```

Capture stdout line by line. Parse progress lines using the existing `ParsePVMoveLine` function. Update `migration_bytes_moved` periodically (every 10 seconds or every 1% progress, whichever comes first).

**On pvmove exit 0:** Continue to verification step.

**On pvmove non-zero exit or signal:** Update `migration_state = failed`, `migration_failure_reason = pvmove error message`, `migration_ended_at = now()`. Log ERROR.

**Verification:** After pvmove exits 0, call `ResolveCurrentTiers` (Step 3) for the affected volume. Confirm the region's `current_tier` now matches `migration_dest_tier`. If it does not, set `migration_state = failed`, `migration_failure_reason = 'placement_verification_failed'`. Log ERROR.

**On successful verification:** Set `migration_state = done`, `migration_ended_at = now()`, `last_movement_reason = migration_triggered_by`, `last_movement_at = now()`. Clear `spilled` if the region's new `current_tier` matches its `intended_tier`.

### I/O throttling

Before starting a migration, check the host's current disk I/O utilization:

```
iostat -x -o JSON 1 1
```

Compute the average `%util` across all mdadm array devices. If the average exceeds `tier_policy_config.migration_io_high_water_pct`, delay the migration by `poll_interval_minutes` and re-enqueue the region at the back of the queue. Log at INFO level.

Cap pvmove throughput using `ionice -c 3` (idle I/O class) to minimize interference with user traffic.

### Progress tracking API

`GET /api/volumes/{id}` returns the current `migration_state`, `migration_bytes_moved`, `migration_bytes_total`, `migration_dest_tier`, `migration_started_at`, and `migration_failure_reason` for each region. The API layer reads directly from the DB.

---

## Acceptance Criteria

- [ ] At most one migration runs at a time per host.
- [ ] The migration state machine transitions through `idle -> queued -> in_progress -> verifying -> done` (or `failed` on error) with DB writes at each transition.
- [ ] The PE range for a region is resolved from `lvs -o seg_pe_ranges` before each migration.
- [ ] `pvmove` is called with the scoped `src_pv:pe_start-pe_end dest_pv` syntax.
- [ ] `migration_bytes_moved` is updated during migration from pvmove progress output.
- [ ] After pvmove exits 0, `ResolveCurrentTiers` confirms actual placement; a mismatch sets state to `failed`.
- [ ] On restart, `in_progress` regions transition to `failed`; `queued` regions are re-enqueued.
- [ ] Migrations are skipped when host I/O utilization exceeds `migration_io_high_water_pct`.
- [ ] pvmove runs under `ionice -c 3` to limit interference with user I/O.
- [ ] Spillover detection marks regions `spilled = true` with `intended_tier` set when extents land on a non-intended tier.
- [ ] After a successful migration, `spilled` is cleared if `current_tier` now matches `intended_tier`.
- [ ] Pinned volume regions are never enqueued for migration.
