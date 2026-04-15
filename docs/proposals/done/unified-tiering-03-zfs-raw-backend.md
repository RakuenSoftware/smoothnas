# Proposal: Unified Tiering — 03: ZFS Raw Backend

**Status:** Pending
**Date:** 2026-04-09
**Updated:** 2026-04-12
**Supersedes:** zfs-backend (earlier draft)
**Depends on:** base appliance (tierd service, web UI shell, disk inventory)
**Part of:** unified-tiering-control-plane
**Preceded by:** unified-tiering-02-mdadm-adapter
**Followed by:** unified-tiering-04-zfs-managed-adapter

---

## Problem

A large class of users needs copy-on-write snapshots, integrated checksumming, native compression, and a managed pool/dataset lifecycle. Without a ZFS backend, they have no managed path and must configure OpenZFS manually through the shell. Raw ZFS must ship as a first-class storage path before the managed tiering adapter (proposal 04) can build on top of it.

---

## Goals

1. Pool creation and destruction, including all standard vdev types.
2. SLOG and L2ARC device management per pool.
3. Dataset lifecycle: create, configure, mount, unmount, destroy.
4. Zvol lifecycle: create, resize, destroy.
5. Snapshot lifecycle: create, list, rollback, clone, send to file.
6. Health monitoring: pool state, resilver progress, checksum errors, L2ARC hit rate, ARC pressure.
7. Scrub scheduling per pool.
8. Disk replacement workflow with resilver visibility.
9. ARC and TXG tuning controls.

---

## Non-goals

- ZFS deduplication
- ZFS native encryption
- Scheduled send/recv replication to remote hosts (manual send-to-file is in scope)
- Sharing disks between the ZFS and mdadm backends
- Cross-host or clustered ZFS
- Any managed tiering over ZFS datasets (covered by proposal 04)

---

## Architecture

```text
ZFS datasets and zvols
  -> zpool
     -> data vdevs
     -> optional SLOG
     -> optional L2ARC
```

### Supported vdev types

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

### Write-path behavior

```text
Async write:
  application -> ARC / in-memory TXG -> data vdevs on TXG commit

Sync write:
  application -> SLOG immediately -> data vdevs on TXG commit
```

Important product constraints:

- SLOG accelerates synchronous writes only, not general write caching.
- L2ARC is a read cache only; losing an L2ARC device loses no data.
- Deduplication and native encryption are out of scope.

---

## Packages

| Package | Purpose |
| --- | --- |
| `zfsutils-linux` | Pool, dataset, zvol, and snapshot management |
| `zfs-dkms` | OpenZFS kernel module |

---

## Appliance Tuning Defaults

| Parameter | Value | Rationale |
| --- | --- | --- |
| `zfs_arc_max` (ZFS only) | 75% of RAM | Maximize ARC on a storage appliance |
| `zfs_arc_max` (ZFS + mdadm) | 50% of RAM | Leave headroom for page cache and mdadm path |
| `zfs_arc_min` | 25% of RAM | Prevent excessive ARC collapse |
| `zfs_txg_timeout` | 5 seconds | Default throughput/latency trade-off |

---

