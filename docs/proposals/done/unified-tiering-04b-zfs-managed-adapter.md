# Proposal: Unified Tiering — 04B: Managed ZFS TieringAdapter

> **2026-04-24 update — FUSE has been removed.** The user-space
> `tierd-fuse-ns` daemon, `tierd/internal/tiering/fuse/` package, and
> related `fuse_mode` / `daemon_state` tracking are gone. `smoothfs`
> (the in-tree stacked kernel module) is the only data plane. Where
> this proposal refers to the FUSE daemon, the FUSE socket protocol,
> or the `mdadm`/`zfsmgd` FUSE handlers, treat those sections as
> historical context.



**Status:** Done
**Depends on:** unified-tiering-01-common-model, unified-tiering-03-zfs-raw-backend, unified-tiering-04a-fuse-daemon
**Part of:** unified-tiering-control-plane
**Preceded by:** unified-tiering-04a-fuse-daemon
**Followed by:** unified-tiering-05-mixed-backend-ui

---

## Context

This proposal is P04B, one half of a split from the original P04 (Managed ZFS Adapter). The original proposal was rated XL and has been divided into two bounded pieces:

- **P04A**: the C FUSE daemon, Unix socket protocol, kernel passthrough and fallback, fd validation, directory protocol, bypass detection, and daemon lifecycle supervision.
- **P04B (this file)**: the Go `TieringAdapter` implementation, adapter-owned metadata dataset schema, normative backing dataset layout, movement workers, synchronous recall, crash recovery, and I/O throttling.

P04B depends on P04A: the adapter cannot resolve placements, serve recalls, or move files until the daemon IPC contract is stable and the socket listener is running.

---

## Problem

The FUSE daemon infrastructure from P04A provides a controlled namespace service and a stable IPC contract. What it does not provide is any placement logic, file movement, recall coordination, or policy enforcement — those responsibilities belong to the Go layer.

This proposal wires the `TieringAdapter` implementation on top of the P04A daemon: it resolves placement decisions, maintains adapter-owned metadata in a co-located SQLite database, schedules and executes background file movement with copy/verify/switch/cleanup semantics, enforces fill-policy thresholds at file-creation time, implements synchronous recall-on-access with a configurable timeout, and defines crash recovery for interrupted movement jobs.

---

## Goals

1. Implement `TieringAdapter` for the managed ZFS backend.
2. Define the adapter-owned metadata dataset schema.
3. Define the normative backing dataset layout.
4. Surface managed ZFS tier targets and namespaces in the unified control-plane API.
5. Implement background copy/verify/switch/cleanup workers for file-level movement.
6. Enforce fill and threshold policy at file-placement time.
7. Implement synchronous recall-on-access with timeout.
8. Define crash recovery for interrupted movement jobs.
9. Define movement I/O throttling.

---

## Non-goals

- The C FUSE daemon itself (P04A).
- The Unix socket protocol between daemon and tierd (P04A).
- Daemon lifecycle supervision and fanotify bypass detection (P04A).
- Coordinated namespace snapshots (P06).
- The mixed-backend GUI (P05).

---

## Adapter Mapping

### TierTarget

Each ZFS backing tier dataset maps to one `TierTarget` in the unified control-plane model:

| Field | Value |
|-------|-------|
| `PlacementDomain` | namespace name |
| `Rank` | tier rank (1 = fastest) |
| `BackendKind` | `"zfs-managed"` |
| `MovementGranularity` | `"file"` |
| `PinScope` | `"namespace"` |
| `SupportsOnlineMove` | `false` (copy-based, not online rename) |
| `SupportsRecall` | `true` |
| `RecallMode` | `"synchronous"` |
| `SnapshotMode` | `"none"` until P06 ships |
| `SupportsCompression` | `true` |
| `FUSEMode` | `"passthrough"` or `"fallback"` from P04A capability report |

### ManagedNamespace

Each managed ZFS namespace maps to one `ManagedNamespace`:

| Field | Value |
|-------|-------|
| `NamespaceKind` | `"filespace"` |
| `ExposedPath` | `/mnt/tiering/<namespace>` |
| `BackendKind` | `"zfs-managed"` |

---

## ActivityBand Derivation

Access frequency counters are maintained per file by the FUSE namespace service: the daemon increments an in-memory counter on each `open()` for a given namespace object key. tierd reads those counters by calling `CollectActivity()` on each cycle.

