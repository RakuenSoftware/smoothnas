# Proposal: Unified Tiering — 01: Common Control-Plane Model

**Status:** Pending
**Date:** 2026-04-09
**Updated:** 2026-04-12
**Depends on:** mdadm-complete-heat-engine
**Part of:** unified-tiering-control-plane
**Followed by:** unified-tiering-02-mdadm-adapter

---

## Problem

SmoothNAS has two storage backends (mdadm/LVM and ZFS) that each need a tiering story, but no shared vocabulary for targets, namespaces, placement, or policy. Without a common model, the UI cannot present one coherent placement workflow, adapters cannot be added without rewiring everything, and the operator-facing API will diverge per backend.

This proposal defines the shared control-plane model that all subsequent unified-tiering proposals build on.

---

## Goals

1. Define canonical Go types for the control-plane model.
2. Define the `TieringAdapter` interface that every backend must satisfy.
3. Define the SQLite schema for control-plane state.
4. Define the unified `/api/tiering/*` API surface.
5. Define activity band values and per-adapter derivation rules.
6. Define a typed error taxonomy for adapter methods.
7. Define revision and invalidation rules for movement jobs.
8. Define placement-domain grouping rules for the GUI.
9. Define control-plane health monitoring signals.

---

## Non-goals

- Any backend-specific implementation (covered by proposals 02–04B).
- GUI implementation beyond grouping and honesty rules (covered by proposal 05).

---

## Decision

The control plane owns placement intent, ranked target definitions, target-fill policy, normalized health and activity summaries, movement scheduling and status, pin requests, and the operator-facing API and UI.

The data plane owns actual reads and writes, backend-native heat collection, backend-native migration mechanics, and backend-native recovery.

The control plane unifies operator workflow. It does not claim identical backend behavior.

---

## Canonical Go Types

### TierTarget

```go
type TierTarget struct {
    ID               string
    Name             string
    PlacementDomain  string
    Rank             int
    BackendKind      string
    TargetFillPct    int
    FullThresholdPct int
    CapacityBytes    uint64
    UsedBytes        uint64
    Health           string
    ActivityBand     string
    ActivityTrend    string
    QueueDepth       int
    Capabilities     TargetCapabilities
    BackingRef       string
    BackendDetails   map[string]any
}
```

### TargetCapabilities

```go
type TargetCapabilities struct {
    MovementGranularity string // region | file | object
    PinScope            string // volume | namespace | object | none
    SupportsOnlineMove  bool
    SupportsRecall      bool
    RecallMode          string // none | synchronous | asynchronous
    SnapshotMode        string // none | backend-native | coordinated-namespace
    FUSEMode            string // passthrough | fallback | n/a
    SupportsChecksums   bool
    SupportsCompression bool
}
// Note: SupportsWriteBias was removed — no adapter uses it and it was never defined.
// It may be reintroduced in a future proposal with a concrete definition.
```

### ManagedNamespace

```go
type ManagedNamespace struct {
    ID              string
    Name            string
    PlacementDomain string
    BackendKind     string
    NamespaceKind   string // volume | filespace
    ExposedPath     string
    PolicyTargetIDs []string
    PinState        string
    Health          string
    ActivityBand    string
    PlacementState  string
    CapacityBytes   uint64
    UsedBytes       uint64
    BackendDetails  map[string]any
}
// CapacityBytes and UsedBytes reflect aggregate capacity/usage across all backing
// targets for this namespace, as reported by the adapter on each CollectActivity() cycle.
```

### ManagedObject

```go
type ManagedObject struct {
    ID               string
    NamespaceID      string
    ObjectKind       string // volume | file | object
    ObjectKey        string
    PinState         string // none | pinned-hot | pinned-cold
    ActivityBand     string
    PlacementSummary PlacementSummary
    BackendRef       string
    BackendDetails   map[string]any
}

type PlacementSummary struct {
    CurrentTargetID  string
    IntendedTargetID string
    State            string // placed | moving | stale | unknown
}
```

