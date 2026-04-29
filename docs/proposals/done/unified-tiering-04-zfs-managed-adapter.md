# Proposal: Unified Tiering — 04: Managed ZFS Adapter [SUPERSEDED]

**Status:** Superseded
**Superseded by:** unified-tiering-04a-fuse-daemon, unified-tiering-04b-zfs-managed-adapter

This proposal has been split. Do not implement from this file.
- P04A covers: C FUSE daemon, Unix socket protocol, passthrough, bypass detection.
- P04B covers: TieringAdapter implementation, metadata dataset schema, movement workers, recall.

---

---

## Problem

Raw ZFS datasets (proposal 03) are a first-class storage path but are not part of a placement or tiering workflow. To participate in the unified tiering GUI, ZFS needs a managed layer that provides placement intent, ranked tier targets, background movement, and a controlled namespace — without pretending raw datasets are interchangeable with region-tiered mdadm volumes.

This proposal defines that managed layer and the C FUSE daemon that implements it.

---

## Goals

1. Implement `TieringAdapter` for the managed ZFS backend.
2. Provide a C FUSE daemon using libfuse3 with passthrough fd support as the namespace service.
3. Route placement decisions through tierd; isolate all data I/O through the FUSE namespace.
4. Implement background copy/verify/switch/cleanup workers for file-level movement.
5. Enforce fill and threshold policy at file-placement time through the FUSE namespace.
6. Implement synchronous recall-on-access.
7. Prevent and detect namespace bypass.

---

## Non-goals

- Asynchronous recall-on-access (deferred to a later phase).
- Coordinated namespace snapshots (covered by proposal 05).
- Changes to the raw ZFS backend (proposal 03).
- The mixed-backend GUI (proposal 05).

---

## Why a Separate C Daemon, Not go-fuse

Two alternatives were evaluated and rejected:

- **go-fuse inside tierd**: go-fuse does not support libfuse3's passthrough fd API. Without passthrough, every read and write round-trips through userspace — a 10–30% throughput penalty for sequential I/O. Unacceptable for a NAS appliance.
- **CGo wrapper around libfuse3 inside tierd**: libfuse3 uses pthreads internally. CGo forces Go goroutines onto OS threads for every C call, which conflicts with Go's scheduler under the threading model libfuse3 expects. CGo call overhead also applies on every FUSE callback, partially negating any passthrough benefit.

A separate C daemon avoids both problems. The daemon is a thin relay (~500–1,000 lines of C). It contains no placement logic.

---

## Architecture

```text
application / NFS / SMB
  → kernel FUSE driver
  → C FUSE daemon (libfuse3)
      on open():    Unix socket query → tierd → backing fd returned to kernel
      on read()/write(): kernel passthrough directly to backing fd (no userspace hop)
      on metadata:  Unix socket query → tierd (or local cache hit)
      on release(): notify tierd fd is closed
  → tierd (Go)
      authoritative placement metadata
      placement decisions
      policy reconciliation
      movement scheduling
```

Placement decisions happen at `open()` time only. After the C daemon registers a passthrough fd with the kernel, subsequent reads and writes on that fd bypass userspace entirely.

---

## Unix Socket Protocol

The C FUSE daemon and tierd communicate over a Unix domain socket at a fixed path: `/run/tierd/fuse-<namespace-id>.sock`. tierd creates the socket file and listens before starting the daemon. The daemon connects on startup; if the connection fails, the daemon exits.

### Message framing

All messages use a fixed 8-byte header:

| Bytes | Field |
| --- | --- |
| 0–3 | Message type (uint32 little-endian) |
| 4–7 | Payload length in bytes (uint32 little-endian) |

Followed by `payload_length` bytes of payload. Maximum payload size is 65536 bytes. Messages exceeding this limit are rejected with a protocol error and the connection is closed.

### Message types

| Type | Direction | Description |
| --- | --- | --- |
| `OPEN_REQUEST` (1) | daemon → tierd | Request backing fd for a namespace object |
| `OPEN_RESPONSE` (2) | tierd → daemon | Backing fd (via `SCM_RIGHTS`) + placement metadata |
| `RELEASE_NOTIFY` (3) | daemon → tierd | Notify tierd that an fd has been closed |
| `HEALTH_PING` (4) | tierd → daemon | Liveness check |
| `HEALTH_PONG` (5) | daemon → tierd | Liveness response |
| `ERROR` (6) | either direction | Protocol error; connection should be closed |