Band thresholds:

| Band | Condition |
|------|-----------|
| `hot` | accessed more than once per hour |
| `warm` | accessed at least once per day |
| `cold` | accessed at least once per week |
| `idle` | not accessed within the most recent collection window |

Per-namespace band is the mode across all files in the namespace; in case of a tie, the hottest band wins.

---

## Metadata Dataset Schema

The adapter owns a dedicated ZFS dataset `pool/tiering_meta`. This is a dataset within the same pool as the backing tier datasets — it is not a separate pool. The adapter mounts it and maintains a SQLite database at `<tiering_meta_mountpoint>/meta.db`.

This database is the authoritative source of truth for placement state, movement progress, and crash recovery. Its format is normative: bypass detection, crash recovery, and coordinated snapshots (P06) all depend on it. Schema changes require a migration version bump.

### Table: `objects`

| Column | Type | Notes |
|--------|------|-------|
| `id` | UUID (TEXT) | Primary key |
| `namespace_id` | TEXT | Foreign key to namespace record |
| `object_key` | TEXT | Relative path within the FUSE namespace |
| `current_target_id` | TEXT | Which backing dataset currently holds the authoritative copy |
| `intended_target_id` | TEXT | Where the placement engine intends the file to live |
| `placement_state` | TEXT | `placed` \| `moving` \| `stale` |
| `size_bytes` | INTEGER | Last observed file size |
| `access_count` | INTEGER | Cumulative open count; incremented by daemon on each open |
| `last_accessed_at` | INTEGER | Unix timestamp of most recent open |
| `content_hash` | TEXT | SHA-256 of file content; updated after each successful movement copy+verify |
| `updated_at` | INTEGER | Unix timestamp of last row modification |

Indexes: `(namespace_id, object_key)` unique; `(namespace_id, current_target_id)`; `(placement_state, intended_target_id)`.

### Table: `movement_log`

| Column | Type | Notes |
|--------|------|-------|
| `id` | UUID (TEXT) | Primary key |
| `object_id` | TEXT | Foreign key to `objects.id` |
| `source_target_id` | TEXT | Backing dataset the file is being moved from |
| `dest_target_id` | TEXT | Backing dataset the file is being moved to |
| `state` | TEXT | `copy_in_progress` \| `copy_complete` \| `switched` \| `cleanup_complete` |
| `started_at` | INTEGER | Unix timestamp |
| `updated_at` | INTEGER | Unix timestamp of last state transition |

`movement_log` is the crash-recovery source of truth. Rows are never deleted during normal operation; they are only marked terminal (`cleanup_complete` or `failed`). The `state` column transitions are append-like: each transition updates `state` and `updated_at` atomically within a SQLite transaction.

This database is snapshotted as part of coordinated namespace snapshots (P06). Its contents must be consistent with the backing dataset state at the moment of snapshot.

---

## Normative Backing Dataset Layout

```text
pool/tiering_fast   ← rank 1 (hot tier)
pool/tiering_warm   ← rank 2 (warm tier)
pool/tiering_cold   ← rank 3 (cold tier)
pool/tiering_meta   ← adapter metadata (SQLite at <mountpoint>/meta.db)

exposed namespace:  /mnt/tiering/<namespace>
```

All backing tier datasets and the metadata dataset must reside within the same ZFS pool. This constraint is required for coordinated atomic snapshots in P06 (`zfs snapshot pool@name` is atomic across all datasets in the pool). At namespace creation time, the adapter verifies that all specified backing datasets share the same pool. If any dataset spans a different pool, `CreateNamespace` returns an error and no namespace is created.

Tier ranks are assigned at namespace creation time from the ordered list of backing datasets supplied by the operator. Rank 1 is the fastest (hottest) tier. The adapter enforces that ranks are contiguous and start at 1; gaps are rejected.

---

## Write-Path Behavior

New files land on the highest-ranked (fastest) writable target whose `used_pct` is below `full_threshold_pct`. The FUSE namespace enforces this at file-creation time: when the daemon receives an `OPEN_REQUEST` with `O_CREAT`, it blocks on the IPC call and tierd selects the placement target before returning a backing fd.

Fill threshold semantics:

| Condition | Behavior |
|-----------|----------|
| `used_pct` below `target_fill_pct` | New files land on this target normally. |
| `used_pct` between `target_fill_pct` and `full_threshold_pct` | Background policy schedules outbound movement to the next-lower-ranked target. Writes continue landing on this target in the meantime. |
| `used_pct` above `full_threshold_pct` | The adapter stops placing new files on this target and promotes the next-lower-ranked writable target instead. If no lower-ranked writable target exists, the adapter reports `no_drain_target` degraded state. |

**In-place growth**: files that grow after initial placement via append or truncate do not trigger placement re-evaluation at write time. The `size_bytes` column in `objects` and per-target `used_bytes` accounting are updated on each `CollectActivity()` cycle. Fill threshold enforcement for growing files therefore lags by up to one evaluation interval. Operators should set `full_threshold_pct` at least 10 percentage points below physical dataset capacity to absorb this lag.

---

## Read-Path and Recall

After `open()` returns a passthrough fd (via P04A), reads go directly to the backing dataset through the kernel — no userspace hop.

### Recall on Cold-File Open

When a cold file is opened and recall is enabled:

1. The daemon sends `OPEN_REQUEST` to tierd; tierd identifies the object as cold (current target rank > 1 and `placement_state = placed`).
2. tierd sets a `recall_pending` flag on the `objects` row atomically and enqueues a recall job to the movement worker pool.
3. tierd blocks the `OPEN_RESPONSE` reply until the recall movement completes.
4. On success, tierd updates `objects.current_target_id` to the fast tier, clears `recall_pending`, and returns the fast-tier backing fd to the daemon.
5. The daemon registers the fd as a passthrough target; subsequent reads go directly to the fast-tier dataset.

### Recall Timeout

Recall timeout is controlled by `recall_timeout_seconds` in `control_plane_config` (default 300 seconds). A value of 0 disables the timeout (not recommended for production).

If recall does not complete within the timeout:

1. The recall is aborted; the movement worker cleans up any partial promoted copy on the fast tier.
2. The `open()` call returns `EIO` to the application.
3. tierd reports `recall_timeout` degraded state for the affected namespace.
4. The `objects` row is left with `placement_state = placed` on the original cold target. The file remains readable through subsequent opens, which will trigger recall again.

### Recall During Active Movement

If a movement worker is mid-copy for a given object when a recall is triggered for that same object:

1. tierd sets the `recall_pending` flag atomically on the `objects` row.
2. The movement worker detects `recall_pending` before writing each copy chunk (polling the flag at the start of each read-write iteration).
3. The worker aborts the copy and deletes any partial destination file.
4. The movement log row for the interrupted movement is marked `failed` with reason `interrupted_by_recall`.
5. Recall proceeds against the source (still-authoritative) backing file.
6. After recall completes, the policy engine may re-queue the object for movement on the next evaluation cycle.

---

## Movement Behavior

Managed ZFS movement is copy-based and proceeds in four phases:

1. **Select**: the placement engine selects candidate objects by activity band and the gap between `current_target_id` and `intended_target_id`.
2. **Copy**: the movement worker reads the source file in chunks and writes to the destination backing dataset. The worker runs with `ionice -c 3` (idle scheduling class) and respects the I/O throttle (see below).
3. **Verify**: after copy completes, the worker computes SHA-256 of the destination file and compares it to the stored `content_hash` (or computes the hash from the source if no prior hash is stored). A mismatch fails the movement job with `verify_failed`.
4. **Switch**: the worker updates `objects.current_target_id` to the destination target and inserts a `movement_log` row with `state = switched` in a single SQLite transaction.
5. **Cleanup**: the worker deletes the source copy and updates the `movement_log` row to `state = cleanup_complete`.

Movement plans carry the `policy_revision` and `intent_revision` under which they were created. The adapter rejects or replans stale movements on any start, resume, or reconciliation where revisions no longer match. A stale movement is marked `failed` with reason `intent_revision_mismatch` and the `objects.placement_state` is set to `stale` to trigger replanning.

### Movement I/O Throttling

Movement copy workers are started with `ionice -c 3` (idle I/O scheduling class) to yield to user-facing I/O automatically at the kernel scheduler level. In addition, before each copy chunk, the adapter checks `control_plane_config.migration_io_high_water_pct`. If average device utilization — sampled via `iostat -x` over a one-second window — exceeds this threshold on any device backing the source or destination dataset, the worker sleeps for one poll interval (default 5 seconds) before retrying the chunk.

