# Proposal: mdadm Complete Heat Engine

**Status:** Pending
**Depends on:** mdadm-tiering-infrastructure
**Clarifies:** mdadm-tiering-migration, mdadm-tiering-policy

---

## Problem

The repository contains the low-level LVM primitives needed for a heat-driven mdadm tiering engine, but it does not contain the complete engine described in the historical design documents.

Today the named-tier implementation provisions a per-tier volume group, creates one logical volume named `data`, formats it, and mounts it directly at `/mnt/{tier}`. That model is useful as a simple mountable storage target, but it is not the same model assumed by the migration and policy documents:

- there is no managed volume model
- there is no region inventory
- there is no dm-stats sampling pipeline
- there is no migration queue or recovery state
- there is no policy engine
- there is no UI or API surface for volume heat, pinning, or migration status

This proposal defines a concrete implementation plan for the complete mdadm heat engine and resolves the largest architectural mismatch first: the system must tier managed logical volumes, not the tier root mount itself.

---

## Goals

1. Deliver the complete mdadm heat engine described by the historical tiering documents.
2. Preserve the current named-tier instance model as the backing pool abstraction.
3. Introduce first-class managed volumes inside each tier instance's VG.
4. Measure per-region heat with dm-stats and persist rolling heat scores.
5. Migrate hot and cold regions online with `pvmove` between ranked tiers inside a tier instance.
6. Provide default ranked tiers `NVME`, `SSD`, and `HDD`, while allowing operators to define additional tiers beyond those defaults.
7. Support per-tier target-fill balancing, with a default target of 50% of each tier's capacity.
8. Support spillover, policy-driven promotion and demotion, pinning, and crash recovery.
9. Provide a usable API and UI for operators to see placement, heat, policy, and migration progress.

---

## Non-goals

- ZFS tiering or any shared engine with ZFS
- dm-cache relationships
- cross-host or network tiering
- more than one concurrent migration per host in the initial implementation
- file-level heat tracking
- replacing LVM with a different migration mechanism
- preserving existing tier root storage layouts across upgrade

---

## Current Gaps

The current tree is missing the following pieces required for a complete heat engine:

- **Managed volumes:** there is no `volumes` concept in the API or DB; tiers only create a mounted root LV.
- **Region state:** there are no DB tables for region offsets, current tier, spillover, heat, or migration state.
- **Heat sampling:** there is no dm-stats lifecycle management and no rolling heat computation.
- **Migration orchestration:** there is a `pvmove` wrapper, but no queue, state machine, throttling, verification, or recovery loop.
- **Policy evaluation:** there are no policy config records, target-fill settings, hysteresis counters, or pin handling.
- **Tier model:** the current storage model is fixed to `NVME`, `SSD`, and `HDD` slots rather than ranked user-extensible tiers.
- **Operator surfaces:** there is no `/api/volumes`, no `/api/tiers/policy`, and no frontend route for volume heat or pinning.
- **Monitor integration:** the background monitor does not sample tier heat or run migration policy.

---

## Decision

The complete heat engine will be implemented on top of **managed logical volumes inside a tier instance's volume group**.

### Upgrade assumption

This design only preserves database continuity, not old tier storage layouts.

- database schema migration is in scope
- old tier instances backed by auto-created `data` LVs are not a supported runtime model after rollout
- operators are expected to delete old tiers and recreate them under the managed-volume model
- there is no storage-object import path and no in-place tier conversion flow

### Canonical object model

```
mdadm arrays
  -> assigned to ranked tiers inside a tier instance
  -> become PVs in one tier-specific VG
  -> managed logical volumes live in that VG
  -> each managed logical volume is divided into regions
  -> dm-stats measures heat per region
  -> pvmove migrates regions between PVs in the VG
```

Tier instances automatically create a default managed volume named `data`, sized to `100%FREE`, and mount it at `/mnt/{tier}`. The heat engine treats that default filesystem as a managed volume from the start.

Each tier instance is seeded with three ranked tiers by default:

- `NVME`
- `SSD`
- `HDD`

Operators may add additional ranked tiers beyond those defaults. Every tier has:

- a name
- an order rank
- one backing mdadm array in the initial implementation
- a configurable `target_fill_pct`, default `50`

Every tier instance has:

- a `region_size_mb` chosen when the tier instance is created
- a default of `256`
- one region size applied consistently to all managed volumes in that tier instance

---

## Architecture

### 1. Tier instances remain the backing pools

