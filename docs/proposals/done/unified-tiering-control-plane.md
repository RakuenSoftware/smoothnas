# Proposal: Unified Tiering Control Plane

**Status:** Done — design complete, split into work items
**Date:** 2026-04-09
**Depends on:** mdadm-complete-heat-engine
**Clarifies:** mdadm-tiering-policy
**Related to:** mdadm-tiering-infrastructure
**Implemented by:**
- unified-tiering-01-common-model
- unified-tiering-02-mdadm-adapter
- unified-tiering-03-zfs-raw-backend
- unified-tiering-04-zfs-managed-adapter
- unified-tiering-05-mixed-backend-ui

---

## Problem

SmoothNAS intentionally supports more than one storage path:

- mdadm arrays and LVM-backed named tiers
- ZFS pools, datasets, zvols, and snapshots

That is the right product direction, but it creates an operator problem around placement and tiering.

Today:

- mdadm/LVM is the only path with a concrete heat-tiering story
- ZFS is a first-class storage backend, but not part of a common tiering workflow
- the UI cannot present one coherent placement model across backends
- raw ZFS and mdadm/LVM do not expose equivalent migration primitives
- a shallow abstraction would mislead users into thinking they behave identically

SmoothNAS needs a control plane that unifies placement workflow, policy, and observability without inventing a fake shared storage engine.

---

## Goals

1. Provide one tiering control plane and one tiering GUI for placement-oriented workflows.
2. Keep the data plane backend-specific and honest.
3. Let mdadm/LVM heat-tiering participate immediately through a common model.
4. Define how ZFS participates in tiering without pretending raw datasets are the same thing as region-tiered volumes.
5. Fold raw ZFS backend management and managed ZFS tiering into one coherent proposal.
6. Normalize policy and operator workflow, not low-level migration semantics.
7. Surface backend capability differences clearly enough that the UI does not lie.
8. Keep the control plane off the steady-state read and write hot path.

---

## Non-goals

- Forcing raw ZFS pools or datasets into the mdadm heat engine
- Making all tiered backends expose the same movement granularity
- Defining one raw "heat score" that is comparable across all backends
- Replacing backend-native admin pages for arrays, pools, datasets, or snapshots
- Cross-host tiering or distributed storage orchestration
- A single storage engine shared by mdadm and ZFS
- Native block-level tiering inside a single ZFS pool

---

## Decision

SmoothNAS will implement a **unified tiering control plane with backend-specific data-plane adapters**.

The control plane owns:

- placement intent
- ranked target definitions
- target-fill policy
- normalized health and activity summaries
- movement scheduling and status
- pin requests
- operator-facing API and UI

The data plane owns:

- actual reads and writes
- actual block, file, or object placement
- backend-native heat collection details
- backend-native migration mechanics
- backend-native recovery and correctness checks

The control plane unifies operator workflow. It does not claim identical backend behavior.

---

## Product Principles

### 1. Unify workflow, not mechanism

Operators should configure rank, fill targets, and placement policy once, but the backend remains responsible for how data actually moves.

### 2. Do not compare unlike things as if they were identical

The control plane may normalize an mdadm region and a ZFS-managed file-space into one UI workflow, but it must not present their raw heat metrics, movement semantics, or recall behavior as equivalent.

### 3. Keep policy out of the hot path

Steady-state reads and writes must continue through backend-native paths. The control plane may influence placement asynchronously, but it must not introduce a per-I/O RPC or synchronous placement round-trip.

### 4. Preserve deep admin paths

Backend-specific pages remain first-class:

- Arrays, tiers, and volumes for mdadm/LVM
- Pools, datasets, zvols, and snapshots for ZFS

The unified tiering GUI sits above them for placement workflows. It does not replace them.

### 5. Do not admit raw ZFS to the unified tiering surface

Raw ZFS pools and datasets remain manageable through the ZFS UI, but they are not tier targets unless wrapped by a managed tiering adapter that satisfies the control-plane contract.

### 6. Rank is domain-scoped, not globally interchangeable

The control plane may show targets from multiple backends in one inventory, but rank only has strict scheduling meaning within a `placement_domain`.

The system must not assume that:

- `rank=1` on mdadm means the same latency class as `rank=1` on managed ZFS
- data may move automatically between arbitrary backends just because two targets are visible in one UI

Mixed-backend inventory is allowed. Cross-domain automatic placement is not.

---

## Architecture Overview

```text
Browser
  -> tiering UI
  -> tierd unified tiering API
     -> SQLite control-plane state
     -> background policy + reconciliation jobs
     -> mdadm/LVM tiering adapter
     -> ZFS managed tiering adapter
     -> existing backend-specific APIs
```

At a high level:

1. backend-specific admin APIs create storage primitives
2. the unified control plane discovers or provisions tier targets on top of those primitives
3. adapters report capabilities, state, activity summaries, and movement status
4. the control plane reconciles desired placement intent with backend reality
5. the data plane executes movement asynchronously

---

## Terminology

### TierTarget

A ranked storage target that can accept placement policy.

Examples:

- an mdadm-backed LVM tier inside a tier instance
- a managed ZFS-backed storage target backed by one or more datasets on one or more pools

### ManagedNamespace

A user-visible logical storage space that the control plane can manage.

Examples:

- an mdadm managed volume
- a managed ZFS file-space

### PlacementDomain

A scheduling boundary inside which rank, fill policy, and automatic movement are meaningful.

Examples:

- one mdadm tier instance
- one managed ZFS tiering namespace family

The control plane may display targets from many domains together, but automatic movement and policy evaluation operate within a single domain.

### ManagedObject

The control-plane unit of pinning and placement reporting for a namespace.

Examples:

- mdadm: usually a managed volume, with region summaries beneath it
- managed ZFS: a file-space and then files or objects within it, depending on adapter capability

### MovementUnit

The backend-native unit of relocation:

- mdadm/LVM: region
- managed ZFS: file or object

### ActivitySummary

A normalized control-plane view of temperature or activity, not a promise that two backends expose identical raw metrics.

The control plane should prefer:

- `activity_band`
- `activity_trend`
- backend-native detail fields

over one cross-backend raw heat number.

#### ActivityBand values

| Value | Meaning |
| --- | --- |
| `hot` | Sustained high access rate; candidate for the fastest tier |
| `warm` | Moderate, intermittent access; candidate for intermediate tiers |
| `cold` | Infrequent access; candidate for cold tier |
| `idle` | No recent access within the collection window; candidate for cold or archive |

`activity_trend` values: `rising`, `stable`, `falling`.

Band assignment is backend-specific. Adapters must document their mapping:

- **mdadm/LVM**: derived from dm-stats region I/O rate over the sampling window. The adapter divides the per-region IOPS distribution into four buckets relative to the top-percentile rate for the tier instance. Hot = top quartile, warm = second quartile, cold = third quartile, idle = bottom quartile or zero-rate regions.
- **managed ZFS**: derived from file or object access frequency counters maintained by the adapter namespace service. Hot = accessed more than once per hour in the sampling window, warm = once per day, cold = once per week, idle = not accessed within the collection window.