`OPEN_REQUEST` payload: null-terminated namespace object key (UTF-8, max 4096 bytes).
`OPEN_RESPONSE` payload: 1-byte result code (0 = success, non-zero = error), followed by 8 bytes of inode number (uint64 little-endian). The backing fd is passed out-of-band via `SCM_RIGHTS` ancillary data on the same `sendmsg` call.

### Reconnect semantics

If tierd restarts while the daemon is running, the daemon detects the broken socket connection (EOF or error on the next send/recv) and exits. tierd then starts a new daemon instance and remounts.

If the daemon exits, tierd detects the exit via its process supervisor and reports `namespace_unavailable` degraded state. tierd does not need to close the socket; the OS reclaims it when the daemon process exits.

---

## C FUSE Daemon

### fd passing

tierd opens backing files and passes the open fds to the C daemon over the Unix socket using `SCM_RIGHTS` ancillary data. The C daemon never opens backing files itself and cannot bypass placement logic by choosing a different backing path. tierd is the sole arbiter of which backing fd maps to which namespace object.

The daemon must validate each received fd before registering it as a passthrough target: it must `fstat` the fd and confirm it refers to the expected inode on the expected backing dataset. If validation fails, the daemon returns `EIO` for the `open()` and tierd reports a `fd_pass_failed` degraded state.

### Kernel version detection

FUSE passthrough requires Linux 6.x (`FUSE_PASSTHROUGH` kernel capability). The daemon must detect support at mount time:

- if `FUSE_PASSTHROUGH` is available: register the backing fd with the kernel on each `open()` response
- if not: fall back to traditional read/write handlers that forward I/O through the daemon

The fallback path must be tested and must not be removed. The API reports the active FUSE mode (`passthrough` or `fallback`) as part of the managed namespace capability field.

### Daemon lifecycle

tierd is responsible for starting, supervising, and restarting the C FUSE daemon.

If the daemon exits unexpectedly:

1. The FUSE mount becomes unavailable; in-flight I/O receives errors.
2. tierd detects the exit, reports `namespace_unavailable` degraded state.
3. tierd attempts restart with exponential backoff.
4. tierd caps restart attempts within a window to prevent a crash loop.
5. Existing open fds held by applications become invalid after remount; applications must reopen.

tierd must not begin serving API traffic for a managed namespace until the daemon is confirmed live and the mount is healthy.

### Implementation order

Build in this order to keep the subsystem testable at each step:

1. **Daemon skeleton** — mount, unmount, passthrough fd registration, Unix socket server, `SCM_RIGHTS` fd passing; no placement logic yet; all opens return the single backing fd
2. **Placement routing** — `open()` queries tierd over the socket; tierd returns the correct backing fd for current placement; passthrough registered with kernel
3. **Daemon lifecycle supervision** — tierd starts, monitors, and restarts the daemon; `namespace_unavailable` degraded state on crash
4. **Kernel version detection and fallback** — detect `FUSE_PASSTHROUGH` at mount time; fall back to traditional handlers on older kernels
5. **Metadata store and dataset layout** — adapter-owned metadata dataset, backing dataset provisioning, `CreateTarget` / `CreateNamespace`
6. **Movement workers** — background copy, verify, switch, cleanup; movement jobs visible through the unified API
7. **Bypass protection** — dataset ownership (`tierd:tierd`, mode `700`), `bypass_detected` degraded-state reporting
8. **Synchronous recall-on-access** — block `open()` reply until recall completes; async recall is deferred

---

## Performance Characteristics

| Workload | Traditional FUSE (no passthrough) | libfuse3 + passthrough (Linux 6.x) |
| --- | --- | --- |
| Sequential read/write throughput | −10–30% vs bare metal | −2–5% vs bare metal |
| Per read/write latency | +5–20 μs | ~0 (kernel direct path) |
| Random small I/O (IOPS) | −20–50% vs bare metal | −5–15% vs bare metal |
| Per `open()` latency | +5–20 μs | +6–25 μs (FUSE round-trip + IPC to tierd) |