---

## Activity Band Definitions

`activity_band` values:

| Value | Meaning |
| --- | --- |
| `hot` | Sustained high access rate; candidate for the fastest tier |
| `warm` | Moderate, intermittent access; candidate for intermediate tiers |
| `cold` | Infrequent access; candidate for cold tier |
| `idle` | No recent access within the collection window; candidate for cold or archive |

`activity_trend` values: `rising`, `stable`, `falling`.

**Trend derivation**: `activity_trend` is computed by the adapter during `CollectActivity()` by comparing the current band assignment to the band from the previous collection cycle. If the band moved to a hotter value (`idle`→`cold`, `cold`→`warm`, or `warm`→`hot`), trend is `rising`. If it moved to a colder value, trend is `falling`. If unchanged, `stable`. For the first collection cycle after startup (no prior sample), trend is reported as `stable`.

Band assignment is backend-specific. Each adapter must document its mapping:

- **mdadm/LVM**: derived from dm-stats region I/O rate over the sampling window. The per-region IOPS distribution is divided into four buckets relative to the **95th-percentile** IOPS rate across all regions in the tier instance. Using the 95th percentile rather than the maximum prevents a single pathological hot region from biasing all others into cold/idle. Hot = top quartile, warm = second, cold = third, idle = bottom quartile or zero-rate regions.
- **managed ZFS**: derived from file or object access frequency counters maintained by the FUSE namespace service. Hot = accessed more than once per hour, warm = once per day, cold = once per week, idle = not accessed within the collection window.

The control plane must not compare band derivation values across adapters.

---

## TieringAdapter Interface

```go
type TieringAdapter interface {
    Kind() string

    // Target lifecycle
    CreateTarget(spec TargetSpec) (*TargetState, error)
    DestroyTarget(targetID string) error
    ListTargets() ([]TargetState, error)

    // Namespace lifecycle
    CreateNamespace(spec NamespaceSpec) (*NamespaceState, error)
    DestroyNamespace(namespaceID string) error
    ListNamespaces() ([]NamespaceState, error)
    ListManagedObjects(namespaceID string) ([]ManagedObjectState, error)

    // Capabilities and policy
    GetCapabilities(targetID string) (TargetCapabilities, error)
    GetPolicy(targetID string) (TargetPolicy, error)
    SetPolicy(targetID string, policy TargetPolicy) error

    // Reconciliation and activity
    Reconcile() error
    CollectActivity() ([]ActivitySample, error)

    // Movement
    PlanMovements() ([]MovementPlan, error)
    StartMovement(plan MovementPlan) (string, error)
    GetMovement(id string) (*MovementState, error)
    CancelMovement(id string) error

    // Pinning
    Pin(scope PinScope, namespaceID string, objectID string) error
    Unpin(scope PinScope, namespaceID string, objectID string) error

    // Degraded state
    GetDegradedState() ([]DegradedState, error)
}
```

`CollectActivity` returns samples that the control plane stores in backend-native tables; the adapter must not assume side effects outside its own persistence layer.

`CancelMovement` must abort an in-progress movement and leave the source object authoritative. The adapter is responsible for cleaning up any partial copy.

`ListManagedObjects` returns an empty slice for region-granularity adapters. The unified API's objects endpoint provides region inventory for those adapters by querying backend-native state directly (see the API section).

### Supporting Types