The current tier instance abstraction stays in place:

- tier instances still own the ranked tier definitions and array assignments
- tier instances still own the LVM volume group
- tier rank defines the performance order, with seeded defaults `NVME > SSD > HDD`

What changes is the role of the tier root:

- tier instances still own the backing pool
- the tier root mount is the default managed volume, not a special unmanaged filesystem
- managed volumes remain the unit of heat tracking, placement, and migration

### 2. Managed volumes become first-class

Each tier instance VG can contain many managed LVs.

Each managed volume records:

- volume name
- tier instance name
- VG/LV identity
- mount point
- filesystem type
- size
- pinned flag
- actual placement summary
- intended placement summary

Mount-point convention:

```
/mnt/{tier}                  -> default auto-created `data` volume
/mnt/volumes/{tier}/{volume} -> additional managed volumes
```

Tier instance names remain the stable backing identifiers. The default user-visible filesystem lives at the tier root, but it is now modeled identically to any other managed volume.

### 3. Regions are the unit of heat and migration

Each managed volume is divided into fixed-size regions.

- the region size is defined per tier instance at creation time
- default size: 256 MiB
- must be a multiple of the VG PE size
- region rows are created on:
  - volume create
  - volume extend
  - startup reconciliation if rows are missing

Each region stores:

- region index
- byte offset
- byte size
- current tier name
- intended tier name when spilled
- migration state
- bytes moved / total
- latest heat score
- last sampled timestamp
- consecutive wrong-tier cycles

### 4. dm-stats is the heat source

For each active managed LV:

1. ensure dm-stats is enabled on the LV's device-mapper target
2. create or reconcile dm-stats regions to match the owning tier instance's configured region size
3. sample cumulative read and write counters on the configured poll interval
4. compute per-region deltas
5. persist rolling heat scores over the configured window

Heat is measured as IOPS per region, with read and write counters summed into one policy score.

### 5. `pvmove` is the migration mechanism

All region movement uses scoped `pvmove`:

```
pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>
```

The migration manager:

- resolves region PE ranges from `lvs -o seg_pe_ranges`
- maps source and destination tier PVs
- runs one migration at a time per host
- persists progress
- verifies final placement after completion
- retries or fails cleanly on restart

### 6. Policy engine drives automatic movement

The policy engine runs on a slower evaluation interval than the sampler.

For each unpinned region:

1. compute each tier's target bytes as `tier_capacity * target_fill_pct`
2. if a tier is below target bytes, promote data up from the next colder tier until the tier reaches target or there is no colder data left
3. if a tier is above target bytes, demote only the excess bytes down to the next colder tier
4. repeat this in rank order from fastest to slowest tiers
5. use heat scores to choose *which* regions move first when more candidate data exists than the byte delta requires
6. use hysteresis via `consecutive_wrong_tier_cycles` so transient spikes do not cause thrash

Spillover handling runs before normal policy movement:

- new extents target the highest-ranked tier with free space below its hard full threshold
- when the intended tier is full, allocation spills to the next colder tier
- spilled regions record both `intended_tier` and `current_tier`
- when free space recovers, spilled regions are promoted first

### 7. Write-path behavior

Incoming writes remain biased toward the highest-ranked writable tier.

That means:

- new writes continue landing on the fast tier even after it exceeds `target_fill_pct`
- the policy engine drains only the excess bytes downward in the background
- the fast tier is allowed to float above target temporarily during active write bursts
- writes only stop preferring that tier when it reaches `full_threshold_pct`

This makes `target_fill_pct` a steady-state control rather than a synchronous write gate.

### 8. Target-fill balancing

`target_fill_pct` is the steady-state occupancy target for a tier.

Default:

- every seeded tier starts at `50%`

Meaning:

- if a tier has 150 GiB capacity and `target_fill_pct = 50`, the engine targets 75 GiB on that tier
- if that tier is below 75 GiB, data is promoted up into it
- if that tier is above 75 GiB, only the excess is demoted down
- heat determines which regions are chosen when more data is available than the target delta requires

Example:

- total data: 200 GiB
- NVME tier: 150 GiB capacity, target 50% -> target bytes 75 GiB
- SSD tier: 100 GiB capacity, target 50% -> target bytes 50 GiB
- HDD tier: 1 TiB capacity, target 50% -> target bytes 512 GiB, but only the remainder is needed

The desired steady-state placement is:

- hottest 75 GiB on NVME
- next 50 GiB on SSD
- remaining 75 GiB on HDD