The control plane must not compare raw band derivation values across adapters. It may display both bands in the same UI with backend-kind labels so the operator knows which system each band came from.

---

## Control Plane Responsibilities

The control plane is the policy and observability layer.

It owns:

- tier target identity
- rank and ordering
- target-fill policy
- full-threshold policy
- placement intent
- pin requests
- unified movement queue visibility
- reconciled health and degraded state
- backend capability discovery
- operator-facing normalized summaries

It does not own:

- direct block I/O
- direct file I/O
- block allocation inside LVM
- block allocation inside ZFS
- backend-native copy semantics
- backend-native transaction guarantees

### Steady-state behavior

The control plane must remain asynchronous relative to foreground I/O.

Good:

- mdadm writes continue landing on the highest-ranked writable tier until the tier's full threshold is reached, while policy drains excess in the background
- managed ZFS reads and writes continue through the active namespace while background movement happens through copy, verify, switch, and cleanup

Not allowed:

- per-read placement RPCs
- per-write control-plane approval
- a global lock that stalls the data plane while policy evaluates

---

## Data Plane Responsibilities

Each adapter owns its own placement reality.

It is responsible for:

- collecting backend-native activity signals
- enforcing placement using backend-native tools
- reporting current placement state
- validating correctness after movement
- exposing degraded or ambiguous states to the control plane
- recovering unfinished moves after restart

The adapter must report truthfully when placement intent is temporarily not enforceable.

---

## Canonical Control-Plane Model

### TierTarget

```go
type TierTarget struct {
    ID                string
    Name              string
    PlacementDomain   string
    Rank              int
    BackendKind       string
    TargetFillPct     int
    FullThresholdPct  int
    CapacityBytes     uint64
    UsedBytes         uint64
    Health            string
    ActivityBand      string
    ActivityTrend     string
    QueueDepth        int
    Capabilities      TargetCapabilities
    BackingRef        string
    BackendDetails    map[string]any
}
```

### TargetCapabilities

```go
type TargetCapabilities struct {
    MovementGranularity   string // region | file | object
    PinScope              string // volume | namespace | object | none
    SupportsOnlineMove    bool
    SupportsRecall        bool
    SnapshotMode          string // none | backend-native | coordinated-namespace
    SupportsChecksums     bool
    SupportsCompression   bool
    SupportsWriteBias     bool
}
```

### ManagedNamespace

```go
type ManagedNamespace struct {
    ID               string
    Name             string
    PlacementDomain  string
    BackendKind      string
    NamespaceKind    string // volume | filespace
    ExposedPath      string
    PolicyTargetIDs  []string
    PinState         string
    Health           string
    ActivityBand     string
    PlacementState   string
    BackendDetails   map[string]any
}
```

### ManagedObject

```go
type ManagedObject struct {
    ID                   string
    NamespaceID          string
    ObjectKind           string // volume | file | object
    ObjectKey            string
    PinState             string // none | pinned-hot | pinned-cold
    ActivityBand         string
    PlacementSummary     PlacementSummary
    BackendRef           string
    BackendDetails       map[string]any
}

type PlacementSummary struct {
    CurrentTargetID  string
    IntendedTargetID string
    State            string // placed | moving | stale | unknown
}
```

The control plane must allow adapters to expose different object depth.

Examples:

- mdadm can expose one managed volume with region summaries instead of millions of objects
- managed ZFS can expose file-space aggregates first, then optional file or object detail views later

The control plane therefore standardizes summary shape, not internal depth.

Managed object identity is namespace-qualified. A managed object ID is only required to be unique within its owning namespace unless the adapter explicitly guarantees global uniqueness.

---

## Backend Adapters

### 1. mdadm/LVM Region-Tiering Adapter

This adapter builds directly on `mdadm-complete-heat-engine`.

### Native model

- backing pool: tier instance volume group
- placement unit: region
- heat source: dm-stats
- movement mechanism: scoped `pvmove`
- write-path behavior: highest-ranked writable tier until full threshold
- steady-state balancing: target-fill policy and hysteresis
- pin scope: managed volume

### What the control plane sees

- ranked targets
- managed volumes
- activity summaries derived from region heat
- movement jobs and movement failure state
- target-fill and full-threshold policy
- pinned volumes

### What the control plane must not claim

- that mdadm movement is the same as ZFS-managed movement
- that mdadm activity values are directly comparable to ZFS object activity

### Fit with existing proposals

This adapter is the first concrete implementation because the mdadm design already defines:

- region inventory
- dm-stats sampling
- policy hysteresis
- spillover
- pinning
- `pvmove`-based online migration

---

### 2. Raw ZFS Backend Foundation

This proposal also subsumes the standalone ZFS backend work.

Raw ZFS remains a first-class SmoothNAS storage path even when it is not participating in unified tiering.

#### Scope

The raw ZFS backend includes:

- pool creation and destruction
- vdev layout management
- SLOG and L2ARC management
- dataset lifecycle
- zvol lifecycle
- snapshot lifecycle
- pool health and resilver monitoring
- scrub scheduling
- disk replacement
- ARC and TXG tuning

#### Raw ZFS backend non-goals

The raw backend does not include:

- ZFS deduplication
- ZFS native encryption
- scheduled send/recv replication to remote hosts
- sharing disks between the mdadm and ZFS backends
- cross-host or clustered ZFS

#### Native architecture

```text
ZFS datasets and zvols
  -> zpool
     -> data vdevs
     -> optional SLOG
     -> optional L2ARC
```

#### Supported vdev types

| Vdev Type | Min Disks | Description |
| --- | --- | --- |
| Single | 1 | No redundancy |
| Mirror | 2 | N-way mirror |
| RAIDZ1 | 3 | Single parity |
| RAIDZ2 | 4 | Double parity |
| RAIDZ3 | 5 | Triple parity |
| dRAID1 | 3 | Distributed spare RAIDZ1 |
| dRAID2 | 5 | Distributed spare RAIDZ2 |
| dRAID3 | 7 | Distributed spare RAIDZ3 |

#### Write-path behavior

```text
Async write:
  application
    -> ARC / in-memory TXG
    -> data vdevs on TXG commit

Sync write:
  application
    -> SLOG immediately
    -> data vdevs on TXG commit
```

Important product constraints:

- SLOG accelerates synchronous writes, not general write caching
- L2ARC is a read cache only
- deduplication is out of scope
- native encryption is out of scope for now
- scheduled send/recv replication is out of scope for now

#### ZFS-native capabilities

The raw backend exposes:

- checksumming
- compression
- snapshots
- send-to-file workflows
- quotas and reservations
- pool-level scrub and resilver operations

These remain backend-native capabilities even when a managed ZFS tiering adapter sits above them.

#### Appliance tuning

Default appliance guidance remains:

| Parameter | Appliance value | Rationale |
| --- | --- | --- |
| `zfs_arc_max` | 75% of RAM when ZFS is the only active backend | Maximize ARC on a storage appliance |
| `zfs_arc_max` with mdadm also active | 50% of RAM | Leave headroom for page cache and mdadm path |
| `zfs_arc_min` | 25% of RAM | Prevent excessive ARC collapse |
| `zfs_txg_timeout` | 5 seconds | Default throughput/latency trade-off |