The `open()` overhead is the residual cost with passthrough. For the NAS workloads this adapter targets (large sequential reads and writes, infrequent file opens), this is acceptable. For workloads that open thousands of small files per second, the per-open IPC cost is noticeable and operators should use raw ZFS datasets instead.

NFS and SMB exports through the FUSE namespace benefit from passthrough: the data path is NFS/SMB kernel handler → FUSE passthrough → backing dataset, with no userspace hop on read/write.

The fallback path (kernels without passthrough) retains the traditional FUSE overhead numbers and must be documented as a known performance limitation.

---

## Namespace Implementation

### Bypass prevention

Backing datasets must be inaccessible to operators through normal ZFS paths:

- backing datasets are owned by the `tierd` system user (`tierd:tierd`, mode `700`)
- `zfs allow` must not grant dataset-level access to operator accounts for adapter-owned datasets
- backing dataset mountpoints, if any, must not be under `/mnt` paths exposed to users
- tierd detects bypass attempts by installing a `fanotify` watch on the backing dataset mountpoint. Any `open()` or `create()` event on the backing mountpoint that originates from a process other than the FUSE daemon (identified by PID) is treated as a bypass attempt. tierd reports `bypass_detected` degraded state and logs the offending path and PID.

Note: `atime`/`mtime`/`ctime` comparison is not used for bypass detection — it produces false positives under `relatime` and misses reads entirely under `noatime`. The `fanotify` approach catches access at the VFS layer before data is served.

On systems where `fanotify` is unavailable (kernels older than 2.6.37), bypass detection falls back to periodic `fstat` comparison with degraded-state `bypass_detection_unavailable`.

### Backing Layout (normative)

The adapter uses the following backing layout. This layout is normative — bypass detection, fd passing, and crash recovery all depend on it. Adapters must not deviate.

- one backing dataset per ranked target
- one adapter-owned metadata dataset
- one exposed FUSE mount path per managed namespace

```text
pool_fast/tiering_fast
pool_warm/tiering_warm
pool_cold/tiering_cold
pool_meta/tiering_meta
exposed namespace: /mnt/tiering/<namespace>
```

Backing datasets must not be the supported user entry point. The exposed FUSE path is authoritative.

---

## Write-Path Behavior

New files land on the highest-ranked writable target below `full_threshold_pct`. The FUSE namespace service enforces placement at file-creation time.

- Below `target_fill_pct`: new files land on the target normally.
- Between `target_fill_pct` and `full_threshold_pct`: background policy schedules outbound movement to the next-lower-ranked target. Writes continue landing on the over-full target.
- Above `full_threshold_pct`: the FUSE service stops placing new files on that target and promotes the next-lower-ranked writable target. If none exists, `no_drain_target` degraded state is reported.

**In-place growth**: files that grow after initial placement via append or truncate do not trigger a placement re-evaluation at write time. The FUSE namespace service updates the per-target `used_bytes` accounting asynchronously (on each `CollectActivity()` cycle). Fill threshold enforcement for growing files therefore lags by up to one evaluation interval. Operators should set `full_threshold_pct` conservatively (at least 10 percentage points below physical capacity) to absorb this lag.

---

## Read-Path and Recall Behavior

Reads continue through the FUSE namespace. After `open()` returns a passthrough fd, subsequent reads go directly to the backing dataset through the kernel — no userspace hop.

When synchronous recall-on-access is enabled, `open()` for a cold object:

1. The C daemon queries tierd; tierd identifies the object as cold.
2. tierd issues a recall request to the movement worker.
3. tierd blocks the `open()` reply until recall completes.
4. tierd returns the fast-tier backing fd; the daemon registers it as a passthrough target.
5. Subsequent reads on the open fd go directly to the fast-tier dataset.

The adapter must advertise recall mode as `synchronous`. The control plane must not assume asynchronous or kernel-native relocation semantics.

tierd enforces a configurable maximum recall duration (`recall_timeout_seconds`, default 300). If recall does not complete within this timeout:
1. The recall is aborted; the movement worker cleans up any partial promoted copy.
2. The `open()` call returns `EIO`.
3. A `recall_timeout` degraded state is reported for the affected namespace.