Maximum concurrent movement workers per adapter instance: `movement_worker_concurrency` from `control_plane_config` (default 4). Workers beyond this limit queue behind a semaphore; they do not start until a running worker finishes or is cancelled.

---

## Movement Crash Recovery

On adapter startup, `Reconcile()` scans the `movement_log` table for rows in non-terminal states (`copy_in_progress`, `copy_complete`, `switched`) and applies the following recovery procedure. Recovery runs before the adapter begins serving any placement requests or starting movement workers.

### State: `copy_in_progress`

The copy was interrupted before completing. The destination file may be absent or partial.

Recovery:
1. Delete any partial destination file (if present, identified by path derived from `dest_target_id` and `object_key`).
2. Mark the `movement_log` row `failed` with reason `interrupted_by_restart`.
3. Verify `objects.current_target_id` still points to the source; if not, see Ambiguous State below.
4. Set `objects.placement_state` to `placed`.

The source file is authoritative. The policy engine may re-queue the object on the next cycle.

### State: `copy_complete`

The copy completed and the destination file exists with a matching content hash, but `objects.current_target_id` still points to the source (the switch transaction did not commit).

Recovery:
1. Delete the destination copy. The source is preferred as ground truth under ambiguity.
2. Mark the `movement_log` row `failed` with reason `interrupted_before_switch`.
3. Set `objects.placement_state` to `placed` (source remains authoritative).

The policy engine may re-queue the object on the next cycle.

### State: `switched`

The switch transaction committed: `objects.current_target_id` already points to the destination. The source copy has not yet been deleted.

Recovery:
1. Mark the `movement_log` row `done`.
2. Schedule deletion of the source copy as a background cleanup task (does not block `Reconcile()` completion).

The destination file is authoritative.

### Ambiguous State

If the metadata database is internally inconsistent (e.g., `objects.current_target_id` points to a dataset where the file does not exist, or a content hash mismatch is detected between a supposedly complete destination copy and its stored hash), the adapter:

1. Reports `reconciliation_required` critical degraded state for the affected namespace.
2. Disables all movement and recall for that namespace.
3. Continues serving reads from whatever backing file is accessible (best-effort).

The namespace remains in `reconciliation_required` until an operator explicitly triggers a reconcile operation via the API, which re-inspects backing dataset contents and rebuilds affected rows.

---

## ZFS-Native Feature Preservation

| Feature | Behavior through the adapter |
|---------|------------------------------|
| Checksums | Backing-dataset SHA-256 checksums are transparent through the FUSE namespace. The adapter's own content hash (stored in `objects.content_hash`) is an independent application-level hash used for movement verification, not a replacement for ZFS checksumming. |
| Compression | Configurable on backing datasets independently. The adapter reports `SupportsCompression = true`. |
| SLOG and L2ARC | Pool-level features; below the adapter. Not surfaced in the control-plane model. |
| ARC sizing | System-level tuning concern; not surfaced in the control-plane model. |
| Snapshots | `SnapshotMode = "none"` until P06 ships. Per-dataset ZFS snapshots are not exposed through the managed namespace because they do not cover the full logical namespace. |

---

## Degraded States (P04B scope)

| Code | Condition |
|------|-----------|
| `no_drain_target` | A target is above `full_threshold_pct` and no lower-ranked writable target exists for new file placement |
| `movement_failed` | A copy, verify, switch, or cleanup worker exited with an error |
| `reconciliation_required` | Adapter metadata is inconsistent on startup; movement and recall are disabled for the affected namespace until an operator clears the state |
| `placement_intent_stale` | An intent revision mismatch was detected; affected movement jobs have been cancelled |
| `recall_timeout` | A synchronous recall did not complete within `recall_timeout_seconds`; the triggering `open()` returned EIO |

The states `namespace_unavailable`, `fd_pass_failed`, `bypass_detected`, and `bypass_detection_unavailable` are defined in P04A.

---

## Effort

**XL** — the Go adapter is a novel subsystem: metadata schema and migration harness, placement engine (fill-policy enforcement, band derivation), copy/verify/switch/cleanup movement workers, I/O throttling via `iostat`, synchronous recall with timeout and cancellation, crash recovery across three distinct interrupted states, and integration with the P04A IPC contract. The C daemon (P04A) is separate work. Treat P04B as a multi-sprint deliverable independent of P04A after the IPC contract stabilises.

