# Proposal: Unified Tiering — 02: mdadm/LVM Adapter

**Status:** Pending
**Depends on:** unified-tiering-01-common-model, mdadm-complete-heat-engine
**Part of:** unified-tiering-control-plane
**Preceded by:** unified-tiering-01-common-model
**Followed by:** unified-tiering-03-zfs-raw-backend

---

## Problem

The mdadm/LVM heat-tiering system (`mdadm-complete-heat-engine`) has its own backend-specific schema, API, and placement model. It needs to be mapped into the unified control-plane model so that mdadm tier targets and managed volumes appear in the common API, and so that future backends can be added without changing how operators interact with mdadm tiering.

This proposal also covers migration of existing mdadm tier state into the unified schema.

---

## Goals

1. Implement `TieringAdapter` for the mdadm/LVM backend.
2. Map existing mdadm tier pools and managed volumes into `tier_targets` and `managed_namespaces`.
3. Surface mdadm activity summaries as normalized `activity_band` values.
4. Expose mdadm movement jobs and pin state through the unified API.
5. Migrate existing mdadm tier state on first start after upgrade without operator action.

---

## Non-goals

- Changes to the mdadm heat engine itself (covered by `mdadm-complete-heat-engine`).
- ZFS or any other backend (covered by proposals 03 and 04).
- The unified GUI (covered by proposal 05).

---

## Assumed Preconditions

`mdadm-complete-heat-engine` is complete and in `testing`. The following are implemented and stable:

- dm-stats region sampling
- policy hysteresis
- `pvmove`-based online migration
- pin support at the managed volume level

This proposal remaps that existing machinery into the unified schema. It does not introduce new heat-engine capability.

---

## Native Model

| Property | Value |
| --- | --- |
| Backing pool | tier instance volume group |
| Placement unit | region |
| Heat source | dm-stats |
| Movement mechanism | scoped `pvmove` |
| Write-path behavior | highest-ranked writable tier until full threshold |
| Steady-state balancing | target-fill policy and hysteresis |
| Pin scope | managed volume |

---

## Adapter Mapping

### TierTarget

Each mdadm tier (fast/warm/cold) inside a tier instance maps to one `TierTarget`:

- `PlacementDomain`: the tier instance name
- `Rank`: tier rank within the instance
- `BackendKind`: `"mdadm"`
- `MovementGranularity`: `"region"`
- `PinScope`: `"volume"`
- `SupportsOnlineMove`: `true`
- `SupportsRecall`: `false`
- `RecallMode`: `"none"`
- `SnapshotMode`: `"none"`. mdadm/LVM volumes do not have a backend-native snapshot primitive exposed through tierd. If the underlying filesystem (e.g. ext4 with LVM snapshots) supports snapshots via a separate mechanism, that is out of scope for the unified adapter.
- `FUSEMode`: `"n/a"`

### ManagedNamespace

Each mdadm managed volume maps to one `ManagedNamespace`:

- `NamespaceKind`: `"volume"`
- `ExposedPath`: existing mount path
- `BackendKind`: `"mdadm"`

### ActivityBand Derivation

The dm-stats region I/O rate is sampled over the configured window. The per-region IOPS distribution within the tier instance is divided into four buckets relative to the **95th-percentile** IOPS rate across all regions in the instance. Using the 95th percentile rather than the maximum prevents a single pathological hot region from biasing all others into cold/idle.

- `hot`: top quartile
- `warm`: second quartile
- `cold`: third quartile
- `idle`: bottom quartile or zero-rate regions

The per-volume `activity_band` is derived from the **mode** (most frequently occurring band) across all of the volume's regions. If no single band is strictly the mode, the hottest band among the tied candidates is used. A volume with no regions reports `idle`.

---

## Fill Policy Semantics

`target_fill_pct` and `full_threshold_pct` are enforced at extent allocation time by the existing mdadm heat engine:

- Below `target_fill_pct`: new extents land on this tier normally.
- Between `target_fill_pct` and `full_threshold_pct`: background policy schedules outbound region movement.
- Above `full_threshold_pct`: new writes spill to the next-lower-ranked writable tier. If none exists, `no_drain_target` degraded state is reported.

The unified control plane reads these values from the adapter; it does not enforce them itself.

---

## Migration

On first start after upgrade, `tierd` must migrate existing mdadm tier state into the unified control-plane schema before serving API traffic. Migration rules:

- Each existing `tier_pool` becomes one `placement_domain`. Its tiers become `tier_targets` within that domain.
- Each existing `managed_volume` becomes one `managed_namespace` with `backend_kind = "mdadm"` and `namespace_kind = "volume"`.
- Existing policy rows (fill percentage, full threshold) are preserved in `tier_targets`. Additional heat-engine policy parameters (hysteresis window, sample interval, activity threshold) are preserved in the backend-native `tier_policy_config` table and remain accessible via the backend-specific `/api/tiers/{name}/policy` endpoint. They are not surfaced through the unified control-plane schema; the unified adapter reads them from the native table on each policy evaluation.
- Terminal movement job state is discarded. In-progress and **queued** jobs are migrated to `movement_jobs`: in-progress jobs are migrated with state `running`; queued jobs are migrated with state `pending`. Both are subject to the standard revision-invalidation rules on the next planner cycle.
- Migration runs inside a SQLite transaction. On error, it rolls back and tierd exits rather than serving partial state.
- Migration is idempotent: re-running against an already-migrated database produces no duplicate rows.
- Migration state is recorded in a `schema_migrations` row with key `mdadm_unified_v1`. The migration runs only if this row is absent. Re-running migration on an already-migrated database is a no-op — it does not produce duplicate rows and does not error.

