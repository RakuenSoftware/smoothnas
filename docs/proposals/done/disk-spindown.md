# Proposal: Disk Spindown — Phase 1 Idle-Power Controls

> **2026-04-24 update — FUSE has been removed.** The user-space
> `tierd-fuse-ns` daemon, `tierd/internal/tiering/fuse/` package, and
> related `fuse_mode` / `daemon_state` tracking are gone. `smoothfs`
> (the in-tree stacked kernel module) is the only data plane. Where
> this proposal refers to the FUSE daemon, the FUSE socket protocol,
> or the `mdadm`/`zfsmgd` FUSE handlers, treat those sections as
> historical context.



**Status:** Done

**Follow-up:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

SmoothNAS deploys mixed-tier pools where HDDs sit alongside SSDs (and sometimes NVMe). For long stretches the HDDs hold cold data that no client touches. Spinning them down during idle periods saves power, reduces heat, and extends drive life — typical homelab and small-office NAS workloads see HDDs idle for hours at a time.

Linux already exposes the basic mechanism (`hdparm -S`, `hdparm -y`, drive-firmware idle timers, the kernel block-layer power-management interfaces). What it does not give us is the *invariant* operators actually want: **once an HDD is spun down, only an actual data read or write to a file that lives on that HDD spins it back up.** Today, dozens of background activities — SMART polling, atime updates, ZFS TXG syncs, dirty-page writeback, heat sampling, NAS browse traffic — would wake a "spundown" HDD within seconds.

This proposal defined the spindown contract, named the silent-waker hazards, and staged the work needed to satisfy the contract. Phase 1 is now delivered: tierd exposes standby-aware disk power state, per-disk hdparm idle timers, manual standby, and SMART reads use `smartctl -n standby`.

The deeper metadata-on-SSD enforcement, active-window schedulers, write staging, and pool-level policy work has been split into the follow-up pending proposal.

---

## Problem

A useful spindown feature must answer three questions:

1. **What does "idle" mean?** A per-disk inactivity timer is the easy part; defining what counts as activity (and what does not) is the rest of the proposal.
2. **What is allowed to wake a spundown HDD?** A reasonable answer: only an in-band data read or write that genuinely needs bytes off the HDD. Everything else — operator browse traffic, atime updates, scrubs, SMART probes, heat sampling, deferred frees, dirty-page flush — must either be served from cache, deferred to an active window, or actively suppressed while the disk is in standby.
3. **What state has to live where for that to be physically possible?** The non-negotiable answer is **metadata on SSD** (and in RAM caches). If a `stat` on a file whose data lives on HDD has to read an inode block off that HDD, the HDD wakes on every directory listing.

Each of those answers cuts across multiple subsystems: backend adapters (`tierd/internal/tiering/{mdadm,zfsmgd}`), the SMART path (`tierd/internal/smart`), the FUSE namespace daemon (`src/fuse-ns/`), the kernel mount options the appliance picks, and the tierd scheduler. The work has to land in stages, not as one drop.

---

## Goals

1. Operator-facing per-disk spindown timer with a clear UI knob.
2. Defined "wake events" — an explicit list of what is and is not allowed to spin a HDD up.
3. Metadata-on-SSD invariant for every supported pool layout: read paths that need only metadata never hit a spundown HDD.
4. Silent-waker mitigations enumerated and implemented: SMART, atime, scrubs, ZFS background work, periodic syncs, heat sampling, reconcile walkers.
5. Observable spindown state: per-disk standby/active reporting in the UI, wake-event attribution in logs.
6. Pool-scoped policy: spindown is opt-in per pool and per disk, not a global toggle.

---

## Non-goals

- Changing the on-disk layout of existing pools to make them spindown-friendly. Conversion is a separate operator step.
- Spinning down SSDs or NVMe. Out of scope; SSDs do not benefit from standby.
- Battery-backed write caches or hardware write-back controllers. Not in the SmoothNAS stack.
- Replacing operator scrubs or SMART monitoring. Both must continue to run; this proposal makes them spindown-aware.
- Sub-second spindown. Practical timers are minutes; we do not chase the fast-cycle case.

---

## Wake-Event Contract

These are the events that can touch an HDD in steady state. The contract assigns each to one of three categories:

- **Allowed wake** — the HDD spins up because the access genuinely needs HDD bytes.
- **Cached / deferred** — served from SSD or RAM, never reaches the HDD.
- **Suppressed in standby** — the operation does not run while the disk is in standby; runs at the next active window.