#### Packages

| Package | Purpose |
| --- | --- |
| `zfsutils-linux` | Pool, dataset, zvol, and snapshot management |
| `zfs-dkms` | OpenZFS kernel module |

#### Raw ZFS API surface

The following backend-specific endpoints are part of this proposal:

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/pools` | GET | List pools with health, size, free space, fragmentation, and vdev layout |
| `/api/pools` | POST | Create a pool with data vdevs and optional SLOG/L2ARC |
| `/api/pools/{name}` | GET | Detailed pool status, scan state, and errors |
| `/api/pools/{name}` | DELETE | Destroy a pool after confirmation |
| `/api/pools/{name}/vdevs` | POST | Add a data vdev |
| `/api/pools/{name}/slog` | POST | Add or replace SLOG devices |
| `/api/pools/{name}/slog` | DELETE | Remove SLOG devices |
| `/api/pools/{name}/l2arc` | POST | Add L2ARC devices |
| `/api/pools/{name}/l2arc` | DELETE | Remove L2ARC devices |
| `/api/pools/{name}/scrub` | POST | Start a scrub |
| `/api/pools/{name}/disks/{disk}/replace` | POST | Replace a failed disk |
| `/api/datasets` | GET | List datasets with usage, quota, reservation, compression, and mountpoint |
| `/api/datasets` | POST | Create a dataset |
| `/api/datasets/{id}` | GET | Get dataset detail |
| `/api/datasets/{id}` | PUT | Update quota, reservation, compression, and mountpoint |
| `/api/datasets/{id}` | DELETE | Destroy a dataset |
| `/api/datasets/{id}/mount` | POST | Mount a dataset |
| `/api/datasets/{id}/unmount` | POST | Unmount a dataset |
| `/api/zvols` | GET | List zvols |
| `/api/zvols` | POST | Create a zvol |
| `/api/zvols/{id}` | GET | Get zvol detail |
| `/api/zvols/{id}` | DELETE | Destroy a zvol |
| `/api/zvols/{id}/resize` | PUT | Resize a zvol |
| `/api/snapshots` | GET | List snapshots |
| `/api/snapshots` | POST | Create a snapshot |
| `/api/snapshots/{id}` | DELETE | Destroy a snapshot |
| `/api/snapshots/{id}/rollback` | POST | Roll back to a snapshot |
| `/api/snapshots/{id}/clone` | POST | Clone a snapshot |
| `/api/snapshots/{id}/send` | POST | Send snapshot to a file |

System surfaces also gain:

- ZFS pool state and SLOG/L2ARC presence in `/api/system/status`
- ARC and TXG tuning in `/api/system/tuning`
- ZFS disk assignment values in `/api/disks`

#### Raw ZFS UI

The backend-specific UI remains first-class:

- `Pools`
- `Datasets`
- `Zvols`
- `Snapshots`

Dashboard and Settings also gain:

- pool health
- resilver visibility
- L2ARC hit-rate visibility
- ARC and TXG tuning controls

#### Health monitoring

When any pool exists, `tierd` should monitor:

| Check | Source | Alert condition |
| --- | --- | --- |
| Pool health | `zpool status` | degraded, faulted, or resilvering |
| Resilver progress | `zpool status` | active resilver |
| Checksum errors | `zpool status` | any non-zero checksum error count |
| Dataset usage | `zfs list` | usage above configured threshold |
| L2ARC hit rate | `/proc/spl/kstat/zfs/arcstats` | cache is ineffective |
| ARC pressure | `/proc/spl/kstat/zfs/arcstats` | ARC pushed below intended floor |

#### Disk replacement workflow

1. operator sees a degraded-pool alert
2. operator identifies the failed disk in the vdev tree
3. operator physically replaces the disk
4. operator triggers replace from the UI
5. `tierd` runs `zpool replace`
6. resilver progress is reported in the UI

This raw backend is useful on its own, independent of unified tiering.

---

### 3. Managed ZFS Tiering Adapter

This adapter is the tiering extension of the raw ZFS backend defined in this document.

It does not replace the raw ZFS backend. It builds on top of it.

### Raw ZFS remains first-class

Pools, datasets, zvols, snapshots, SLOG, L2ARC, ARC tuning, scrub, and health workflows continue to be managed through dedicated ZFS pages and APIs.

### What is new

To participate in the unified tiering GUI, ZFS needs a managed layer above raw datasets.

That managed layer provides:

- one logical file-space per managed namespace
- metadata that records current placement
- policy-aware placement intent
- activity collection at file-space, file, or object granularity
- background copy, verify, switch, and cleanup
- optional recall-on-access behavior
- restricted direct access to backend storage paths

### Namespace implementation boundary

The managed ZFS adapter uses a **dedicated C FUSE daemon** built on **libfuse3** as the adapter-owned namespace service mounted at the exposed path. This is not a Go FUSE library embedded in tierd — it is a separate process written in C.

#### Why a separate C daemon, not go-fuse inside tierd

Two alternatives were rejected:

- **go-fuse inside tierd**: go-fuse does not support libfuse3's passthrough fd API, which is the single most impactful FUSE performance feature available. Without passthrough, every read and write round-trips through userspace. For a NAS appliance whose primary workload is large sequential I/O, that overhead is unacceptable.
- **CGo wrapper around libfuse3 inside tierd**: libfuse3 uses pthreads internally. CGo forces Go goroutines onto OS threads for every C call, which interacts badly with Go's scheduler under the threading model libfuse3 expects. CGo call overhead also applies on every FUSE callback, partially negating the passthrough benefit.

A separate C daemon avoids both problems. The C daemon is a thin relay (~500–1,000 lines); it contains no placement logic.

#### Architecture

```text
application / NFS / SMB
  → kernel FUSE driver
  → C FUSE daemon (libfuse3)
      on open():     Unix socket query → tierd → backing fd returned to kernel
      on read()/write(): kernel passthrough directly to backing fd (no userspace hop)
      on metadata:   Unix socket query → tierd (or local cache hit)
      on release():  notify tierd fd is closed
  → tierd (Go)
      authoritative placement metadata
      placement decisions
      policy reconciliation
      movement scheduling