The timeout is configurable in `tier_policy_config`. A timeout of 0 disables the limit (not recommended for production).

**Recall during active movement**: if a movement worker is copying an object when an `open()` triggers synchronous recall for the same object, the recall request takes precedence. The movement worker detects the competing recall (via an adapter-internal flag set atomically on recall initiation), aborts the copy, and cleans up the partial destination copy. The recall then proceeds against the source (still-authoritative) backing file. After recall completes, the policy engine may re-queue the object for movement on the next evaluation cycle.

---

## Movement Behavior

Managed ZFS movement is:

1. Select candidate files by activity band and placement intent.
2. Copy to the destination dataset.
3. Verify content and metadata.
4. Switch the authoritative location in adapter metadata.
5. Clean up the old copy.

Movement plans are valid only for the `placement_domain`, `policy_revision`, and `intent_revision` under which they were created. The adapter must reject or replan stale movements on start, resume, or reconciliation.

### Movement Crash Recovery

On adapter startup (called from `Reconcile()`), the adapter scans movement jobs in state `running` and performs the following recovery steps:

1. **Copy incomplete** (no destination file, or destination file size < source size): delete any partial destination copy; mark the movement job `failed` with reason `interrupted_by_restart`; leave the source authoritative.
2. **Copy complete, switch not performed** (destination file exists with matching content hash, but adapter metadata still points to source): delete the destination copy; mark the movement job `failed` with reason `interrupted_before_switch`; leave the source authoritative. The policy engine may re-queue the object.
3. **Switch performed** (adapter metadata points to destination): mark the movement job `done`; schedule cleanup of the source copy.

Recovery state is determined by comparing the adapter metadata dataset against the backing dataset contents. Ambiguous state (e.g., corrupted metadata) reports `reconciliation_required` degraded state and disables movement for the affected namespace until resolved.

### Movement I/O Throttling

Movement copy workers are started with `ionice -c 3` (idle scheduling class) to yield to user-facing I/O. Additionally, the adapter reads `tier_policy_config.migration_io_high_water_pct` before dequeuing each movement job. If average device utilization (sampled via `iostat`) exceeds this threshold, the worker sleeps for one poll interval before retrying. This mirrors the throttling behaviour of the mdadm migration engine.

---

## ZFS-Native Feature Preservation

- Checksumming remains a backing-dataset capability, transparent through the FUSE namespace.
- Compression remains configurable on backing datasets.
- Snapshots: per-dataset snapshots exist but do not cover the full logical namespace. Coordinated namespace snapshots are deferred to proposal 05.
- SLOG and L2ARC remain pool-level accelerators below the adapter.
- ARC sizing remains a system-level tuning concern.

---

## Snapshot Semantics

The adapter must not advertise `coordinated-namespace` snapshot support until a multi-dataset plus metadata-consistent snapshot operation is implemented (proposal 05). Until then, `SnapshotMode` is `none` for managed ZFS namespaces.

---

## Degraded States

| Code | Condition |
| --- | --- |
| `namespace_unavailable` | C FUSE daemon has exited; mount is inaccessible |
| `fd_pass_failed` | Received fd failed inode validation; open returned EIO |
| `bypass_detected` | Backing dataset modified outside the FUSE namespace |
| `no_drain_target` | Target above `full_threshold_pct` with no colder writable target |
| `movement_failed` | Copy/verify/switch/cleanup worker failed |
| `reconciliation_required` | Adapter state diverged from control-plane intent on startup |
| `placement_intent_stale` | Intent revision mismatch detected |

---

## Effort

**XL** — novel subsystem. Unix socket protocol, C FUSE daemon, fd passing, daemon supervision, movement workers, bypass detection, synchronous recall, I/O throttling, crash recovery, and the `fanotify` watch are all new. Treat this as a multi-sprint effort.

---

## Acceptance Criteria