---

## Acceptance Criteria

- [ ] The managed ZFS `TieringAdapter` implementation satisfies the `TieringAdapter` interface defined in proposal 01; all required methods are implemented and type-check against the interface.
- [ ] The adapter-owned metadata dataset `pool/tiering_meta` exists after `CreateNamespace`; `meta.db` is present at its mountpoint and contains the `objects` and `movement_log` tables with the schema defined in this document.
- [ ] At `CreateNamespace` time, the adapter verifies all backing datasets share the same ZFS pool; namespaces spanning multiple pools are rejected with a descriptive error.
- [ ] Tier ranks are contiguous starting at 1; a gap in ranks is rejected at namespace creation time.
- [ ] Managed ZFS targets appear in `/api/tiering/targets` with `BackendKind = "zfs-managed"`, `MovementGranularity = "file"`, `SupportsRecall = true`, `RecallMode = "synchronous"`, `SupportsCompression = true`, and `SnapshotMode = "none"`.
- [ ] Managed ZFS namespaces appear in `/api/tiering/namespaces` with `NamespaceKind = "filespace"` and `ExposedPath = /mnt/tiering/<namespace>`.
- [ ] `FUSEMode` on each target reflects the active P04A capability report (`"passthrough"` or `"fallback"`).
- [ ] New files created through the namespace land on the highest-ranked writable target whose `used_pct` is below `full_threshold_pct`.
- [ ] When a target's `used_pct` is above `full_threshold_pct` and no lower-ranked writable target exists, `no_drain_target` degraded state is reported.
- [ ] Opening a cold file triggers synchronous recall; the `open()` call does not return to the application until recall is complete and the fast-tier fd is ready.
- [ ] A recall that exceeds `recall_timeout_seconds` causes `open()` to return `EIO` and sets `recall_timeout` degraded state for the namespace.
- [ ] When a movement worker is copying an object and a recall for that same object is triggered, the worker detects `recall_pending` and aborts; the movement log row is marked `failed` with reason `interrupted_by_recall`; the recall proceeds against the source.
- [ ] Crash recovery for `copy_in_progress`: any partial destination file is deleted; the log row is marked `failed` with reason `interrupted_by_restart`; the source remains authoritative.
- [ ] Crash recovery for `copy_complete`: the destination copy is deleted; the log row is marked `failed` with reason `interrupted_before_switch`; the source remains authoritative.
- [ ] Crash recovery for `switched`: the log row is marked `done`; the source copy is scheduled for background deletion; the destination is authoritative.
- [ ] Ambiguous state (metadata inconsistency or hash mismatch) reports `reconciliation_required` and disables movement for the affected namespace.
- [ ] Movement copy workers run under `ionice -c 3`.
- [ ] When device utilization (sampled via `iostat`) exceeds `migration_io_high_water_pct`, movement workers sleep for one poll interval before retrying.
- [ ] Maximum concurrent movement workers per adapter is bounded by `movement_worker_concurrency` (default 4); workers beyond this limit queue.
- [ ] `SupportsCompression = true` is reported on all managed ZFS targets.
- [ ] `SnapshotMode = "none"` is reported on all managed ZFS targets until P06 ships.
- [ ] `objects.size_bytes` and per-target `used_bytes` accounting are updated on each `CollectActivity()` cycle, not only at file-creation time.
- [ ] `content_hash` (SHA-256) in the `objects` table is updated after each successful movement copy and verify step.
- [ ] Movement plans carrying a stale `intent_revision` are rejected; affected jobs are marked `failed` with reason `intent_revision_mismatch` and `objects.placement_state` is set to `stale`.
- [ ] `Reconcile()` completes crash recovery before the adapter begins serving placement requests or starting movement workers.
- [ ] `placement_intent_stale` and `movement_failed` degraded states are emitted under the conditions described in the Degraded States section.

---

## Test Plan

### Integration Tests