```

Placement decisions happen only at `open()` time. After the C daemon registers a passthrough fd with the kernel, subsequent `read()` and `write()` calls on that fd bypass userspace entirely — the kernel reads and writes directly against the backing dataset file.

#### fd passing

tierd opens backing files and passes the open fds to the C daemon over the Unix socket using `SCM_RIGHTS` ancillary data. The C daemon never opens backing files itself and therefore cannot bypass placement logic by choosing a different backing path. tierd is the sole arbiter of which backing fd maps to which namespace object.

#### Kernel version dependency

FUSE passthrough requires Linux 6.x. The C daemon must detect whether the kernel supports `FUSE_PASSTHROUGH` at mount time using `fuse_lowlevel_notify_retrieve`:

- if passthrough is available: register the backing fd with the kernel on each `open()` response
- if passthrough is not available: fall back to traditional read/write handlers that forward I/O through the daemon (with the traditional FUSE overhead)

The fallback must be tested and must not be removed. SmoothNAS may run on kernels older than 6.x.

#### Bypass prevention

Backing datasets must be inaccessible to operators through normal ZFS dataset paths:

- backing datasets are owned by the `tierd` system user (e.g., `tierd:tierd`, mode `700`)
- `zfs allow` must not grant dataset-level access to operator accounts for adapter-owned datasets
- backing dataset mountpoints, if any, must not be under `/mnt` paths exposed to users
- `tierd` must detect and report a `degraded_state` event with code `bypass_detected` when it observes atime, mtime, or ctime changes on a backing dataset that were not initiated through the FUSE namespace

#### Shim lifecycle

`tierd` is responsible for starting, supervising, and restarting the C FUSE daemon. If the daemon exits unexpectedly:

1. the FUSE mount becomes unavailable, causing I/O errors on in-flight operations
2. `tierd` detects the exit, reports a `degraded_state` event with code `namespace_unavailable`
3. `tierd` attempts to restart the daemon and remount
4. existing open fds held by applications become invalid after remount; applications must reopen

`tierd` must not attempt to restart the daemon more than N times within a window to avoid a crash loop that continuously interrupts I/O.

#### Recall-on-access mechanics

When recall-on-access is enabled, the C daemon intercepts `open()` for an object whose authoritative location is a cold or warm backing dataset. It queries tierd, which:

1. issues a recall request to the movement worker
2. either blocks the `open()` reply until recall completes and returns the fast-tier backing fd (synchronous recall mode, Phase 3), or returns the cold-tier backing fd immediately while recall proceeds in the background (asynchronous recall mode, deferred)

In both modes, the passthrough fd registered with the kernel points at the correct authoritative dataset for that object at the time of open. If the object migrates while the file is open (rare, requires policy change during an active open), the C daemon receives a placement-changed notification from tierd and must either invalidate the cached fd or leave the open to complete against the old location and recheck on the next open.

The adapter must advertise which recall mode it provides. The control plane must not assume synchronous promotion semantics.

#### Performance characteristics

| Workload | Traditional FUSE (no passthrough) | libfuse3 + passthrough (Linux 6.x) |
| --- | --- | --- |
| Sequential read/write throughput | −10–30% vs bare metal | −2–5% vs bare metal |
| Per read/write latency | +5–20 μs | ~0 (kernel direct path) |
| Random small I/O (IOPS) | −20–50% vs bare metal | −5–15% vs bare metal |
| Per `open()` latency | +5–20 μs (FUSE round-trip) | +6–25 μs (FUSE round-trip + IPC to tierd) |

The `open()` overhead is the residual cost with passthrough. It is acceptable for the NAS workloads this adapter targets: large sequential reads and writes dominate, and `open()` is infrequent relative to I/O volume. For workloads that open thousands of small files per second, the per-open IPC cost is noticeable and operators should prefer raw ZFS datasets.

NFS and SMB exports through the FUSE namespace benefit from passthrough: the data path becomes application → NFS/SMB kernel handler → FUSE passthrough → backing dataset, with no userspace hop on read/write. This is a significant improvement over the non-passthrough case for network shares.

The fallback path (kernels without passthrough) retains the traditional FUSE numbers above and must be documented as a performance limitation for those kernels.

### Recommended initial ZFS tiering shape

The minimum viable design is a **managed file-space tiering layer**.

It should use the existing ZFS backend primitives like this:

- operators create and manage pools through `/api/pools`
- operators create or select backing datasets on those pools
- the tiering adapter claims specific datasets as backing targets
- the control plane exposes those datasets as ranked `TierTarget`s
- the managed namespace is mounted through an adapter-owned path, not through raw backing dataset paths

Each managed ZFS namespace belongs to exactly one `placement_domain`. Automatic movement, rank ordering, and target-fill policy are evaluated only within that domain.

### Backing layout

One practical initial layout is:

- one backing dataset per ranked target
- one adapter-owned metadata dataset
- one exposed logical namespace path

Example:

```text
pool_fast/tiering_fast
pool_warm/tiering_warm
pool_cold/tiering_cold
pool_meta/tiering_meta
exposed namespace: /mnt/tiering/<namespace>
```

The exact dataset naming is implementation detail, but the key rule is that the exposed namespace must be authoritative and raw backing datasets must not be the supported user entry point.

### Write-path behavior

Writes land on the highest-ranked writable target below full threshold, just as they do in the mdadm design conceptually.

The difference is implementation:

- mdadm allocates extents onto a fast PV
- managed ZFS places new files or objects into the fast backing dataset

#### Fill policy semantics for managed ZFS

`target_fill_pct` is a steady-state occupancy goal, not a synchronous hard gate.

- When a target's `used / capacity` is below `target_fill_pct`, new writes land on that target.
- When a target's `used / capacity` exceeds `target_fill_pct`, the background policy worker begins scheduling outbound movement to the next lower-ranked target. Writes continue landing on the over-full target until it exceeds `full_threshold_pct`.
- When a target's `used / capacity` exceeds `full_threshold_pct`, the FUSE namespace service stops placing new files on that target and promotes the next-lower-ranked writable target to receive writes. If no colder writable target exists, the adapter reports a `degraded_state` event with code `no_drain_target`.

This matches the mdadm spillover model conceptually. The difference is that mdadm enforces it at extent allocation time; managed ZFS enforces it at file-placement time through the FUSE namespace service.

### Read-path behavior

Reads continue through the managed namespace via the FUSE passthrough path once a file is open. There is no per-read userspace hop after `open()` returns.

If recall-on-access is enabled, the overhead occurs at `open()` time only:

- the C daemon queries tierd, which checks the object's current placement
- if the object is cold, tierd either blocks the open reply until recall completes (Phase 3), or returns the cold-tier backing fd immediately (async recall, later phase)
- subsequent reads on the open fd go directly to the backing dataset through the kernel passthrough path

Because this is materially different from mdadm, the adapter must advertise recall behavior explicitly. The control plane must not assume synchronous promotion semantics or kernel-native transparent relocation.

### Movement behavior

Managed ZFS movement is:

1. select candidate files or objects
2. copy to the destination dataset
3. verify content and metadata
4. switch authoritative location
5. clean up the old copy

This is not equivalent to `pvmove`.

The UI should show:

- movement granularity: `file` or `object`
- recall support: `true` or `false`
- online move semantics: backend-specific

Movement plans are valid only for the `placement_domain`, `policy_revision`, and `intent_revision` under which they were created. The adapter must reject or replan stale movements on start, resume, or reconciliation.

### ZFS-native feature preservation

The managed tiering adapter must preserve the benefits from the raw ZFS backend where possible:

- checksumming remains a backend capability
- compression remains configurable on backing datasets
- snapshots remain available, but snapshot semantics must be defined at the managed-namespace layer
- SLOG and L2ARC remain pool-level accelerators underneath the adapter
- ARC sizing remains a system-level tuning concern

### Snapshot semantics

Raw dataset snapshots are not enough on their own once one logical namespace spans more than one ranked backing target.

The adapter therefore needs one of these models:

1. a managed namespace snapshot that coordinates snapshots across all backing datasets and adapter metadata
2. or no unified snapshot operation until that coordination exists

The control plane must not advertise namespace snapshots unless the adapter can provide crash-consistent coordinated snapshots.

Accordingly, snapshot capability must be modeled as a mode, not a boolean:

- `none`
- `backend-native`
- `coordinated-namespace`

### Direct access restrictions

The proposal must be explicit here:

- direct user access to backing datasets bypasses placement accounting
- unmanaged writes to backing datasets invalidate tiering metadata
- raw backing datasets should therefore be hidden from the normal operator workflow or documented as unsupported for tier-managed namespaces

### Why this is not "just part of ZFS"

The managed ZFS tiering adapter is not a small wrapper around `zfs list`.

It is a real subsystem with:

- a C FUSE daemon (libfuse3) with passthrough and fallback paths
- a Unix socket protocol between the daemon and tierd
- fd passing via `SCM_RIGHTS`
- daemon lifecycle supervision inside tierd
- metadata ownership and a metadata dataset
- namespace ownership via the FUSE mount
- migration workers (copy, verify, switch, cleanup)
- policy reconciliation
- failure recovery
- bypass prevention

That is acceptable, but the rollout and effort must acknowledge it.

---

## Capability Model

The unified GUI should show both normalized workflow and honest backend differences.

| Capability | mdadm/LVM adapter | managed ZFS adapter |
| --- | --- | --- |
| Movement granularity | region | file or object |
| Heat source | dm-stats region activity | file-space, file, or object activity |
| Online relocation | yes, via `pvmove` | adapter-defined, usually copy/switch |
| Recall on access | no | optional |
| Checksumming | inherited from underlying stack, not native end-to-end data checksumming | native ZFS checksumming |
| Snapshot mode | backend-native or none, depending on backend volume stack | backend-native underneath, `coordinated-namespace` only when adapter metadata is snapshotted consistently |
| Compression | filesystem-dependent | native ZFS compression |
| Pin scope | volume | namespace or object |

The control plane should normalize:

- rank
- fill policy
- activity band
- placement health
- movement status

It should not normalize away:

- placement domain boundaries
- movement granularity
- recall behavior
- snapshot behavior
- raw heat metric meaning

---

## Adapter Contract

Every backend admitted to the unified tiering GUI must satisfy a common contract.

### Required behaviors

- expose targets, capacities, and usage
- expose backend capabilities
- expose current placement state
- accept target-fill and threshold policy
- expose normalized activity summaries plus backend-native details
- expose pin support and pin granularity
- support background movement
- report movement progress and failure state
- report degraded states and intent-enforcement gaps
- invalidate stale plans when policy or intent revisions change
- reconcile persisted control-plane intent with backend reality on startup

### Recommended Go interface

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

`CancelMovement` must be able to abort an in-progress movement and leave the source object authoritative. The adapter is responsible for cleaning up any partial copy.

### Degraded-state contract

This proposal requires explicit degraded-state reporting.

Examples:

- "placement intent stale"
- "metadata reconciliation required"
- "raw backing path accessed outside managed namespace"
- "movement verification failed"
- "movement plan stale after policy change"
- "target over full threshold and no colder destination available"

The unified GUI should show this plainly instead of pretending the target is healthy.

### Error taxonomy

All `TieringAdapter` methods return errors using a structured type so the control plane can take the correct action without inspecting error strings.

```go
type AdapterError struct {
    Kind    AdapterErrorKind
    Message string
    Cause   error
}

