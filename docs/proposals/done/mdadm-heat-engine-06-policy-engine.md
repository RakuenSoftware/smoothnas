# Proposal: mdadm Heat Engine — Policy Engine

**Status:** Pending
**Part of:** mdadm-complete-heat-engine (Step 6 of 9)
**Depends on:** mdadm-heat-engine-04-dmstats, mdadm-heat-engine-05-migration-engine

---

## Problem

Without a policy engine, heat scores accumulate in the database but nothing acts on them. Regions never move based on thermal state. The heat engine is inert without an evaluation loop that reads heat scores, applies target-fill logic, and emits migration requests.

---

## Specification

### Evaluation interval

The policy engine runs on `tier_policy_config.evaluation_interval_minutes`. This is separate from the dm-stats sampling interval (`poll_interval_minutes`). Evaluation typically runs less frequently than sampling (e.g. sample every 5 minutes, evaluate every 15 minutes).

### Policy config

All thresholds come from the single row in `tier_policy_config`. No per-volume policy overrides exist in this implementation.

```
poll_interval_minutes               - how often dm-stats are sampled
rolling_window_hours                - heat score EMA window
evaluation_interval_minutes         - how often the policy engine runs
consecutive_cycles_before_migration - wrong-tier cycles before migration is triggered
migration_reserve_pct               - free-space headroom required on destination tier
migration_iops_cap_mb               - pvmove throughput cap
migration_io_high_water_pct         - host I/O utilization above which migrations are deferred
```

### Evaluation algorithm

The policy engine runs once per evaluation interval. The full algorithm for one evaluation cycle:

#### Step 1: Gather state

For each tier instance:

1. Load all `tier_levels` rows in rank order (rank 1 = fastest).
2. For each tier level, compute:
   - `capacity_bytes` — total bytes of PEs on that tier's PV (`pvs -o pv_size`)
   - `used_bytes` — total bytes of all regions currently placed on that tier (sum from `managed_volume_regions` where `current_tier = tier_name`)
   - `free_bytes` = `capacity_bytes - used_bytes`
   - `target_bytes` = `capacity_bytes * target_fill_pct / 100`
   - `full_bytes` = `capacity_bytes * full_threshold_pct / 100`

#### Step 2: Promote spilled regions first

Before normal target-fill evaluation, promote spilled regions that can be returned to their intended tier:

1. Find all regions where `spilled = true` and `intended_tier != current_tier`.
2. For each such region (ordered by heat score descending):
   - Check that the `intended_tier` has `free_bytes > region_size_bytes + reserve`.
   - If yes, enqueue a migration from `current_tier` to `intended_tier` with `migration_triggered_by = 'spillover_recovery'`.
3. Stop enqueuing spillover migrations when the intended tier's free bytes are exhausted.

#### Step 3: Target-fill promotion (fast → faster)

Walk tiers from slowest (highest rank) to fastest (lowest rank):

For each tier that is below its `target_bytes`:
- Compute the deficit: `target_bytes - used_bytes`.
- Collect candidate regions from the next colder tier (the next higher rank) that are unpinned and `migration_state = idle`.
- Sort candidates by `heat_score` descending (hottest first).
- Enqueue enough candidates to fill the deficit, checking that the destination tier has sufficient headroom.
- Stop when deficit is filled or no more candidates exist.

#### Step 4: Target-fill demotion (slow → slower)

Walk tiers from fastest (lowest rank) to slowest (highest rank):

For each tier that is above its `target_bytes`:
- Compute the excess: `used_bytes - target_bytes`.
- Collect candidate regions on that tier that are unpinned and `migration_state = idle`.
- Sort candidates by `heat_score` ascending (coldest first).
- Enqueue enough candidates to drain the excess to the next warmer colder tier (next higher rank).
- Stop when excess is drained or no more candidates exist.

#### Step 5: Hysteresis enforcement

Before enqueuing any candidate region from Steps 3 or 4:

1. Check `consecutive_wrong_tier_cycles` on the region row.
2. If `consecutive_wrong_tier_cycles < tier_policy_config.consecutive_cycles_before_migration`, increment the counter and skip migration for this cycle.
3. If `consecutive_wrong_tier_cycles >= consecutive_cycles_before_migration`, proceed with enqueuing and reset the counter to 0.

A region that is already on the correct tier (based on current target-fill state) has its `consecutive_wrong_tier_cycles` reset to 0.

#### Step 6: Write-burst tolerance

A tier may exceed `target_fill_pct` during write bursts without blocking new writes. The policy engine drains the excess in the background via demotion, but it does not gate writes. Writes only stop preferring a tier when that tier reaches `full_threshold_pct`. This is enforced in the spillover-aware allocation path (Step 5 migration engine).

### Enqueue semantics

The policy engine calls into the migration engine's queue interface, not into pvmove directly. Each enqueue call sets the region's `migration_state = queued` and `migration_dest_tier` and `migration_triggered_by`.

The policy engine does not wait for migrations to complete before the next evaluation cycle. It skips any region already in `queued` or `in_progress` state.

### Pinning

Pinned volumes (`managed_volumes.pinned = 1`) are excluded entirely from Steps 2, 3, and 4. All regions belonging to a pinned volume are skipped without touching their `consecutive_wrong_tier_cycles` counter.

### Evaluation result logging

After each evaluation cycle, log at INFO level:

- Number of spillover promotions enqueued
- Number of target-fill promotions enqueued per tier pair
- Number of target-fill demotions enqueued per tier pair
- Number of regions skipped due to hysteresis
- Number of regions skipped due to pin

---

## Acceptance Criteria

- [ ] The policy engine runs on `evaluation_interval_minutes` independently of the sampler.
- [ ] All thresholds are read from `tier_policy_config` at the start of each evaluation cycle (not cached at startup).
- [ ] Spilled regions are promoted back to their intended tier before normal target-fill logic runs.
- [ ] Promotion fills the fastest tiers first, sourcing from the next colder tier, up to `target_bytes`.
- [ ] Demotion drains the excess above `target_bytes` on each tier downward, removing coldest regions first.
- [ ] A region must have `consecutive_wrong_tier_cycles >= consecutive_cycles_before_migration` before being enqueued for migration.
- [ ] Regions already on their correct target tier have their `consecutive_wrong_tier_cycles` reset to 0.
- [ ] Pinned volume regions are excluded from evaluation and do not have their hysteresis counters modified.
- [ ] Regions already in `queued` or `in_progress` state are not re-enqueued.
- [ ] With 200 GiB total data on tiers sized 150 GiB NVME (target 50%), 100 GiB SSD (target 50%), and 1 TiB HDD (target 50%), steady-state evaluation converges to 75 GiB on NVME, 50 GiB on SSD, and the remainder on HDD.
- [ ] The policy engine logs a summary of enqueued and skipped regions after each evaluation cycle.