Another example:

- total data: 500 GiB
- NVME tier: 1 TiB capacity, target 50% -> target bytes 500 GiB

Result:

- the first 500 GiB goes to NVME and stays there while total data remains at or below 500 GiB
- data only starts moving down once NVME exceeds its 500 GiB target

Write-burst example:

- volume logical size: 1 TiB
- incoming write workload: 2 TiB over time
- NVME tier: 1 TiB capacity, target 50% -> target bytes 500 GiB

Behavior:

- writes keep landing on NVME first
- once NVME grows beyond 500 GiB, the policy engine begins moving the excess down to colder tiers
- those downward moves happen while new writes are still accepted on NVME
- NVME is not treated as "closed for writes" at 500 GiB; it is only above target and eligible for background drain
- writes stop preferring NVME only when NVME reaches its hard `full_threshold_pct`

This target-fill logic is separate from spillover:

- `target_fill_pct` is the steady-state up/down trigger used during normal placement
- `full_threshold_pct` is the hard threshold used to prevent new allocations or migrations into an overfull tier

---

## Data Model

This proposal adds the following tables.

### `managed_volumes`

- `id`
- `tier_name`
- `vg_name`
- `lv_name`
- `mount_point`
- `filesystem`
- `size_bytes`
- `pinned` (boolean)
- `created_at`
- `updated_at`

### `managed_volume_regions`

- `id`
- `volume_id`
- `region_index`
- `region_offset_bytes`
- `region_size_bytes`
- `current_tier`
- `intended_tier` (nullable)
- `spilled` (boolean)
- `heat_score`
- `heat_sampled_at`
- `consecutive_wrong_tier_cycles`
- `migration_state`
- `migration_triggered_by`
- `migration_dest_tier`
- `migration_bytes_moved`
- `migration_bytes_total`
- `migration_started_at`
- `migration_ended_at`
- `migration_failure_reason`
- `last_movement_reason`
- `last_movement_at`

### `tier_levels`

- `id`
- `tier_instance_name`
- `name`
- `rank`
- `array_path`
- `target_fill_pct`
- `full_threshold_pct`
- `created_at`
- `updated_at`

The database migration seeds three rows per tier instance by default:

- `NVME`
- `SSD`
- `HDD`

but operators may create more ranked tiers later.

### `tier_instances` additions

- `region_size_mb`

`region_size_mb` is set when the tier instance is created and applies to all managed volumes in that tier instance.

### `tier_policy_config`

Single-row table or config-backed view with:

- `poll_interval_minutes`
- `rolling_window_hours`
- `evaluation_interval_minutes`
- `consecutive_cycles_before_migration`
- `migration_reserve_pct`
- `migration_iops_cap_mb`
- `migration_io_high_water_pct`

### `dmstats_regions`

Optional reconciliation table recording:

- `volume_id`
- `dmstats_region_id`
- `region_index`
- `last_reads`
- `last_writes`
- `last_read_ios`
- `last_write_ios`
- `last_sampled_at`

This table is not the source of truth for placement, but it makes restart reconciliation deterministic.

---

## API

### Tiers

Existing endpoints remain, but their meaning changes slightly:

- `POST /api/tiers` creates the backing VG and default ranked tiers
- `POST /api/tiers` accepts `region_size_mb`
- it auto-creates the default managed volume at `100%FREE`

New endpoint:

- `GET /api/tiers/{name}` returns:
  - tier definitions and ranks
  - capacity by tier
  - target-fill percentages
  - `region_size_mb`
  - queue depth
  - managed volume count

Additional tier-definition endpoints:

- `POST /api/tiers/{name}/levels`
- `PUT /api/tiers/{name}/levels/{level}`
- `DELETE /api/tiers/{name}/levels/{level}`

### Volumes

New endpoints:

- `GET /api/volumes`
- `POST /api/volumes`
- `GET /api/volumes/{id}`
- `PUT /api/volumes/{id}`
- `DELETE /api/volumes/{id}`
- `PUT /api/volumes/{id}/pin`
- `DELETE /api/volumes/{id}/pin`

`GET /api/volumes` returns:

- volume identity and mount point
- tier instance
- actual tier summary
- intended tier summary when spilled
- pinned flag
- aggregate heat summary
- active migration status
- per-region heat/migration map

### Policy

New endpoints:

- `GET /api/tiers/policy`
- `PUT /api/tiers/policy`