| Event | Category | Mechanism |
|---|---|---|
| Client `read` against a file whose data lives on the HDD | Allowed wake | Data path |
| Client `write` against a file on the HDD | Allowed wake (or staged) | Data path; Phase 3 stages writes through SSD |
| Client `stat` / `getattr` | Cached | Metadata served from SSD-pinned record + RAM cache |
| Client `readdir` | Cached | Directory snapshot served from SSD or RAM cache |
| Client `lookup` (path traversal) | Cached | Same as `stat`; walked from SSD-pinned dir entries |
| Heat sample | Cached | Heat is collected at the cache / FUSE / smoothfs layer, not by polling HDD inodes |
| atime update on read | Suppressed | `noatime` on the mount |
| `mtime` / `ctime` update on metadata-only change | Cached | Metadata write goes to the SSD-pinned record |
| SMART attribute poll | Suppressed in standby | `smartctl -n standby` skip flag; opportunistic poll on next wake |
| `mdadm --action=check` / scrub | Suppressed in standby | Scheduled scrub windows, not free-running |
| ZFS scrub | Suppressed in standby | Operator-scheduled scrub window |
| ZFS TXG sync (no dirty data on the HDD vdev) | Cached | TXG sync skips clean vdevs by ZFS design; verified by inspection |
| ZFS DDT lookup on dedup pool | Forbidden by configuration | Dedup not allowed on spindown pools, or DDT must live on a `special` vdev |
| Page-cache writeback for dirty pages targeting the HDD | Allowed wake (or staged) | Phase 3 introduces optional SSD write-staging |
| Tierd reconcile walk of the HDD backing tree | Suppressed in standby | Reconcile schedules itself to active windows |
| Operator browse via SMB / NFS / iSCSI | Cached | Same metadata-on-SSD invariant covers exported namespaces |

Anything not in this table that wakes a disk in steady state is a bug under this contract.

---

## Metadata-on-SSD Invariant

This is the load-bearing precondition. Every supported pool layout must answer how it satisfies it.

### File-level paths (smoothfs and the existing FUSE adapters)

- **smoothfs** (`docs/proposals/done/smoothfs-stacked-tiering.md`): the persistent metadata area lives on the fastest tier by design. Phase 1 of smoothfs already pins the placement record to the fastest lower; this proposal additionally requires that smoothfs's xattr / ACL caches and directory-shadow records (per the Directories section of smoothfs's POSIX Semantics) live on the SSD tier.
- **Existing zfs-managed adapter** (`tierd/internal/tiering/zfsmgd/`): metadata lives in a tier-local `.tierd-meta/` under each tier's backing dataset. To satisfy the invariant, the metadata for files whose data lives on the HDD must be promoted to the SSD tier's `.tierd-meta/`. This is a one-time migration plus a small change to `tierd/internal/tiering/meta/store.go`'s tier resolution: writes for HDD-resident files target the fastest tier's shard.
- **Existing mdadm adapter via FUSE** (`tierd/internal/tiering/mdadm/`): same model as zfs-managed; metadata is per-tier today. The same promotion applies.

### Block-level path (mdadm heat engine, LVM PE / region tiering)

The mdadm heat engine moves PE-sized regions, not files. The lower filesystem (XFS-on-LV) lays out its own metadata however it likes — a freshly-grown XFS scatters inode and AG-header blocks across the device. To satisfy the invariant on this path:

- **XFS metadata-region pinning.** When a managed volume is created on a mdadm-tiered pool, all XFS metadata regions (allocation groups, inode chunks, log) must be allocated on PEs that map to the fastest tier and held there by policy. This is a new responsibility for `tierd/internal/tiering/mdadm/placement.go`.
- **Allocation policy.** New file metadata writes must extend metadata regions on the fastest tier; data extents may land per the existing heat policy.
- **Migration policy.** The mdadm heat engine's region-migration path must never demote a metadata region to a slower tier.

### Raw ZFS (no smoothfs, no FUSE adapter)

Raw zpool layouts are operator-built and may not meet the invariant. For raw ZFS pools to qualify for spindown:

- The pool must include a **`special` vdev** on SSD that absorbs metadata and small-block I/O.
- Optional but recommended: **L2ARC** on SSD for hot read caching.
- The pool must not enable dedup (or its DDT must live on the `special` vdev).
- ARC must be sized so metadata stays resident across the spindown window.

Raw ZFS pools without a `special` vdev are explicitly **not eligible** for spindown; the UI rejects enabling spindown and explains the requirement.

---

## Silent-Waker Mitigations

Each item below is a known way an HDD wakes when no client asked it to. The proposal must address every one.

### SMART

- All SMART polling sites in `tierd/internal/smart` use `smartctl -n standby` so the probe returns immediately if the drive is in standby. Configurable per disk, but the default is on for any spindown-eligible disk.
- `smartd`-style daemon polling on managed disks runs through this same path.
- Long self-tests (`smartctl -t long`) explicitly wake the disk and run during operator-defined active windows.

### atime