```go
type TargetSpec struct {
    Name             string
    PlacementDomain  string
    Rank             int
    BackendKind      string
    TargetFillPct    int
    FullThresholdPct int
    BackingRef       string
}

type TargetState struct {
    Target   TierTarget
    Revision int64
}

type NamespaceSpec struct {
    Name            string
    PlacementDomain string
    BackendKind     string
    NamespaceKind   string
    ExposedPath     string
    PolicyTargetIDs []string
}

type NamespaceState struct {
    Namespace ManagedNamespace
    Revision  int64
}

type ManagedObjectState struct {
    Object   ManagedObject
    Revision int64
}

type TargetPolicy struct {
    TargetFillPct    int
    FullThresholdPct int
    PolicyRevision   int64
}

type ActivitySample struct {
    TargetID      string
    NamespaceID   string
    ObjectID      string // empty for target-level samples
    ActivityBand  string
    ActivityTrend string
    SampledAt     time.Time
}

type MovementPlan struct {
    NamespaceID     string
    ObjectID        string // empty for namespace-level (region-granularity) moves
    SourceTargetID  string
    DestTargetID    string
    PlacementDomain string
    PolicyRevision  int64
    IntentRevision  int64
    PlannerEpoch    int64
    TriggeredBy     string
}

type MovementState struct {
    ID            string
    Plan          MovementPlan
    State         string // pending | running | done | failed | cancelled | stale
    ProgressBytes int64
    TotalBytes    int64
    FailureReason string
    StartedAt     *time.Time
    UpdatedAt     time.Time
    CompletedAt   *time.Time
}

type PinScope struct {
    Kind string // "namespace" | "object"
}

type DegradedState struct {
    ID          string
    BackendKind string
    ScopeKind   string // "target" | "namespace" | "domain" | "backend"
    ScopeID     string // ID of the relevant entity; for scope_kind="backend", the backend kind string
    Severity    string // "warning" | "critical"
    Code        string
    Message     string
    UpdatedAt   time.Time
    ResolvedAt  *time.Time // set when condition clears; nil if still active
}
```

---

## Error Taxonomy

```go
type AdapterError struct {
    Kind    AdapterErrorKind
    Message string
    Cause   error
}

type AdapterErrorKind string

const (
    // ErrTransient: temporary backend condition; the control plane may retry after backoff.
    ErrTransient AdapterErrorKind = "transient"

    // ErrPermanent: operator action required before the operation can succeed.
    ErrPermanent AdapterErrorKind = "permanent"

    // ErrCapabilityViolation: adapter does not support the requested operation.
    ErrCapabilityViolation AdapterErrorKind = "capability_violation"

    // ErrStaleRevision: policy or intent revision mismatch.
    ErrStaleRevision AdapterErrorKind = "stale_revision"

    // ErrBackendDegraded: backend cannot safely execute in its current health state.
    ErrBackendDegraded AdapterErrorKind = "backend_degraded"
)
```

Methods that start a background operation (e.g., `StartMovement`) return `ErrTransient` if the backend is temporarily unavailable and `ErrPermanent` if the plan is structurally invalid. They must not block until the background operation completes.

---

## SQLite Schema

### `placement_domains`

- `id` (the domain name string; stable across restarts)
- `backend_kind`
- `description`
- `created_at`
- `updated_at`

Created automatically when the first `tier_target` referencing that domain name is registered. Removed when no targets remain in it **and** no active (non-terminal) `placement_intents` or `movement_jobs` rows reference that domain. Removal is deferred until the end of the planner cycle.

### `tier_targets`

- `id`, `name`, `placement_domain`, `backend_kind`, `rank`
- `target_fill_pct`, `full_threshold_pct`, `policy_revision`
- `health`, `activity_band`, `activity_trend`
- `capabilities_json`, `backing_ref`
- `created_at`, `updated_at`

### `managed_namespaces`

- `id`, `name`, `placement_domain`, `backend_kind`, `namespace_kind`
- `exposed_path`, `pin_state`, `intent_revision`
- `health`, `placement_state`, `backend_ref`
- `capacity_bytes`, `used_bytes`
- `policy_target_ids_json`
- `created_at`, `updated_at`

### `managed_objects`

- `id`, `namespace_id`, `object_kind`, `object_key`
- `pin_state`, `activity_band`
- `placement_summary_json`, `backend_ref`
- `updated_at`