Per-tier target-fill, full-threshold, and tier-instance region-size settings are returned through the tier and tier-definition APIs, while global sampler and migration settings remain under `/api/tiers/policy`.

### Migration

No manual migration endpoint is added in the first complete-heat-engine implementation. Movement remains automatic and policy-driven.

---

## UI

### Tiers page

The Tiers page changes from "mounted tier roots" to "backing pools and policy":

- show ranked tier definitions, capacity, and target-fill percentages
- show each tier instance's configured region size
- show queued and active migrations
- add a policy panel for thresholds, intervals, and throttling

### Volumes page

Add a new Volumes page with:

- show the default auto-created managed volume for each tier
- create additional volume flow
- resize and delete actions
- pin toggle
- actual tier and spillover state
- aggregate heat indicator
- expandable region heat map
- migration progress and last movement reason

### Dashboard

Add:

- active migration count
- hot-region backlog
- tiers near spillover thresholds
- tiers above or below their target-fill balance

---

## Implementation Plan

### Work Package 1: Storage-model convergence

1. Add DB tables for managed volumes and regions.
2. Replace fixed-slot tier assignment with ranked tier definitions seeded with `NVME`, `SSD`, and `HDD`.
3. Add per-tier `target_fill_pct` and `full_threshold_pct`.
4. Add per-tier-instance `region_size_mb` at creation time.
5. Change tier provisioning so tiers create the VG plus a default `data` managed volume at `100%FREE`.
6. Add managed volume CRUD in the backend.
7. Mount the default managed volume at `/mnt/{tier}` and additional managed volumes under `/mnt/volumes/{tier}/{volume}`.

**Exit criteria**

- tier creation auto-provisions one full-capacity managed volume
- that default volume is tracked by the managed-volume and region model
- tier definitions are no longer hard-coded to exactly three fixed slots
- each tier has configurable target-fill and full-threshold values
- each tier instance has a region size defined at creation time
- operators can create additional managed volumes inside a tier when desired

### Work Package 2: Placement and reconciliation

1. Derive actual region placement from `seg_pe_ranges`.
2. Create and refresh region rows on volume create, extend, and startup.
3. Summarize region placement back to the volume level.

**Exit criteria**

- every managed volume has a full region inventory
- every region has a resolved current tier slot

### Work Package 3: dm-stats heat sampling

1. Add dm-stats wrapper package.
2. Ensure region definitions match each tier instance's configured region size.
3. Sample counters on the poll interval.
4. Compute rolling heat scores and persist them.

**Exit criteria**

- per-region heat scores update continuously for active volumes
- restart does not lose counter continuity

### Work Package 4: Spillover and migration engine

1. Implement spillover-aware LV create and extend.
2. Add migration queue and single-runner execution.
3. Use `pvmove` per region with progress tracking.
4. Verify final placement and recover cleanly after restart.
5. Add target-fill balancing so hotter data is packed upward to each tier's configured target bytes.

**Exit criteria**

- spilled extents are tracked and promoted back when capacity recovers
- region migrations run online and survive restart
- steady-state placement converges toward each tier's target-fill percentage

### Work Package 5: Policy engine

1. Add global policy config and validation.
2. Add hysteresis counters.
3. Rank regions by heat and distribute them downward through the ordered tiers based on target-fill percentages.
4. Respect pinning and skip migrating regions already in flight.
5. Support more than the seeded default tiers.

**Exit criteria**

- the hottest data moves up automatically until faster tiers reach target occupancy
- colder data drains downward automatically when faster tiers are above target occupancy
- pinned volumes never move

### Work Package 6: API, UI, and observability

1. Add `/api/volumes` and `/api/tiers/policy`.
2. Add frontend Volumes page and Tiers policy panel.
3. Expose migration progress and queue depth on the dashboard.
4. Extend monitor startup to reconcile heat-engine state.

**Exit criteria**

- operators can see and manage the full heat engine from the UI
- all runtime state is inspectable via API

---

## Rollout

### Phase 0: dark launch in code

- ship schema migration and managed-volume model
- do not enable policy movement yet
- expose the auto-created default volumes read-only first

### Phase 1: managed volumes enabled

- auto-create the default managed volume on tier creation
- allow creating additional managed volumes
- enable spillover and migration queue
- require any pre-existing tier roots to be removed and recreated before feature enablement

### Phase 2: heat sampling enabled

- dm-stats collection begins
- heat maps become visible
- policy remains disabled by default until validation data exists

### Phase 3: policy enablement

