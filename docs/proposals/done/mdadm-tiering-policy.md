# Proposal: mdadm Tiering — Phase 3: Policy Engine

**Status:** Implemented
**Implements:** mdadm-tiering
**Depends on:** mdadm-tiering-migration

---

## Problem

Phases 1 and 2 put data on the right tier at creation time and handle spillover. But access patterns change over time. Without a policy engine, a cold LV created on NVME stays on NVME forever, and a hot LV on HDD never moves up. This phase adds the heat measurement pipeline and policy engine that make tiering automatic and continuous.

---

## Goals

- Measure per-region I/O heat continuously via dm-stats at configurable region granularity.
- Automatically promote hot regions directly to the appropriate tier based on heat score.
- Automatically demote cold regions one tier at a time.
- Allow operators to pin individual LVs to prevent automatic movement.
- Expose heat scores, pin state, and movement history in the UI.
- Allow global and per-LV policy threshold configuration.

---

## Non-goals

- Manual operator-triggered migration as a UI action.
- dm-cache relationships (separate concern).
- Migration concurrency beyond one per host.

---

## Architecture

### Heat measurement

`tierd` enables dm-stats on each active LV's device-mapper target at startup and when new LVs are created. The LV is divided into fixed-size **regions** (default: 256 MB, configurable). dm-stats tracks read and write IOPS independently per region.

On each poll interval (default: 5 minutes), `tierd` reads cumulative per-region IOPS from dm-stats and maintains a rolling window average per region (default: 24 hours). This rolling average is the region's **heat score**.

The region size must be a multiple of the LVM physical extent size. Smaller regions give finer-grained migration at the cost of more metadata and more frequent migrations. Larger regions reduce migration churn but may carry cold blocks along with hot ones.

### Tier bands

Two configurable IOPS thresholds divide the heat scale into three bands, applied per region:

| Condition | Target tier |
|-----------|-------------|
| Heat ≥ `nvme_threshold` | NVME (hot) |
| Heat ≥ `ssd_threshold` and < `nvme_threshold` | SSD (warm) |
| Heat < `ssd_threshold` | HDD (cold) |

`ssd_threshold` must always be strictly less than `nvme_threshold`. `tierd` rejects configuration that violates this.

With only two active tiers, only the relevant threshold applies (e.g. with SSD + HDD active, only `ssd_threshold` is used; the NVME band is unreachable).

### Policy engine

The policy engine runs on a configurable evaluation interval (default: 30 minutes). On each cycle it:

1. First, promotes any spilled regions whose intended tier has recovered (inherited from Phase 2).
2. For each region of each unpinned LV, determines which tier band its current heat score falls in.
3. If the target tier is faster than the region's `current_tier`, queues a **promotion** directly to the target tier. A region on HDD with hot-level heat migrates straight to NVME — it does not pass through SSD first.
4. If the target tier is slower than the region's `current_tier`, queues a **demotion** of exactly one tier step (NVME→SSD or SSD→HDD). Demotion is gradual to avoid over-reacting to transient cold periods.
5. A region must be in the wrong-tier state for N consecutive evaluation cycles before migration is queued (default: 3 cycles). This prevents thrashing on transient load spikes.

All migrations queued by the policy engine are marked `triggered_by: policy`.

### Pinning

A `pinned` flag on an LV excludes all of its regions from policy evaluation. Pinned LVs are never promoted or demoted automatically. Pin state is togglable per LV in the UI.

---

## Data Model

**LV Region additions (to Phase 2 schema):**
- `heat_score` (float — current rolling average IOPS)
- `heat_sampled_at` (timestamp)
- `consecutive_wrong_tier_cycles` (integer; reset on migration or heat band change)

**LV additions (to Phase 2 schema):**
- `pinned` (boolean, default false)
- Per-LV policy overrides (all nullable; absent means global default applies):
  - `nvme_threshold_override`
  - `ssd_threshold_override`
  - `consecutive_cycles_override`

**Policy config additions (to Phase 2 schema):**
- `nvme_threshold` (IOPS per region; default TBD based on hardware benchmarks)
- `ssd_threshold` (IOPS per region; must be < `nvme_threshold`)
- `consecutive_cycles_before_migration` (default 3)
- `poll_interval_minutes` (default 5)
- `rolling_window_hours` (default 24)
- `evaluation_interval_minutes` (default 30)
- `region_size_mb` (default 256; must be a multiple of the LVM PE size)

---

## API

### Tiers — full policy config