- [ ] Managed ZFS `TieringAdapter` implementation satisfies the interface from proposal 01.
- [ ] The namespace service is a C FUSE daemon using libfuse3, not go-fuse or a raw dataset mount.
- [ ] On Linux 6.x, the daemon registers a passthrough fd with the kernel on each `open()`; reads and writes do not pass through userspace.
- [ ] On kernels without `FUSE_PASSTHROUGH`, the daemon falls back to traditional read/write handlers without error.
- [ ] tierd passes backing fds to the daemon via `SCM_RIGHTS`; the daemon validates each fd before registering it.
- [ ] The API reports the active FUSE mode (`passthrough` or `fallback`) as a capability field.
- [ ] tierd supervises the daemon and reports `namespace_unavailable` on exit; it remounts after restart with backoff.
- [ ] Direct raw backing dataset access is technically restricted via dataset ownership and permissions.
- [ ] `bypass_detected` degraded state is emitted when a backing dataset is modified outside the namespace.
- [ ] Managed ZFS targets appear in `/api/tiering/targets` with correct capabilities.
- [ ] Managed ZFS namespaces appear in `/api/tiering/namespaces`.
- [ ] Movement status and placement summaries are visible through the unified API.
- [ ] Synchronous recall-on-access blocks `open()` until the file is on the fast tier.
- [ ] Sequential read/write throughput through the managed namespace is within 5% of raw ZFS on Linux 6.x with passthrough active.
- [ ] `SnapshotMode` is reported as `none` until proposal 05 ships.
- [ ] Unix socket uses the protocol defined in the Unix Socket Protocol section; message types and framing match the specification.
- [ ] Recall during active movement causes the movement worker to abort and the recall to proceed against the source.
- [ ] On startup, the adapter scans running movement jobs and applies the three-step crash recovery procedure.
- [ ] `fanotify` is used for bypass detection; falls back to `bypass_detection_unavailable` on older kernels.
- [ ] Movement copy workers run with `ionice -c 3` and respect `migration_io_high_water_pct`.
- [ ] Recall returns `EIO` and reports `recall_timeout` degraded state after `recall_timeout_seconds`.
- [ ] `used_bytes` accounting is updated on each `CollectActivity()` cycle (not only at file-creation time).

## Test Plan

- [ ] Integration test: daemon skeleton mounts, serves opens, and unmounts cleanly.
- [ ] Integration test: `SCM_RIGHTS` fd passing delivers a valid fd; daemon validates inode; passthrough is registered correctly.
- [ ] Integration test: daemon detects invalid fd from tierd, returns `EIO` on open, reports `fd_pass_failed`.
- [ ] Integration test: passthrough fallback on a kernel without `FUSE_PASSTHROUGH` — reads and writes complete correctly through the traditional handler.
- [ ] Integration test: C daemon crash causes `namespace_unavailable` degraded state; tierd remounts; subsequent opens succeed.
- [ ] Integration test: file written through the namespace lands on the correct backing dataset per placement intent.
- [ ] Integration test: movement worker runs copy/verify/switch/cleanup correctly; old copy is removed.
- [ ] Integration test: movement cancellation leaves the source object authoritative and cleans up partial copy.
- [ ] Integration test: synchronous recall blocks `open()` and returns the promoted fast-tier fd.
- [ ] Integration test: write to a backing dataset outside the namespace triggers `bypass_detected`.
- [ ] Throughput benchmark: sequential read and write through the namespace on Linux 6.x is within 5% of direct ZFS dataset access.
- [ ] Throughput benchmark: sequential read and write on the fallback path shows the expected −10–30% overhead.
- [ ] Integration test: `OPEN_REQUEST` / `OPEN_RESPONSE` / `RELEASE_NOTIFY` message round-trip completes correctly.
- [ ] Integration test: concurrent recall and movement worker for the same object — movement aborts, recall succeeds.
- [ ] Integration test: crash recovery correctly handles copy-incomplete, copy-complete-pre-switch, and switch-performed states.
- [ ] Integration test: direct open on backing dataset triggers `bypass_detected` via `fanotify`.
- [ ] Integration test: recall exceeding `recall_timeout_seconds` returns `EIO` and sets `recall_timeout` degraded state.
- [ ] Integration test: appending to a file does not immediately trigger fill enforcement; `used_bytes` updates on next `CollectActivity()`.