## API

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/pools` | GET | List pools with health, size, free space, fragmentation, and vdev layout |
| `/api/pools` | POST | Create a pool with data vdevs and optional SLOG/L2ARC |
| `/api/pools/import` | POST | Import an existing pool by name or device path (`zpool import`) |
| `/api/pools/import` | GET | List importable pools discovered on attached devices (`zpool import -d /dev`) |

Note: `import` is a reserved pool name. `POST /api/pools` rejects a pool name of `import` with HTTP 422 (`pool name 'import' is reserved`). This prevents the `/api/pools/import` route from shadowing a real pool.

| `/api/pools/{name}` | GET | Detailed pool status, scan state, and errors |
| `/api/pools/{name}` | DELETE | Destroy a pool after confirmation |
| `/api/pools/{name}/vdevs` | POST | Add a data vdev |
| `/api/pools/{name}/vdevs/{vdev_id}` | DELETE | Remove a removable vdev (mirror or log device; requires OpenZFS 2.0+). Data vdevs of type `single` or `raidz*` cannot be removed; attempting to remove them returns HTTP 422 with a descriptive error. The `vdev_id` is the ZFS-native GUID as reported in the `GET /api/pools/{name}` response. The pool detail response must include a `vdevs` array where each vdev has a `guid` field (the uint64 GUID as a decimal string) and a `type` field. The UI uses this GUID as the `vdev_id` in DELETE requests. Attempting to use a name or index instead of a GUID returns HTTP 400. |
| `/api/pools/{name}/slog` | POST | Add or replace SLOG devices |
| `/api/pools/{name}/slog` | DELETE | Remove SLOG devices |
| `/api/pools/{name}/l2arc` | POST | Add L2ARC devices |
| `/api/pools/{name}/l2arc` | DELETE | Remove L2ARC devices |
| `/api/pools/{name}/scrub` | POST | Start a scrub |
| `/api/pools/{name}/scrub/schedule` | GET | Get the scrub schedule for a pool |
| `/api/pools/{name}/scrub/schedule` | PUT | Set or update the scrub schedule (cron expression) |
| `/api/pools/{name}/scrub/schedule` | DELETE | Remove the scrub schedule |
| `/api/pools/{name}/disks/{disk}/replace` | POST | Replace a failed disk |
| `/api/datasets` | GET | List datasets with usage, quota, reservation, compression, and mountpoint |
| `/api/datasets` | POST | Create a dataset |
| `/api/datasets/{id}` | GET | Dataset detail |
| `/api/datasets/{id}` | PUT | Update quota, reservation, compression, and mountpoint |
| `/api/datasets/{id}` | DELETE | Destroy a dataset |
| `/api/datasets/{id}/mount` | POST | Mount a dataset |
| `/api/datasets/{id}/unmount` | POST | Unmount a dataset |
| `/api/zvols` | GET | List zvols |
| `/api/zvols` | POST | Create a zvol |
| `/api/zvols/{id}` | GET | Zvol detail |
| `/api/zvols/{id}` | DELETE | Destroy a zvol |
| `/api/zvols/{id}/resize` | PUT | Resize a zvol |
| `/api/snapshots` | GET | List snapshots |
| `/api/datasets/{id}/snapshots` | GET | List snapshots for a specific dataset |
| `/api/snapshots` | POST | Create a snapshot |
| `/api/snapshots/{id}` | DELETE | Destroy a snapshot |
| `/api/snapshots/{id}/rollback` | POST | Roll back to a snapshot |
| `/api/snapshots/{id}/clone` | POST | Clone a snapshot |
| `/api/snapshots/{id}/send` | POST | Send snapshot to a file |
| `/api/snapshots/exports` | GET | List snapshot export files in the export directory (name, size, created_at) |
| `/api/snapshots/exports/{filename}` | DELETE | Delete a snapshot export file |

### Snapshot Send Constraints

`POST /api/snapshots/{id}/send` writes a raw ZFS send stream to a server-side path. The destination path must be:
- An absolute path under a configurable `tierd`-owned export directory (default `/var/lib/tierd/exports/`).
- A filename, not a directory traversal (no `..` components).

The stream format is a raw ZFS send stream (`zfs send -R`). Incremental sends (`-i`) are not supported in this phase. The file can be restored with `zfs recv` on any compatible ZFS host.

tierd validates the path before writing. Requests with paths outside the export directory return HTTP 400.

### Snapshot Export Management

Snapshot export files accumulate in the export directory. To prevent unmanaged disk growth:
- `GET /api/snapshots/exports` lists all files in the configured export directory with name, size in bytes, and created_at timestamp.
- `DELETE /api/snapshots/exports/{filename}` removes a specific export file. The filename must not contain path separators; requests with `..` or `/` components return HTTP 400.
- tierd does not automatically purge export files. Operators are responsible for deleting exports when no longer needed.
- The available space on the filesystem containing the export directory is reported in `GET /api/system/status` as `export_free_bytes`.

### Scrub Scheduling

Pools have an optional scrub schedule stored in the `pool_scrub_schedules` table (`pool_name`, `cron_expression`, `last_run_at`, `next_run_at`). The monitor goroutine checks pending scrubs on each poll cycle and triggers `zpool scrub` when due. The default schedule for newly created pools is weekly (Sunday 02:00 local time). Operators may change or remove the schedule per pool.

### Zvol Resize

Zvol resize is one-directional: the API only accepts size increases. A resize request that specifies a size smaller than the current size returns HTTP 422. This prevents accidental block-device truncation.

### ARC Tuning Application

ARC parameters are written to `/etc/modprobe.d/zfs.conf` and take effect on the next kernel module load. Because the module is typically loaded at boot, changes require a reboot to take full effect. When a first ZFS pool is created on a system that also has mdadm tiers, tierd writes the 50%/25% configuration and logs a notice that a reboot is required for the new ARC limits to take effect. tierd does not reboot the system automatically. The reboot notice is surfaced in the API: `GET /api/system/status` includes an `arc_reboot_required` boolean field, set to `true` when ARC parameters have been written to `/etc/modprobe.d/zfs.conf` but the running kernel module has not yet loaded those values. The UI displays a persistent banner: 'ARC tuning changes require a reboot to take full effect.' The banner is dismissed once the system reboots and the loaded ARC values match the configured values.

The 50%/25% (mixed ZFS + mdadm) ARC configuration must also be applied when an mdadm tier is created on a system that already has a ZFS pool. tierd checks the coexistence condition at pool-create time and at mdadm tier-create time, and writes the appropriate `/etc/modprobe.d/zfs.conf` in either direction.

System surfaces also gain:

- ZFS pool state and SLOG/L2ARC presence in `/api/system/status`
- ARC and TXG tuning in `/api/system/tuning`
- ZFS disk assignment values in `/api/disks`

---

## Health Monitoring

| Check | Source | Alert condition |
| --- | --- | --- |
| Pool health | `zpool status` | degraded, faulted, or resilvering |
| Resilver progress | `zpool status` | active resilver |
| Checksum errors | `zpool status` | any non-zero checksum error count |
| Dataset usage | `zfs list` | usage above configured threshold |
| L2ARC hit rate | `/proc/spl/kstat/zfs/arcstats` | cache is ineffective |
| ARC pressure | `/proc/spl/kstat/zfs/arcstats` | ARC pushed below intended floor |

---

## Disk Replacement Workflow

1. Operator sees a degraded-pool alert.
2. Operator identifies the failed disk in the vdev tree.
3. Operator physically replaces the disk.
4. Operator triggers replace from the UI.
5. `tierd` runs `zpool replace`.
6. Resilver progress is reported in the UI.

---

## UI

The backend-specific UI includes:

- `Pools` — pool creation, vdev layout, SLOG, L2ARC, scrub, disk replacement
- `Datasets` — dataset lifecycle, quota, reservation, compression, mount
- `Zvols` — zvol lifecycle, resize
- `Snapshots` — snapshot lifecycle, rollback, clone, send

Dashboard and Settings also gain:

- pool health widget
- resilver progress visibility
- L2ARC hit-rate visibility
- ARC and TXG tuning controls

Raw ZFS datasets do not appear in the unified tiering GUI. They are managed exclusively through these backend-specific pages.

---

## Effort

**M** — well-scoped pool/dataset/zvol/snapshot management; no novel subsystems.

---

## Acceptance Criteria

- [ ] Pool creation supports all vdev types in the table above.
- [ ] SLOG and L2ARC devices can be added and removed per pool.
- [ ] Dataset lifecycle (create, configure, mount, unmount, destroy) works end to end.
- [ ] Zvol lifecycle (create, resize, destroy) works end to end.
- [ ] Snapshot lifecycle (create, rollback, clone, send) works end to end.
- [ ] Health monitoring emits alerts for degraded, faulted, resilvering, checksum errors, L2ARC ineffectiveness, and ARC pressure.
- [ ] Disk replacement triggers `zpool replace` and surfaces resilver progress.
- [ ] ARC and TXG tuning controls write to the appropriate kernel parameters.
- [ ] Raw ZFS datasets do not appear in `/api/tiering/targets` or `/api/tiering/namespaces`.
- [ ] Pool state and SLOG/L2ARC presence appear in `/api/system/status`.
- [ ] `GET /api/pools/import` lists importable pools.
- [ ] `POST /api/pools/import` imports an existing pool.
- [ ] Newly created pools receive a default weekly scrub schedule.
- [ ] Scrub schedule can be updated and removed via the API.
- [ ] The monitor triggers `zpool scrub` according to the schedule.
- [ ] ARC tuning parameters are written to `/etc/modprobe.d/zfs.conf` on first pool creation; a reboot notice is surfaced in the API.
- [ ] Zvol shrink requests return HTTP 422.
- [ ] `DELETE /api/pools/{name}/vdevs/{vdev_id}` removes removable vdevs and returns HTTP 422 for non-removable types.
- [ ] `GET /api/pools/{name}` response includes a `vdevs` array with `guid` and `type` per vdev.
- [ ] Pool names `import` and any other ZFS-reserved names are rejected at create time with HTTP 422.
- [ ] `GET /api/datasets/{id}/snapshots` lists snapshots scoped to the specified dataset.
- [ ] `GET /api/snapshots/exports` lists export files with name, size, and created_at.
- [ ] `DELETE /api/snapshots/exports/{filename}` removes an export file; path traversal returns HTTP 400.
- [ ] `GET /api/system/status` includes `arc_reboot_required` boolean.
- [ ] Creating an mdadm tier on a ZFS-only system triggers re-evaluation of ARC limits.

## Test Plan

- [ ] Integration tests for pool creation and destruction with each supported vdev type.
- [ ] Integration tests for SLOG and L2ARC add/remove workflows.
- [ ] Integration tests for dataset lifecycle including quota and compression.
- [ ] Integration tests for zvol lifecycle including resize.
- [ ] Integration tests for snapshot lifecycle: create, rollback, clone, send-to-file.
- [ ] Integration tests for disk replacement and resilver visibility.
- [ ] Integration test that no raw ZFS dataset appears in the unified tiering inventory.
- [ ] Integration test for ARC and TXG tuning parameter application.
- [ ] Unit tests for health monitoring alert conditions from `zpool status` and arcstats.
- [ ] Integration test: `DELETE /api/pools/{name}/vdevs/{vdev_id}` with a valid GUID succeeds; with a non-GUID returns HTTP 400.
- [ ] Integration test: `POST /api/pools` with name `import` returns HTTP 422.
- [ ] Integration test: `GET /api/datasets/{id}/snapshots` returns only snapshots for that dataset.
- [ ] Integration test: `GET /api/snapshots/exports` and `DELETE /api/snapshots/exports/{filename}` work correctly; path traversal returns HTTP 400.
- [ ] Integration test: `arc_reboot_required` is true after writing `/etc/modprobe.d/zfs.conf` and false after reboot.