Extends the partial policy config from Phase 2:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/tiers/policy` | GET | Full policy configuration: heat thresholds, region size, evaluation interval, poll interval, rolling window, hysteresis cycles, I/O cap, reserve percentage. |
| `/api/tiers/policy` | PUT | Update any policy configuration fields. `ssd_threshold` < `nvme_threshold` is enforced; violation is rejected. |

### Volumes — heat and pinning

`GET /api/volumes` response gains per-volume:
- `pinned` (boolean)
- `heat_map`: array of per-region objects with `region_index`, `current_tier`, `heat_score`, `migration_state`

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/volumes/{id}/pin` | PUT | Pin LV; exclude all regions from policy evaluation. |
| `/api/volumes/{id}/pin` | DELETE | Remove pin; return LV regions to heat-based evaluation. |

---

## UI Pages

### Volumes page

The volume list gains:
- Heat indicator per volume (aggregate heat across regions — e.g. hottest region's band).
- Pin state with toggle per volume.
- Expandable per-volume heat map: a table or visual grid showing each region's current tier, heat score, and migration state.
- Last movement reason (policy | spillover) and timestamp.

### Tiers page

The Tiers page gains a policy configuration panel:
- Global `nvme_threshold`, `ssd_threshold`, `consecutive_cycles_before_migration`, `region_size_mb`.
- Poll interval and rolling window size.
- Evaluation interval.
- Migration I/O cap and high-water mark (shared with Phase 2).

### Settings page

Policy configuration is also accessible from the Settings page (links to the Tiers page policy panel or duplicates key fields).

---

## Acceptance Criteria

### Heat measurement

- [ ] `tierd` creates dm-stats regions of the configured size on each active LV at startup and LV creation.
- [ ] Per-region IOPS are collected on the configured poll interval.
- [ ] Heat scores are maintained as a rolling window average and persisted in the database.

### Policy engine

- [ ] A region with sustained hot-level heat is promoted directly to NVME, regardless of current tier.
- [ ] A region on HDD with warm-level heat is promoted to SSD.
- [ ] A region on SSD with hot-level heat is promoted to NVME.
- [ ] A cold region on SSD is demoted to HDD; a cold region on NVME is demoted to SSD (one step at a time).
- [ ] A region must be in the wrong-tier state for the configured number of consecutive cycles before migration is queued.
- [ ] `ssd_threshold` < `nvme_threshold` is enforced; configuration violating this is rejected with a clear error.
- [ ] Policy engine evaluation does not queue migrations for regions that are already migrating.

### Pinning

- [ ] All regions of a pinned LV are excluded from policy evaluation.
- [ ] Pinned LVs are never promoted or demoted automatically.
- [ ] Pin state is toggleable per LV in the UI.

### UI

- [ ] Heat score and current tier are visible per region in the volume heat map.
- [ ] Pin toggle is accessible per volume in the Volumes page.
- [ ] Policy-triggered migrations are distinguishable from spillover migrations in the UI.
- [ ] Global policy thresholds and region size are configurable in the Tiers policy panel.
- [ ] Per-LV threshold overrides are configurable from the volume detail view.

---

## Owner and Effort

- **Owner:** SmoothNAS storage backend
- **Effort:** M

---

## Rollout

Depends on Phase 2 (mdadm-tiering-migration) being deployed. The policy engine can be enabled and disabled via a global setting without affecting existing tier assignments or migration state. Heat collection begins immediately on deploy; the first policy evaluation cycle runs after the configured evaluation interval.

---

## Test Plan

- [ ] Unit tests for heat score computation from dm-stats samples.
- [ ] Unit tests for rolling window average calculation (correct window eviction at boundary).
- [ ] Unit tests for tier band assignment given a heat score and thresholds.
- [ ] Unit tests for policy engine: correct promotion target (direct-to-target, not step-by-step), correct demotion step.
- [ ] Unit tests for hysteresis cycle counting (reset on band change, increment on wrong-tier).
- [ ] Unit tests for threshold validation (`ssd_threshold` < `nvme_threshold` enforced).
- [ ] Integration tests for dm-stats region creation on active LVs.
- [ ] Integration tests for automatic promotion: HDD→NVME, HDD→SSD, SSD→NVME.
- [ ] Integration tests for automatic demotion: NVME→SSD, SSD→HDD (one step at a time).
- [ ] Integration tests for hysteresis: volume does not migrate until N consecutive wrong-tier cycles.
- [ ] Integration tests for pinning: pinned LV regions not moved despite heat score.
- [ ] Test that policy engine does not queue migrations for already-migrating regions.
- [ ] Test per-LV overrides take precedence over global defaults.
- [ ] Manual verification of heat map display and pin toggle in UI.