- [ ] Integration test: `CreateNamespace` with backing datasets from two different pools returns an error; no metadata dataset or namespace record is created.
- [ ] Integration test: `CreateNamespace` with a rank gap (e.g., ranks 1, 3) returns an error.
- [ ] Integration test: `CreateNamespace` with valid backing datasets creates `pool/tiering_meta`, mounts it, and initialises `meta.db` with the `objects` and `movement_log` tables.
- [ ] Integration test: managed ZFS targets appear in `/api/tiering/targets` with all specified fields; managed namespace appears in `/api/tiering/namespaces`.
- [ ] Integration test: file written to the namespace via `O_CREAT` lands on the rank-1 target when rank-1 `used_pct` is below `full_threshold_pct`; `objects` row is inserted with `current_target_id` pointing to rank-1.
- [ ] Integration test: rank-1 target is above `full_threshold_pct`; new file is placed on rank-2 target instead; `objects` row reflects rank-2 placement.
- [ ] Integration test: all targets are above `full_threshold_pct`; `no_drain_target` degraded state is reported.
- [ ] Integration test: `CollectActivity()` updates `objects.size_bytes` and per-target `used_bytes` for a file that grew in place since the last cycle.
- [ ] Integration test: cold file `open()` blocks until recall completes; timing confirms the call did not return before the movement worker finished; returned fd is on rank-1.
- [ ] Integration test: recall timeout — `recall_timeout_seconds` set to 1 second; a recall that cannot complete in time returns `EIO`; `recall_timeout` degraded state is set; file remains on cold tier.
- [ ] Integration test: simultaneous recall and movement for the same object — movement worker aborts; movement log row has reason `interrupted_by_recall`; recall succeeds from source.
- [ ] Integration test: movement worker completes full copy/verify/switch/cleanup cycle; `objects.current_target_id` updated; source copy deleted; `movement_log` row in `cleanup_complete` state; `objects.content_hash` updated.
- [ ] Integration test: movement worker encounters a verify failure (corrupt destination copy injected); job marked `failed` with reason `verify_failed`; `movement_failed` degraded state reported; source copy is not deleted.
- [ ] Integration test: movement job with a stale `intent_revision` is rejected; `movement_failed` reason is `intent_revision_mismatch`; `placement_intent_stale` degraded state reported.
- [ ] Integration test: device utilization injected above `migration_io_high_water_pct`; movement worker pauses for one poll interval; copies resume when utilization drops.
- [ ] Integration test: `movement_worker_concurrency` limit is respected — more than `movement_worker_concurrency` concurrent movement jobs are queued; only the configured maximum run simultaneously.

### Unit Tests: Crash Recovery

- [ ] Unit test: `Reconcile()` with a `movement_log` row in `copy_in_progress` and no destination file — deletes nothing (nothing to delete), marks row `failed`, verifies `objects.placement_state = placed`.
- [ ] Unit test: `Reconcile()` with a `movement_log` row in `copy_in_progress` and a partial destination file — deletes the partial file, marks row `failed` with `interrupted_by_restart`, verifies source remains authoritative.
- [ ] Unit test: `Reconcile()` with a `movement_log` row in `copy_complete` and a matching destination file — deletes the destination copy, marks row `failed` with `interrupted_before_switch`, verifies `objects.current_target_id` still points to source.
- [ ] Unit test: `Reconcile()` with a `movement_log` row in `switched` and the source copy still present — marks row `done`, schedules background source deletion, verifies `objects.current_target_id` points to destination.
- [ ] Unit test: `Reconcile()` with `objects.current_target_id` pointing to a dataset where the file does not exist — `reconciliation_required` degraded state set; movement disabled for namespace; subsequent `CreateMovementJob` calls return an error.
- [ ] Unit test: `Reconcile()` completes successfully with no non-terminal `movement_log` rows — no degraded states set; movement workers start normally.

### Unit Tests: I/O Throttling

- [ ] Unit test: simulated `iostat` output above threshold — throttle logic returns `true` (sleep required) before the next copy chunk.
- [ ] Unit test: simulated `iostat` output below threshold — throttle logic returns `false`; no sleep injected.
- [ ] Unit test: `migration_io_high_water_pct = 0` — throttle is effectively disabled; all chunks proceed without sleep.
- [ ] Unit test: `ionice -c 3` is applied to the movement worker goroutine's OS thread (verified by inspecting the goroutine's I/O scheduling class via `/proc/<pid>/task/<tid>/io` or equivalent).

### Benchmarks

- [ ] Benchmark: end-to-end throughput of a movement job (copy+verify) on a 1 GiB file; baseline is direct `cp` between the same two datasets; adapter overhead must not exceed 10%.
- [ ] Benchmark: recall latency for a 100 MiB cold file against a warm-tier SSD dataset; p50 and p99 reported.