### `movement_jobs`

- `id`, `backend_kind`, `namespace_id`, `object_id` (nullable)
- `movement_unit`, `placement_domain`
- `source_target_id`, `dest_target_id`
- `policy_revision`, `intent_revision`, `planner_epoch`
- `state`, `triggered_by`
- `progress_bytes`, `total_bytes`, `failure_reason`
- `started_at`, `updated_at`, `completed_at`

#### Movement Job State Machine

Valid states and transitions:

| From | To | Trigger |
| --- | --- | --- |
| `pending` | `running` | Scheduler calls `StartMovement` successfully |
| `pending` | `stale` | Revision mismatch detected on planner cycle |
| `pending` | `cancelled` | Operator calls `DELETE /api/tiering/movements/{id}` |
| `running` | `done` | `GetMovement` reports terminal success |
| `running` | `failed` | `GetMovement` reports terminal failure |
| `running` | `stale` | Revision mismatch detected while job is in flight |
| `running` | `cancelled` | Operator calls cancel; `CancelMovement` returns successfully |
| `stale` | `pending` | Replanned on next planner cycle if conditions are still met |

`done`, `failed`, and `cancelled` are terminal — no further transitions. `stale` jobs not replanned within 24 hours transition to `failed` with reason `stale_timeout`. Terminal jobs are retained 30 days then purged.

### `placement_intents`

- `id`, `namespace_id`, `object_id` (nullable; null = namespace-level intent)
- `intended_target_id`, `placement_domain`
- `policy_revision`, `intent_revision`
- `reason`, `state`, `updated_at`

#### Placement Intent Lifecycle

A `placement_intent` expresses where the control plane wants a namespace or object to reside. Lifecycle:

1. Planner creates/updates a `placement_intent` when a move is needed. State: `pending`.
2. Planner creates a `movement_job` and sets intent state to `queued`. At most one non-terminal job per intent exists; the planner skips intents that already have a `pending` or `running` job.
3. Job reaches `done` → `intent_revision` increments, intent state becomes `satisfied`.
4. Job reaches `failed` or `cancelled` → intent state reverts to `pending` for re-evaluation.
5. Job goes `stale` → intent state reverts to `pending`.
6. No associated job (placement already correct) → state `satisfied`.

Satisfied intents older than 7 days are purged by the scheduler.

### `degraded_states`

- `id`, `backend_kind`, `scope_kind`, `scope_id`
- `severity`, `code`, `message`, `updated_at`
- `resolved_at` (nullable): set when the condition clears. Rows with `resolved_at` older than 7 days are purged by the scheduler.

`scope_kind` valid values: `target`, `namespace`, `domain`, `backend`. For `scope_kind = backend`, `scope_id` is the backend kind string (e.g., `"mdadm"`).

### `control_plane_config`

Key/value configuration table for control-plane and adapter operational parameters:

| Key | Default | Description |
| --- | --- | --- |
| `movement_queue_depth_warn` | 50 | Alert when more than this many jobs are in `pending` |
| `movement_queue_age_minutes` | 30 | Alert when a `pending` job is older than this many minutes |
| `movement_failed_rate_window_minutes` | 60 | Sliding window for failed movement rate alerting |
| `movement_failed_rate_max` | 10 | Alert when more than this many jobs fail within the window |
| `reconciliation_max_age_minutes` | 60 | Alert when reconciliation has not run within this interval |
| `planner_interval_minutes` | 15 | Scheduler cycle interval |
| `reconcile_debounce_seconds` | 60 | Minimum seconds between manual `POST /api/tiering/reconcile` calls |
| `migration_io_high_water_pct` | 80 | Movement workers pause when device utilization exceeds this percentage |
| `recall_timeout_seconds` | 300 | Maximum seconds for synchronous recall before returning EIO |
| `movement_worker_concurrency` | 4 | Maximum concurrent movement workers per adapter |