type AdapterErrorKind string

const (
    // ErrTransient: a temporary backend condition; the control plane may retry
    // after a backoff. Examples: backend process restarting, lock contention.
    ErrTransient AdapterErrorKind = "transient"

    // ErrPermanent: operator action is required before the operation can succeed.
    // Examples: corrupt metadata, insufficient space with no drain path.
    ErrPermanent AdapterErrorKind = "permanent"

    // ErrCapabilityViolation: the adapter does not support the requested
    // operation at all. Examples: cancelling movement on an adapter that only
    // supports pvmove (which self-manages cancellation).
    ErrCapabilityViolation AdapterErrorKind = "capability_violation"

    // ErrStaleRevision: the operation was rejected because the policy or intent
    // revision the caller passed no longer matches current state.
    ErrStaleRevision AdapterErrorKind = "stale_revision"

    // ErrBackendDegraded: the backend cannot safely execute the operation in
    // its current health state. The control plane should surface this and pause
    // scheduling for the affected target.
    ErrBackendDegraded AdapterErrorKind = "backend_degraded"
)
```

Methods that complete synchronously and start a background operation (such as `StartMovement`) return `ErrTransient` if the backend is temporarily unavailable, and `ErrPermanent` if the plan is structurally invalid. They must not block until the background operation completes.

---

## Control-Plane Data Model

The control plane should persist its own durable state in SQLite.

### Tables

#### `placement_domains`

- `id` (same as the string name; human-readable, unique)
- `backend_kind`
- `description`
- `created_at`
- `updated_at`

A placement domain row is created automatically when the first `tier_target` referencing that domain name is registered. It is removed when no targets remain in it. The `id` is the domain name string, not an opaque UUID, so it is stable across restarts.

#### `tier_targets`

- `id`
- `name`
- `placement_domain`
- `backend_kind`
- `rank`
- `target_fill_pct`
- `full_threshold_pct`
- `policy_revision`
- `health`
- `activity_band`
- `activity_trend`
- `capabilities_json`
- `backing_ref`
- `created_at`
- `updated_at`

#### `managed_namespaces`

- `id`
- `name`
- `placement_domain`
- `backend_kind`
- `namespace_kind`
- `exposed_path`
- `pin_state`
- `intent_revision`
- `health`
- `placement_state`
- `backend_ref`
- `created_at`
- `updated_at`

#### `managed_objects`

- `id`
- `namespace_id`
- `object_kind`
- `pin_state`
- `activity_band`
- `object_key`
- `placement_summary_json`
- `backend_ref`
- `updated_at`

#### `movement_jobs`

- `id`
- `backend_kind`
- `namespace_id`
- `object_id` (nullable)
- `movement_unit`
- `placement_domain`
- `source_target_id`
- `dest_target_id`
- `policy_revision`
- `intent_revision`
- `planner_epoch`
- `state`
- `triggered_by`
- `progress_bytes`
- `total_bytes`
- `failure_reason`
- `started_at`
- `updated_at`
- `completed_at`

#### `placement_intents`

- `id`
- `namespace_id`
- `object_id` (nullable)
- `intended_target_id`
- `placement_domain`
- `policy_revision`
- `intent_revision`
- `reason`
- `state`
- `updated_at`

#### `degraded_states`

- `id`
- `backend_kind`
- `scope_kind`
- `scope_id`
- `severity`
- `code`
- `message`
- `updated_at`

### Backend-native detail storage

Backend-specific detail should stay backend-specific.

Examples:

- mdadm region heat rows remain in mdadm-oriented tables
- managed ZFS object placement metadata remains in ZFS-oriented tables

The control plane stores normalized summaries and identifiers, not every low-level backend detail.

### Revision and invalidation rules

The control plane must version policy and intent explicitly:

- every target policy change increments `policy_revision`
- every namespace or object placement change increments `intent_revision`
- every planner run records a `planner_epoch`

A movement job is valid only if its recorded domain, policy revision, and intent revision still match current state. Otherwise the job must be marked stale and replanned rather than continuing under obsolete assumptions.

---

## Unified API

### Common endpoints

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/tiering/domains` | GET | List placement domains with member target count and health summary |
| `/api/tiering/domains/{id}` | GET | Placement domain detail: member targets, rank order, fill state |
| `/api/tiering/targets` | GET | List all tier targets across adapters |
| `/api/tiering/targets/{id}` | GET | Detailed target view, including capabilities and backend details |
| `/api/tiering/targets/{id}/policy` | PUT | Update target fill and threshold policy |
| `/api/tiering/namespaces` | GET | List managed namespaces |
| `/api/tiering/namespaces` | POST | Create a managed namespace (adapter and backing spec in body) |
| `/api/tiering/namespaces/{id}` | GET | Namespace detail with placement and activity summaries |
| `/api/tiering/namespaces/{id}` | DELETE | Destroy a managed namespace |
| `/api/tiering/namespaces/{id}/pin` | PUT | Apply a namespace-level pin when supported |
| `/api/tiering/namespaces/{id}/pin` | DELETE | Remove a namespace-level pin |
| `/api/tiering/namespaces/{id}/snapshot` | POST | Create a coordinated namespace snapshot (adapter must support `coordinated-namespace`) |
| `/api/tiering/namespaces/{id}/objects` | GET | Optional managed-object listing for a namespace |
| `/api/tiering/namespaces/{id}/objects/{object_id}` | GET | Namespace-qualified managed-object detail |
| `/api/tiering/namespaces/{id}/objects/{object_id}/pin` | PUT | Apply an object-level pin when supported |
| `/api/tiering/namespaces/{id}/objects/{object_id}/pin` | DELETE | Remove an object-level pin |
| `/api/tiering/movements` | GET | List movement jobs across adapters |
| `/api/tiering/movements/{id}` | DELETE | Cancel a movement job (adapter must support `CancelMovement`) |
| `/api/tiering/degraded` | GET | List degraded-state signals across adapters |
| `/api/tiering/reconcile` | POST | Trigger control-plane reconciliation |