No operator action is required. Existing tier instances and managed volumes must be visible in the unified API immediately after upgrade.

---

### Pin Behaviour

mdadm supports pinning at the volume (namespace) level only. `Pin(scope, namespaceID, objectID)` behaves as follows:

- If `scope.Kind == "namespace"` and `objectID == ""`: pin the managed volume. Succeeds.
- If `scope.Kind == "object"` or `objectID != ""`: return `ErrCapabilityViolation` — mdadm does not support object-level pins.

Callers must check `TargetCapabilities.PinScope == "volume"` before attempting object-level pins.

---

## Pin and Movement Interaction

When a volume is pinned via `Pin(scope="namespace", ...)`:

1. The adapter sets the volume's `pin_state` in the managed-volume store.
2. Any `pending` movement jobs that have this namespace as source or destination are immediately cancelled (the adapter calls `CancelMovement` for each).
3. Any `running` movement job for this namespace has `pvmove --abort` issued. The movement job transitions to `cancelled`.
4. The planner must not create new movement jobs for a pinned namespace. `PlanMovements` must skip pinned namespaces.

When a volume is unpinned, the planner resumes normal evaluation on the next cycle.

---

## Degraded States

| Code | Condition |
| --- | --- |
| `no_drain_target` | Target above `full_threshold_pct` with no colder writable target |
| `movement_failed` | A `pvmove` job failed or was interrupted |
| `reconciliation_required` | Adapter state diverged from control-plane intent on startup |
| `placement_intent_stale` | Intent revision mismatch detected |

---

## Movement Cancellation

When `CancelMovement` is called for an mdadm movement job:

1. The adapter calls `pvmove --abort <lv_path>` to stop the in-progress physical extent transfer. LVM reverts any partially moved extents to the source physical volume automatically.
2. If `pvmove --abort` fails (e.g., the job already completed or was never started), the adapter checks `GetMovement` to determine the current state and returns the appropriate result.
3. Once abort completes, the source logical volume is authoritative. No cleanup of a partial destination copy is required — LVM handles this atomically.
4. The movement job transitions to `cancelled`.

Callers must not assume the abort is instantaneous; `pvmove --abort` may take several seconds to complete for a large in-progress transfer.

---

## Effort

**M** — remaps existing models; migration logic requires transaction safety, idempotency, and handling of queued/in-progress state.

---

## Acceptance Criteria

- [ ] mdadm `TieringAdapter` implementation satisfies the interface from proposal 01.
- [ ] mdadm tier targets appear in `/api/tiering/targets` with correct rank, fill policy, and capability fields.
- [ ] mdadm managed volumes appear in `/api/tiering/namespaces`.
- [ ] Activity bands are derived from dm-stats per the four-bucket rule and appear in target detail.
- [ ] Pin state and movement status are visible through the unified API.
- [ ] Target-fill policy can be updated through `/api/tiering/targets/{id}/policy`.
- [ ] After upgrade, existing tier instances and managed volumes appear without re-registration.
- [ ] Migration transaction rolls back cleanly and tierd exits on migration error.
- [ ] Migration is idempotent across multiple tierd restarts.
- [ ] All four degraded-state codes are emitted under the correct conditions.
- [ ] `queued` regions in the legacy schema are migrated to `movement_jobs` with state `pending`.
- [ ] Migration is gated on the absence of a `schema_migrations` row with key `mdadm_unified_v1`; the row is written on successful migration.
- [ ] `Pin` with `scope.Kind == "object"` returns `ErrCapabilityViolation`.
- [ ] Per-volume `activity_band` uses the mode rule described in the ActivityBand Derivation section.
- [ ] `pvmove --abort` is issued on movement cancellation; LVM handles partial extent cleanup.
- [ ] Pinning a namespace cancels its active movement jobs and prevents new ones from being planned.

## Test Plan

- [ ] Unit tests for mdadm-to-`TierTarget` mapping for each tier rank.
- [ ] Unit tests for activity-band derivation from dm-stats samples per the four-bucket rule.
- [ ] Unit tests for migration from legacy schema: correct rows produced in `tier_targets` and `managed_namespaces`.
- [ ] Unit test that a migration failure inside a transaction leaves the database unchanged.
- [ ] Unit test that re-running migration on an already-migrated database produces no duplicate rows.
- [ ] Integration tests that mdadm targets and namespaces appear correctly in the unified API after adapter registration.
- [ ] Integration test for `no_drain_target` degraded state when all tiers are above threshold.
- [ ] Integration test for movement job visibility through `/api/tiering/movements`.
- [ ] Unit test: `pvmove --abort` is called when `CancelMovement` is invoked for a running movement job; movement transitions to `cancelled`.
- [ ] Unit test: `PlanMovements` skips pinned namespaces; pinning a namespace cancels its pending and running movement jobs.
- [ ] Unit test: activity-band derivation with one pathological outlier region confirms the 95th-percentile anchor prevents the outlier from inflating all other bands.