Backend-specific detail (e.g., mdadm region heat rows, managed ZFS object placement metadata) remains in backend-native tables. The control plane stores normalized summaries and identifiers only.

---

## Revision and Invalidation Rules

- Every target policy change increments `policy_revision`.
- Every confirmed placement change increments `intent_revision`. A placement change is defined as: a movement job transitioning to `done`; a pin request completing; or a `Reconcile()` call that modifies any placement row. Enqueueing a movement job does not increment `intent_revision` until the move completes. This ensures the invalidation rule reacts to committed state changes, not scheduled intentions.
- Every planner run records a `planner_epoch`.

A movement job is valid only if its recorded domain, policy revision, and intent revision still match current state. Otherwise the job must be marked stale and replanned.

Cross-domain movements are rejected by the control plane before reaching the adapter. The `PlacementDomain` on the source and destination target must be equal; a mismatch returns HTTP 422 from the API layer.

---

## Unified API

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/tiering/domains` | GET | List placement domains with member target count and health summary |
| `/api/tiering/domains/{id}` | GET | Domain detail: member targets, rank order, fill state |
| `/api/tiering/targets` | GET | List all tier targets across adapters |
| `/api/tiering/targets/{id}` | GET | Detailed target view including capabilities and backend details |
| `/api/tiering/targets/{id}/policy` | PUT | Update target fill and threshold policy |
| `/api/tiering/namespaces` | GET | List managed namespaces |
| `/api/tiering/namespaces` | POST | Create a managed namespace |
| `/api/tiering/namespaces/{id}` | GET | Namespace detail with placement and activity summaries |
| `/api/tiering/namespaces/{id}` | DELETE | Destroy a managed namespace |
| `/api/tiering/namespaces/{id}/pin` | PUT | Apply a namespace-level pin when supported |
| `/api/tiering/namespaces/{id}/pin` | DELETE | Remove a namespace-level pin |
| `/api/tiering/namespaces/{id}/snapshot` | POST | Create a coordinated namespace snapshot (adapter must support `coordinated-namespace`) |
| `/api/tiering/namespaces/{id}/objects` | GET | Object listing for a namespace. For adapters with `MovementGranularity = file` or `object`, returns managed objects via `ListManagedObjects`. For adapters with `MovementGranularity = region`, the API returns the namespace's region inventory in a normalized format by querying backend-native state directly; `ListManagedObjects` is not called. |
| `/api/tiering/namespaces/{id}/objects/{object_id}` | GET | Namespace-qualified managed-object detail |
| `/api/tiering/namespaces/{id}/objects/{object_id}/pin` | PUT | Apply an object-level pin when supported |
| `/api/tiering/namespaces/{id}/objects/{object_id}/pin` | DELETE | Remove an object-level pin |
| `/api/tiering/movements` | GET | List movement jobs across adapters |
| `/api/tiering/movements/{id}` | DELETE | Cancel a movement job |
| `/api/tiering/degraded` | GET | List degraded-state signals across adapters |
| `/api/tiering/reconcile` | POST | Trigger control-plane reconciliation (debounced; see below) |

`POST /api/tiering/reconcile` is debounced: calls within `reconcile_debounce_seconds` of the last successful reconcile return HTTP 429 with a `Retry-After` header set to the remaining wait time.

**Known gap — pagination**: All list endpoints are currently unbounded. Cursor-based pagination will be added before these endpoints are expected to serve large result sets. Implementers should not assume the list is always small.

Backend-specific APIs (`/api/tiers`, `/api/volumes`, `/api/pools`, etc.) remain valid and authoritative for deep admin tasks. Concurrent writes through both surfaces are not protected by mutual exclusion at the control-plane level; operators should choose one surface for policy management per tier instance.

---

## Authentication

All `/api/tiering/*` endpoints require a valid session token, enforced by the same `middleware.RequireAuth` middleware applied to all other authenticated API routes. The health monitoring signals emitted to `/api/health` (the unauthenticated endpoint) include only aggregate counts and severity, not placement details.

---

## Adapter Registration

The control plane exposes a registration surface that adapters call on startup:

```go
type ControlPlane interface {
    RegisterAdapter(adapter TieringAdapter) error
    UnregisterAdapter(kind string) error
}
```

`RegisterAdapter` is called once per adapter kind per tierd process lifetime. Calling it twice with the same kind returns an error. Adapters that fail registration are excluded from the unified API but do not prevent tierd from starting.

On tierd startup, built-in adapters (mdadm, managed ZFS) are registered in order before any API traffic is served. The unified API endpoints return empty lists, not 404 or 500, when no adapters are registered.

Hot-plug (adding a new adapter kind to a running tierd) is not supported in this phase. Adding a new backend requires a tierd restart.

`UnregisterAdapter` is called only during an orderly tierd shutdown. On unregistration, the control plane:
1. Stops the scheduler goroutine for that adapter (no new planner cycles).
2. Waits up to 30 seconds for any in-progress `StartMovement` or `CancelMovement` calls to return.
3. Marks all `running` movement jobs for that backend as `failed` with reason `adapter_unregistered`.
4. Leaves `degraded_states` rows intact; they will be addressed by the next tierd startup's reconciliation.

Stopping a managed ZFS FUSE daemon is the adapter's own shutdown responsibility, called before `UnregisterAdapter` returns.

---

## GUI Placement-Domain Grouping

Targets in the tiering inventory are grouped by `placement_domain`. Each domain is a collapsible section with a header showing domain name, backend kind, aggregate health, and aggregate used/capacity. Targets are sorted by rank ascending within their section. The GUI must not imply that rank is comparable across domains or present a cross-domain movement option.

---

## Control-Plane Scheduler

The control plane runs a background scheduler goroutine per registered adapter. Each scheduler:

1. Sleeps for `planner_interval` (default 15 minutes; configurable in `control_plane_config`).
2. Calls `adapter.CollectActivity()` and stores the returned `ActivitySample` rows in backend-native activity tables.
3. Calls `adapter.PlanMovements()` and records each returned `MovementPlan` as a `movement_jobs` row in state `pending`.
4. Dequeues `pending` movement jobs and calls `adapter.StartMovement(plan)`.
5. Polls `pending` and `running` movement jobs by calling `adapter.GetMovement(id)` until they reach a terminal state.
6. Invalidates any `movement_jobs` row whose `policy_revision` or `intent_revision` no longer matches current state; marks it `stale`.
7. Purges resolved `degraded_states` older than 7 days, terminal `movement_jobs` older than 30 days, and satisfied `placement_intents` older than 7 days.

`planner_epoch` is a monotonically increasing counter incremented once per scheduler cycle. Movement jobs created in epoch N are invalidated if the scheduler detects state changes before epoch N+1 completes.

Only one scheduler cycle runs at a time per adapter. If a cycle takes longer than `planner_interval`, the next cycle begins immediately after the current one finishes rather than skipping.

`adapter.PlanMovements()` and `adapter.StartMovement()` are never called concurrently for the same adapter.

---

## Control-Plane Health Monitoring

| Check | Source | Alert condition |
| --- | --- | --- |
| Movement queue depth | `movement_jobs` | more than N jobs in `pending` for more than T minutes |
| Stale movement jobs | `movement_jobs` | any job older than `updated_at + max_job_age` still in `running` |
| Degraded-state count | `degraded_states` | any row with `severity = critical` |
| Failed movement rate | `movement_jobs` | more than N jobs to `failed` within a sliding window |
| Reconciliation staleness | last `Reconcile()` timestamp | reconciliation has not run within the expected interval |

---

## Steady-State Constraints

Not allowed:
- per-read placement RPCs
- per-write control-plane approval
- a global lock that stalls the data plane while policy evaluates

---

## Effort

**M** — multiple new DB tables, new API surface, adapter registration, scheduler goroutine, health monitoring. No backend implementation.

---

## Acceptance Criteria

- [ ] `TierTarget`, `ManagedNamespace`, `ManagedObject`, and `TargetCapabilities` Go types exist.
- [ ] `TieringAdapter` interface compiles and is documented.
- [ ] `AdapterError` and `AdapterErrorKind` types exist with all five error kinds.
- [ ] SQLite schema migrations exist for all tables in this proposal.
- [ ] `placement_domains` rows are created automatically on `tier_target` insert and removed only after all active intents and jobs for that domain reach terminal state.
- [ ] All unified API endpoints exist and return empty lists (not 404/500) when no adapters are registered.
- [ ] Movement jobs are invalidated when `policy_revision` or `intent_revision` changes.
- [ ] Control-plane health monitoring signals are emitted under the defined alert conditions.
- [ ] All types referenced in `TieringAdapter` compile: `ActivitySample`, `MovementPlan`, `MovementState`, `TargetSpec`, `NamespaceSpec`, `TargetState`, `NamespaceState`, `ManagedObjectState`, `TargetPolicy`, `DegradedState`, `PinScope`.
- [ ] `RegisterAdapter` returns an error if the same kind is registered twice.
- [ ] `planner_epoch` is incremented once per scheduler cycle and recorded on new movement jobs.
- [ ] `intent_revision` is incremented only on confirmed placement changes, not on job enqueueing.
- [ ] `degraded_states` rows with `resolved_at` older than 7 days are purged by the scheduler.
- [ ] Terminal `movement_jobs` rows older than 30 days are purged by the scheduler.
- [ ] Satisfied `placement_intents` older than 7 days are purged by the scheduler.
- [ ] All `/api/tiering/*` endpoints require a valid session token.
- [ ] `POST /api/tiering/reconcile` returns HTTP 429 within `reconcile_debounce_seconds` of the last call.
- [ ] Cross-domain movement attempts return HTTP 422.
- [ ] Movement job state transitions follow the state machine table; unlisted transitions are rejected.
- [ ] A `placement_intent` with state `queued` does not produce a second `movement_job` on the next planner cycle.
- [ ] `activity_trend` is computed by comparing current band to the prior band; first-cycle trend is `stable`.
- [ ] `managed_namespaces` rows include `capacity_bytes`, `used_bytes`, and `policy_target_ids_json` columns.
- [ ] `UnregisterAdapter` marks all running movement jobs for that backend as `failed` with reason `adapter_unregistered`.

## Test Plan

- [ ] Unit tests for `policy_revision` and `intent_revision` invalidation logic.
- [ ] Unit tests for `placement_domain` deferred removal — not removed while active intents or jobs exist.
- [ ] Unit tests for `AdapterError` kind classification covering all five kinds.
- [ ] Unit tests for activity-band normalization: correct band for each adapter's derivation rule given sample inputs.
- [ ] Unit tests for `activity_trend` derivation: rising, stable, and falling transitions; stable on first cycle.
- [ ] Unit tests for movement job state machine: valid transitions succeed; invalid transitions (e.g., `done` → `running`) are rejected.
- [ ] Unit tests for placement intent lifecycle: job `done` → intent `satisfied` + `intent_revision` increment; job `failed` → intent reverts to `pending`.
- [ ] Integration tests that all unified API endpoints return correct responses with a stub adapter registered.
- [ ] Integration test: cross-domain movement returns HTTP 422.
- [ ] Integration test: `POST /api/tiering/reconcile` within the debounce window returns HTTP 429 with `Retry-After`.
- [ ] Integration test: terminal movement jobs purged after 30 days; resolved degraded states purged after 7 days; satisfied placement intents purged after 7 days.