### Backend-specific APIs remain

These remain valid and authoritative for deep admin tasks:

- `/api/tiers`
- `/api/volumes`
- `/api/pools`
- `/api/datasets`
- `/api/zvols`
- `/api/snapshots`

The control-plane API sits above them. It does not replace them.

---

## Unified GUI

### Primary operator workflow

The unified tiering GUI should optimize for these tasks:

1. inspect ranked storage targets
2. adjust rank and target-fill policy
3. inspect activity and placement summaries
4. see movement backlog and failures
5. pin data at the supported scope

### Placement domain grouping

Targets in the tiering inventory are grouped by `placement_domain`. Each domain renders as a collapsible section with a header showing:

- domain name
- backend kind
- overall health (degraded if any member target is degraded)
- aggregate used and capacity

Within a domain, targets are sorted by rank ascending. Rank numbers are only meaningful within the section — no visual treatment should suggest that `rank=1` in one domain is comparable to `rank=1` in another.

Operators may filter the inventory to a single domain. The default view shows all domains.

When a movement job spans two targets, both targets must be in the same domain. The GUI must not present a cross-domain movement option.

### What is shown by default

- target name
- backend kind
- placement domain
- rank
- used and capacity
- target fill and full threshold
- health
- activity band
- queue depth
- pin capability
- movement granularity

### What is shown for advanced users

- backend-native heat detail
- recall behavior and recall mode (synchronous or asynchronous)
- snapshot support model
- checksum support
- compression support
- backing pool or tier references
- degraded-state details

### What must not be implied

The GUI must not imply:

- that rank is globally interchangeable across backends
- equal heat metrics across backends
- equal movement latency across backends
- equal snapshot semantics across backends
- that direct raw ZFS dataset access remains supported for managed namespaces

---

## Rollout Plan

### Phase 1: mdadm-backed unified control plane

**Effort: S**

This phase assumes `mdadm-complete-heat-engine` is done: dm-stats region sampling, policy hysteresis, `pvmove`-based online migration, and pin support are all implemented and in `testing`. Phase 1 remaps that existing machinery into the unified control-plane schema. It does not introduce any new heat-engine capability.

Work items:

- introduce common tier target and namespace models
- map mdadm heat-tiering into the unified API
- expose mdadm capabilities truthfully
- keep ZFS as backend-specific UI only
- migrate existing mdadm tier state into the unified schema on first start (see Migration below)

Exit criteria:

- mdadm targets show in `/api/tiering/targets`
- mdadm managed volumes show in `/api/tiering/namespaces`
- pinning, movement status, and activity summaries work through the common API
- existing tier instances and managed volumes are visible without operator re-registration after upgrade

### Phase 2: ZFS backend foundation

**Effort: M**

- ship the raw ZFS backend defined in this proposal
- deliver pool, dataset, zvol, snapshot, SLOG, L2ARC, and ARC management
- deliver scrub scheduling, disk replacement, and health monitoring
- keep raw ZFS outside the unified tiering GUI

Exit criteria:

- ZFS backend is usable and stable as a separate storage path
- no raw dataset is misrepresented as a tier target

### Phase 3: managed ZFS tiering adapter

**Effort: XL**

This is the largest phase. It introduces a novel subsystem: a C FUSE daemon using libfuse3 with passthrough, a Unix socket protocol between the daemon and tierd, metadata store, migration workers, bypass detection, and synchronous recall-on-access. Do not underestimate it.

Implement in this order to keep the subsystem testable at each step:

1. **C FUSE daemon skeleton** — mount, unmount, passthrough fd registration, Unix socket server, `SCM_RIGHTS` fd passing; no placement logic yet, all opens return the single backing fd
2. **Daemon protocol and placement routing** — `open()` queries tierd over the socket; tierd returns the correct backing fd per current placement metadata; passthrough registered with kernel
3. **Daemon lifecycle supervision** — tierd starts, monitors, and restarts the daemon; `namespace_unavailable` degraded state on crash
4. **Kernel version detection and fallback** — detect `FUSE_PASSTHROUGH` capability at mount time; fall back to traditional read/write handlers on older kernels
5. **Metadata store and dataset layout** — adapter-owned metadata dataset, backing dataset provisioning, `CreateTarget` / `CreateNamespace`
6. **Migration workers** — background copy, verify, switch, cleanup; movement jobs visible through the unified API
7. **Bypass protection** — dataset ownership (`tierd:tierd`, mode `700`), `bypass_detected` degraded-state reporting
8. **Synchronous recall-on-access** — block `open()` reply until recall completes; async recall is deferred to a later phase