- Pool-managed mounts default to `noatime` for spindown-eligible pools. `relatime` is disallowed because relatime still updates atime once a day, which wakes the disk.
- `lazytime` is acceptable only when paired with metadata-on-SSD, because lazytime defers the atime write but still eventually flushes it to the inode block.

### mdadm scrub

- The `mdadm --action=check` cron currently runs on a free schedule. On spindown-enabled pools it runs only during operator-defined active windows.
- Mid-scrub, the disk is by definition active; spindown timers reset on scrub completion.

### ZFS

- `zpool scrub` runs on operator-defined windows (the existing weekly default applies but must respect spindown).
- `zfs_txg_timeout` and dirty-data thresholds are tuned so a clean HDD vdev does not see TXG writes. Verified by inspection of vdev I/O counters during idle.
- `zfs_arc_max` sized so metadata stays resident in ARC across the spindown window.
- Dedup is forbidden on spindown pools (or its DDT must live on a `special` vdev).

### Page cache and writeback

- Dirty pages targeting an HDD-only file are flushed by `dirty_writeback_centisecs` / `dirty_expire_centisecs`. A pure write-through model wakes the disk per write batch.
- Phase 3 introduces an optional **SSD write-staging tier**: writes land on SSD and drain to HDD during active windows or under fullness pressure. This decouples write activity from HDD wake events. Until Phase 3, a write to an HDD-resident file wakes the disk and the spindown timer restarts.

### Heat sampling

- Heat must be collected from the cache / FUSE / smoothfs hot path, not by walking HDD inodes.
- The reconcile walker in `tierd/internal/tiering/meta/reconcile.go` honours an "active windows only" mode for spindown-enabled pools.

### Tierd background work

- Reconcile, scheduler purges, and any other periodic walker that could touch the HDD backing tree must check spindown state before walking and defer to the next active window if the disk is in standby.

### Operator browse traffic

- The metadata-on-SSD invariant covers SMB / NFS browse: a client `ls` of a deep tree resolves entirely from the SSD-pinned metadata + RAM cache. iSCSI is not a browse path; iSCSI LUN reads always wake the backing disk.

---

## Spindown Mechanism

- Per-disk idle timer driven by `hdparm -S <units>` programmed at appliance boot and on disk hot-add.
- Drive-firmware idle timers (set via `hdparm -B` APM levels) are explicitly disabled on managed disks; a single authority avoids the kernel and firmware racing each other.
- Manual spindown via `hdparm -y` exposed as an admin action.
- Standby state queried via `hdparm -C` or the kernel's `/sys/block/<dev>/device/state`. Polled sparingly; the poll itself uses the standby-aware path.
- All spindown control flows through `tierd/internal/disk` — there is no other writer to `hdparm` settings on a managed disk.

---

## Pool and Disk Eligibility

A disk is **spindown-eligible** if all of the following hold:

- It is rotational (`/sys/block/<dev>/queue/rotational == 1`).
- It is part of a tier whose pool has spindown enabled.
- Its containing pool layout satisfies the metadata-on-SSD invariant for its backend kind.
- Its SMART status is healthy. Failing or pending-failed disks are not allowed to spin down — operator visibility on a failing disk matters more than power saving.

A pool is **spindown-eligible** if all of the following hold:

- It contains at least one fastest-tier SSD or NVMe target.
- All metadata for files on slower tiers can be pinned to the fastest tier (Metadata-on-SSD Invariant).
- All silent wakers above have been mitigated for the pool's backend kind.

The UI surfaces both per-pool and per-disk eligibility, with a specific reason when ineligible.

---

## Observability

- Per-disk **state** in the UI: `active`, `standby`, `ineligible`, `manual-hold`.
- Per-disk **uptime delta**: time since last spin-up, spin cycles within an operator-chosen window.
- **Wake-event log**: every spin-up records who woke the disk (data read on inode X, scrub, manual override, unknown). "Unknown" is a bug to investigate.
- Pool-level **time-in-standby percentage** over operator-chosen windows.
- Debug CLI: `tierd-cli spindown trace <disk>` streams wake events live.

---

## Phased Delivery

### Phase 1 — Contract, UI, and per-disk timer

Scope:

- Wake-Event Contract finalised and documented.
- `tierd/internal/disk` gains spindown control: `hdparm -S`, `hdparm -y`, `hdparm -C`, eligibility evaluation, observability surface.
- UI: per-pool spindown enable, per-disk timer setting, eligibility status, basic state display.
- SMART poll path converted to use `smartctl -n standby`.
- `noatime` enforced on all spindown-enabled pool mounts.
- Spindown does not yet require the metadata-on-SSD invariant — Phase 1 ships with a conservative warning that spindown will not be effective until Phase 2 completes for the relevant backend.

### Phase 2 — Metadata-on-SSD invariant for the existing backends

Scope:

- mdadm adapter: XFS metadata regions are allocated on the fastest tier when a managed volume is created on a spindown-enabled pool. Reconcile detects and reports volumes that violate this.
- mdadm heat engine: when migrating regions, never demote metadata regions to a slower tier.
- zfs-managed adapter: per-pool metadata for files whose data lives on the HDD is promoted to the fastest tier's `.tierd-meta/`.
- Raw ZFS: eligibility check requires a `special` vdev; UI explains the requirement.
- mdadm scrub and ZFS scrub schedulers gain "active windows only" mode for spindown pools.
- Tierd reconcile and scheduler walkers honour active windows for spindown-enabled pools.

### Phase 3 — SSD write-staging (optional but high-value)

Scope:

- Optional per-pool SSD write-staging tier that absorbs writes targeted at HDD-resident files.
- Drain to HDD during active windows or under fullness pressure.
- UI exposes the staging tier as a first-class pool component with its own fill/threshold policy.
- Verified by an end-to-end test: a write workload on a spindown pool keeps the HDD in standby for an operator-chosen window.

### Phase 4 — smoothfs integration

Scope:

- smoothfs Phase 1 and Phase 2 already pin the placement record to the fastest tier; this phase wires smoothfs into the spindown contract.
- Heat sampling for smoothfs pools is collected from kernel-side smoothfs events, never by walking HDD inodes.
- Snapshot quiesce (`SMOOTHFS_CMD_QUIESCE` from the smoothfs proposal) is plumbed into the active-window scheduler so snapshots run during active windows for spindown pools.
- Once smoothfs Phase 3 lands, the file-level mdadm and zfs-managed adapters cut over to smoothfs and Phase 2's adapter-specific metadata pinning becomes redundant.

---

## Relationship to In-Flight Work

| Proposal / Feature | Relationship to spindown |
|---|---|
| `smoothfs-stacked-tiering.md` | Phase 4 wires smoothfs into the spindown contract. smoothfs's metadata-on-fastest-tier design satisfies the Metadata-on-SSD Invariant for free; smoothfs's snapshot quiesce hook is reused for active-window scheduling. |
| `fuse-ns-create-fast-path.md` | Orthogonal. The fast-path improves CREATE latency on the existing FUSE stack; it neither helps nor hurts spindown. |
| `unified-tiering-04b-zfs-managed-adapter.md` | Phase 2 adds metadata-promotion logic to this adapter. No semantic divergence. |
| `unified-tiering-06-coordinated-snapshots.md` | Coordinated snapshots run during active windows on spindown pools. Quiesce hook reused. |
| `mdadm-heat-engine-*` (done + pending UI) | Phase 2 adds the metadata-region pinning rule and the "no metadata demotion" rule to the mdadm heat engine's migration policy. |

---

## Risks

| Risk | Why it matters | Mitigation |
|---|---|---|
| Silent waker missed in the contract | Disks never reach standby; operator believes feature is broken | Wake-event log with explicit attribution; "unknown" wakes are bugs to investigate |
| Drive firmware fights kernel timer | Drives spin up and down erratically | Explicit `hdparm -B` disable of drive APM on managed disks; single authority |
| XFS metadata pinning interferes with allocator | Allocator falls over on tier exhaustion | Reserve a budgeted metadata fraction on the fastest tier; allocator falls back with explicit log line and pool exits spindown eligibility |
| ZFS without `special` vdev | Operator enables spindown on a raw ZFS pool that physically cannot meet the invariant | UI eligibility check rejects with a clear remediation step |
| Page-cache writeback wakes disk on every dirty-window flush | Spindown achieves nothing under any write workload | Phase 3 SSD write-staging; until then, document that write workloads keep HDDs spinning |
| Spindown delays a write under unusual concurrency | A latency-sensitive write sees an extra spin-up | Document expected wake latency; provide `manual-hold` mode for workloads that cannot tolerate it |
| SMART skipped too often misses a failing drive | A predictive failure goes undetected | Long self-tests scheduled into active windows; eligibility check pulls a disk out of spindown as soon as SMART status degrades |
| Spindown cycles shorten drive life | Excessive Load_Cycle_Count from aggressive timers | UI surfaces per-disk cycle counts; default timer chosen conservatively; operator-tunable per disk |

---

## Completion Notes

- `tierd/internal/disk` owns hdparm standby state, timer programming, APM disable, manual standby, eligibility, and unit-tested timer/state parsing.
- `/api/disks/{disk}/power` reports state and configures timers; `/api/disks/{disk}/standby` performs manual standby.
- The Disks page shows power state, eligibility, configured timer, timer controls, and manual standby.
- SMART inventory and self-test history reads now use `smartctl -n standby --all --json` so standby disks are not spun up for polling.
- Remaining phases are tracked in `docs/proposals/pending/disk-spindown-06-smoothfs-write-staging-dataplane.md`.