- enable policy engine behind a global flag
- default to conservative thresholds and migration rate caps

---

## Acceptance Criteria

- [ ] Database schema migration adds the managed-volume, region, and policy state required by the heat engine.
- [ ] Database schema migration replaces fixed slot definitions with ranked tier definitions seeded with `NVME`, `SSD`, and `HDD`.
- [ ] Tier creation accepts `region_size_mb`, default `256`, and persists it on the tier instance.
- [ ] New tier creation auto-creates a default managed `data` volume at `100%FREE` and mounts it at `/mnt/{tier}`.
- [ ] That default volume is represented in the managed-volume and region model rather than as a special-case tier root.
- [ ] Operators can list, resize, and delete managed volumes inside a tier instance, and can create additional managed volumes when free capacity exists.
- [ ] Every managed volume is divided into regions and each region has a resolved current tier.
- [ ] Region size is derived from the owning tier instance, not from a global policy setting.
- [ ] Each tier has a configurable `target_fill_pct`, default `50`.
- [ ] Operators can create additional ranked tiers beyond the seeded defaults `NVME`, `SSD`, and `HDD`.
- [ ] dm-stats heat is collected per region and persisted as a rolling score.
- [ ] New extents spill to colder tiers when the intended tier is full.
- [ ] A tier may continue receiving writes while above `target_fill_pct`; only the excess is drained downward in the background until the tier reaches target again.
- [ ] Region migration runs online via `pvmove` with tracked progress.
- [ ] Region placement is verified after each migration.
- [ ] Restart after a crash reconciles migration state and dm-stats state without corrupting placement metadata.
- [ ] The policy engine keeps the hottest data on the fastest tiers up to each tier's configured target occupancy.
- [ ] With 200 GiB of data on tiers sized 150 GiB NVME, 100 GiB SSD, and 1 TiB HDD, all at 50% target, steady-state placement converges toward 75 GiB on NVME, 50 GiB on SSD, and the remainder on HDD.
- [ ] Pinned volumes are excluded from automatic movement.
- [ ] `/api/volumes` and `/api/tiers/policy` expose the complete runtime state.
- [ ] The UI shows heat, spillover, migration progress, and policy controls.

---

## Risks and Mitigations

### dm-stats drift

Risk:
Region definitions may drift after LV resize or manual intervention.

Mitigation:

- reconcile dm-stats definitions on startup
- rebuild missing regions deterministically from DB policy

### Migration thrash

Risk:
Aggressive target-fill or full-threshold settings could cause repeated movement.

Mitigation:

- require consecutive wrong-tier cycles
- separate soft target-fill from hard full-threshold behavior
- keep conservative defaults

### Operator surprise during rollout

Risk:
Changing the implementation model underneath tier provisioning may surprise operators who still think the tier root is special.

Mitigation:

- surface the change clearly in the Tiers UI copy
- document that old tiers must be deleted and recreated after upgrade
- show the default root filesystem as a managed volume in the volume views

---

## Test Plan

### Backend

- [ ] Unit tests for region inventory creation from LV size and PE size.
- [ ] Unit tests for tier-instance `region_size_mb` validation and persistence.
- [ ] Unit tests for dm-stats sampling and rolling-window heat calculation.
- [ ] Unit tests for target-fill byte calculation per tier.
- [ ] Unit tests for ranked tier ordering and validation.
- [ ] Unit tests for spillover destination selection.
- [ ] Unit tests for migration queue ordering and single-runner behavior.
- [ ] Unit tests for restart recovery after interrupted `pvmove`.

### Integration

- [ ] Create a tier with seeded `NVME`, `SSD`, and `HDD` levels and verify the default `data` volume is created at `100%FREE`, mounted, and tracked as a managed volume.
- [ ] Create tier instances with different `region_size_mb` values and verify their region layouts differ accordingly.
- [ ] Create an additional ranked tier and verify it participates in ordering and balancing.
- [ ] Verify spillover when the highest-ranked tier is above `full_threshold_pct`.
- [ ] Verify the hottest data converges toward the configured target-fill percentages across tiers.
- [ ] Verify pinning blocks automatic movement.
- [ ] Verify migration progress survives `tierd` restart.

### UI

- [ ] Volumes page displays region heat and migration state correctly.
- [ ] Tiers page displays ranked tier definitions, region size, target-fill settings, policy configuration, and queue depth correctly.

---

## Owner and Effort

- **Owner:** SmoothNAS mdadm/LVM storage path
- **Effort:** XL