Exit criteria:

- managed ZFS targets appear in `/api/tiering/targets`
- one managed ZFS namespace appears in `/api/tiering/namespaces`
- movement status and placement summaries are visible
- direct raw backing dataset access is technically restricted or reported as degraded
- the FUSE namespace is the authoritative entry point for local and network access
- passthrough is active on Linux 6.x; traditional FUSE fallback is active on older kernels
- sequential read/write throughput through the managed namespace is within 5% of raw ZFS on Linux 6.x

### Phase 4: mixed-backend tiering UI and coordinated snapshots

**Effort: S**

- show mdadm and managed ZFS tier targets in one tiering inventory, grouped by placement domain
- keep capability badges prominent
- preserve backend-specific deep admin pages
- keep policy evaluation and movement domain-scoped
- implement coordinated namespace snapshot for managed ZFS (multi-dataset snapshot + adapter metadata snapshot, exposed via `/api/tiering/namespaces/{id}/snapshot`)

Exit criteria:

- the common UI unifies workflow without hiding capability differences
- operators can distinguish movement granularity and recall behavior immediately
- operators can see placement-domain groupings and domain boundaries clearly
- managed ZFS namespaces report `coordinated-namespace` snapshot support after Phase 4 ships

---

## Migration

### Existing mdadm tier state

Before this proposal, `tierd` stores mdadm tier state in backend-specific tables (`tier_pools`, `managed_volumes`, etc.). Phase 1 must migrate that state into the unified control-plane schema on first start after upgrade.

Migration rules:

- Each existing `tier_pool` maps to one `placement_domain` and its constituent tiers map to `tier_targets` within that domain.
- Each existing `managed_volume` maps to one `managed_namespace` with `backend_kind = "mdadm"` and `namespace_kind = "volume"`.
- Existing policy rows (fill percentage, full threshold) are preserved as-is in the new `tier_targets` schema.
- Existing movement job state is migrated to `movement_jobs` or discarded if the job is in a terminal state.
- The migration runs idempotently: if the unified schema rows already exist, the migration does not re-create or overwrite them.

The migration must complete before `tierd` begins serving API traffic on startup. If migration fails, `tierd` must log the error and exit rather than serving stale or partial state.

No operator action is required. Existing tier instances and managed volumes must be visible in the unified API immediately after upgrade without re-registration.

---

## Risks and Mitigations

### False abstraction

Risk:

The UI implies equal semantics where none exist.

Mitigation:

- normalize workflow only
- show backend kind and capability badges
- use activity bands instead of a fake universal heat number
- show placement domains explicitly and forbid cross-domain auto movement

### ZFS scope explosion

Risk:

The managed ZFS adapter is underestimated and grows into an unplanned storage subsystem.

Mitigation:

- keep the initial adapter file-space-oriented
- reuse raw ZFS backend primitives
- make the subsystem boundary explicit in this proposal

### Namespace bypass

Risk:

Users write directly to backing datasets and invalidate placement metadata.

Mitigation:

- expose an adapter-owned namespace path as authoritative
- hide or document raw backing datasets as unsupported for tier-managed use
- report degraded state when bypass is detected

### Control-plane hot-path creep

Risk:

Policy logic leaks into foreground reads and writes.

Mitigation:

- require asynchronous policy enforcement
- forbid per-I/O control-plane round-trips
- keep placement adjustment in background jobs

### Stale movement plans

Risk:

Policy or placement changes invalidate an in-flight movement job, but the job continues anyway.

Mitigation:

- version policy and intent explicitly
- attach revisions and planner epoch to each movement job
- require adapters to reject stale plans during start, resume, and reconciliation

### Snapshot confusion

Risk:

Users assume a managed namespace snapshot is just a raw ZFS dataset snapshot.

Mitigation:

- advertise namespace snapshots only when coordinated snapshots exist
- otherwise keep snapshot UX backend-specific

### C FUSE daemon crash mid-I/O

Risk:

The C daemon exits while applications have open fds through the FUSE mount. Existing open fds become invalid; applications see I/O errors.

Mitigation:

- tierd detects daemon exit and immediately reports `namespace_unavailable` degraded state
- tierd remounts after restart with exponential backoff; cap restart attempts within a window to prevent a crash loop
- document that open fds are invalidated on remount and applications must reopen
- do not advertise managed ZFS namespace availability until the daemon is confirmed live and the mount is healthy

### fd passing failure

Risk:

`SCM_RIGHTS` fd passing from tierd to the C daemon fails or returns a stale fd, causing the daemon to register the wrong backing file with the kernel passthrough.

Mitigation:

- the daemon must validate the received fd before registering it (e.g., `fstat` to confirm it refers to the expected inode on the expected backing dataset)
- if validation fails, the daemon must return an `EIO` error for the `open()` rather than registering a bad passthrough fd
- tierd must log and report a `degraded_state` event with code `fd_pass_failed` when this occurs

### Passthrough unavailable on target kernel

Risk:

SmoothNAS is deployed on a kernel older than 6.x and the passthrough fallback path is undertested, causing silent data corruption or incorrect placement.

Mitigation:

- the traditional read/write fallback must have its own integration test suite, run against the oldest supported kernel
- the UI and API must report the active FUSE mode (`passthrough` or `fallback`) as part of the managed namespace capability so operators know which performance profile applies
- document clearly that the −10–30% throughput overhead applies on the fallback path

### Schema migration failure

Risk:

The Phase 1 migration from backend-specific mdadm tables to the unified schema fails mid-run, leaving partial data that corrupts subsequent reads.

Mitigation:

- wrap the entire migration in a SQLite transaction; roll back and exit on any error
- run migration idempotently so a retry after a rolled-back attempt succeeds cleanly
- log the migration outcome (rows migrated, revisions assigned) so the operator can verify it

### Insufficient observability

Risk:

The unified control plane ships without its own health signals, making it impossible to diagnose scheduling problems in production.

Mitigation:

The control plane must expose the following signals when any adapter is active:

| Check | Source | Alert condition |
| --- | --- | --- |
| Movement queue depth | `movement_jobs` table | more than N jobs in `pending` state for more than T minutes |
| Stale movement jobs | `movement_jobs` table | any job older than `updated_at + max_job_age` still in `running` state |
| Degraded-state count | `degraded_states` table | any row with `severity = critical` |
| Failed movement rate | `movement_jobs` table | more than N jobs transitioned to `failed` within a sliding window |
| Reconciliation staleness | last `Reconcile()` run timestamp | reconciliation has not run within the expected interval |

---

## Acceptance Criteria

### Common model

- [ ] `TierTarget`, `ManagedNamespace`, and capability reporting exist in the backend.
- [ ] The unified API can list targets and namespaces across adapters.
- [ ] The unified API exposes degraded-state reporting.
- [ ] Activity is normalized as summaries or bands, not as a fake cross-backend raw metric.
- [ ] Placement domains are explicit, and rank is only enforced within a placement domain.
- [ ] Movement jobs are revisioned and invalidated when policy or intent changes.

### mdadm adapter

- [ ] mdadm heat-tiered targets appear in the unified target list.
- [ ] mdadm managed volumes appear in the unified namespace list.
- [ ] mdadm movement status and pinning are visible through the unified API.
- [ ] mdadm target-fill policy can be managed through the common policy surface.

### ZFS backend integration

- [ ] Raw ZFS pools, datasets, zvols, and snapshots remain manageable through backend-specific APIs and pages.
- [ ] Raw ZFS datasets do not appear in the unified tiering GUI by default.
- [ ] Pool creation, dataset lifecycle, zvol lifecycle, snapshot lifecycle, SLOG, L2ARC, ARC tuning, scrub, and disk replacement are defined in this proposal.
- [ ] The proposal defines how SLOG, L2ARC, ARC tuning, checksumming, compression, and snapshots remain ZFS-native capabilities beneath the adapter.
- [ ] Raw ZFS health monitoring and package requirements are defined in this proposal.

### Managed ZFS adapter

- [ ] A managed ZFS-backed tier target can appear in the unified target list.
- [ ] A managed ZFS namespace can appear in the unified namespace list.
- [ ] Managed ZFS movement status is visible through the unified API.
- [ ] The adapter exposes its movement granularity, recall behavior, snapshot support model, and checksum support honestly.
- [ ] The adapter reports degraded state when managed namespace guarantees are broken or ambiguous.
- [ ] The managed namespace is implemented as a C FUSE daemon using libfuse3, not go-fuse or a raw dataset mount.
- [ ] Local access and network exports for a managed namespace terminate at the FUSE mount.
- [ ] On Linux 6.x, the C FUSE daemon registers a passthrough fd with the kernel on each `open()`; read/write operations do not pass through userspace.
- [ ] On kernels without `FUSE_PASSTHROUGH`, the daemon falls back to traditional read/write handlers without error.
- [ ] tierd passes backing fds to the C daemon via `SCM_RIGHTS`; the daemon validates each fd before registering it.
- [ ] The API reports the active FUSE mode (`passthrough` or `fallback`) as part of the managed namespace capability.
- [ ] tierd supervises the C daemon and reports `namespace_unavailable` degraded state on exit; it remounts after restart with backoff.
- [ ] Sequential read/write throughput through the managed namespace is within 5% of raw ZFS on Linux 6.x with passthrough active.

### UI honesty

- [ ] The unified GUI shows backend kind and capability badges.
- [ ] The unified GUI does not imply that mdadm and managed ZFS share identical movement or heat semantics.
- [ ] The unified GUI groups targets by placement domain and does not imply cross-domain rank equivalence.
- [ ] Raw backing dataset access is either restricted or clearly documented as unsupported for managed ZFS tiering.

### Migration

- [ ] Existing mdadm tier instances and managed volumes appear in the unified API after upgrade without operator re-registration.
- [ ] Migration runs inside a SQLite transaction and rolls back cleanly on error.
- [ ] Migration is idempotent; re-running it does not duplicate rows.

### Observability

- [ ] The control plane exposes movement queue depth, stale job count, degraded-state count, failed movement rate, and reconciliation staleness.
- [ ] Alerts fire when movement queue depth or failed movement rate exceeds configured thresholds.

---

## Test Plan

- [ ] Unit tests for capability mapping per adapter.
- [ ] Unit tests for activity-band normalization from backend-native inputs.
- [ ] Unit tests for degraded-state propagation through the unified API.
- [ ] Unit tests for policy persistence and rank ordering.
- [ ] Unit tests for placement-domain enforcement and cross-domain movement rejection.
- [ ] Unit tests for policy revision and intent revision invalidation.
- [ ] Integration tests that mdadm targets and namespaces appear correctly in the unified API.
- [ ] Integration tests for raw ZFS pool, dataset, zvol, and snapshot lifecycle.
- [ ] Integration tests for SLOG and L2ARC add/remove workflows.
- [ ] Integration tests for ZFS disk replacement and resilver visibility.
- [ ] Integration tests that raw ZFS datasets do not appear in the unified tiering inventory.
- [ ] Integration tests that managed ZFS namespaces are exposed only through the adapter-owned mount.
- [ ] Integration tests for managed ZFS namespace movement lifecycle: copy, verify, switch, cleanup.
- [ ] Integration tests for movement failure reporting and restart recovery.
- [ ] Integration tests for namespace bypass detection or restriction on the managed ZFS adapter.
- [ ] UI tests for backend badges, movement granularity display, and recall visibility.
- [ ] UI tests for placement-domain grouping: targets in the same domain appear together; targets in different domains are not rank-compared.
- [ ] Integration test that migration from pre-unified mdadm schema populates `tier_targets` and `managed_namespaces` correctly.
- [ ] Integration test that migration rolled back inside a transaction leaves the database unchanged.
- [ ] Integration test for movement cancellation: cancelled job leaves source object authoritative and cleans up partial copy.
- [ ] Integration test: C FUSE daemon crash while a file is open causes `namespace_unavailable` degraded state; tierd remounts; subsequent opens succeed.
- [ ] Integration test: `SCM_RIGHTS` fd passing delivers a valid fd; daemon validates inode; passthrough is registered correctly.
- [ ] Integration test: daemon detects invalid fd from tierd, returns `EIO` on open, and reports `fd_pass_failed` degraded state.
- [ ] Integration test: passthrough fallback path on a kernel without `FUSE_PASSTHROUGH` — reads and writes complete correctly through the traditional handler.
- [ ] Throughput benchmark: sequential read and write through the managed namespace on Linux 6.x with passthrough is within 5% of direct ZFS dataset access.
- [ ] Throughput benchmark: sequential read and write through the managed namespace on the fallback path shows the expected −10–30% overhead.
- [ ] Integration test for coordinated namespace snapshot on managed ZFS: all backing datasets and adapter metadata are snapshotted consistently.
- [ ] Integration test for activity-band normalization per adapter: mdadm dm-stats values and managed ZFS access counters produce the correct band values per the documented thresholds.

---

## Owner and Effort

- **Owner:** SmoothNAS storage control plane
- **Phase 1 (mdadm adapter migration):** S — remaps existing mdadm heat-engine state into unified schema; no new heat-engine work
- **Phase 2 (ZFS raw backend):** M — well-scoped pool/dataset/zvol/snapshot management; no novel subsystems
- **Phase 3 (managed ZFS tiering adapter):** XL — novel subsystem: FUSE namespace service, metadata store, migration workers, bypass detection, recall-on-access, policy reconciliation
- **Phase 4 (mixed-backend UI + coordinated snapshots):** S — UI grouping layer on top of finished API; snapshot coordination is bounded

---

## Conclusion

SmoothNAS should have one tiering control plane and more than one honest data plane.

mdadm/LVM is the first concrete implementation because its heat-tiering design already exists. ZFS enters the unified tiering story only through a managed adapter built on top of the raw ZFS backend, not by pretending raw datasets are interchangeable with region-tiered volumes.

That gives SmoothNAS one placement story for operators while preserving the project's core design value: the UI should make Linux storage understandable, not fictional.
