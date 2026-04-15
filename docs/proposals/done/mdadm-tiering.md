# Proposal: mdadm Storage Tiering

**Status:** Pending
**Date:** 2026-04-07

---

## Problem

There is no turnkey solution for building a Linux-based storage appliance with heat-driven SSD/HDD tiering using commodity hardware. Existing tools (mdadm, LVM) provide the building blocks, but assembling them into a tiered storage system requires manual command-line work, deep knowledge of the Linux storage stack, and ongoing maintenance through shell commands. There is no web UI that ties these layers into a coherent, managed workflow.

The previous mdadm backend modelled storage as a fixed dm-cache relationship — one SSD array as an opaque writeback cache in front of one HDD array. This is not tiering: data is duplicated, the SSD is never the authoritative location, and there is no concept of explicit tier placement or heat-driven migration. It also locks the system into exactly two arrays with a fixed cache topology.

---

## Goals

1. Three named storage tiers — **NVME**, **SSD**, and **HDD** — each optionally backed by one mdadm RAID array
2. New logical volumes placed on the fastest available tier by default, with automatic spillover to the next colder tier when a tier is full
3. Continuous per-block heat measurement at configurable region granularity: hot regions live on NVME, warm regions on SSD, cold regions on HDD
4. Automatic heat-driven extent migration between tiers via online `pvmove` targeting specific physical extent ranges — no unmounting required
5. A web UI for tier assignment, volume management, heat visibility, and policy configuration
6. A custom Debian 13 OS image (autoinstall ISO) that installs the full appliance onto bare metal

---

## Non-goals

- Per-file tiering within a volume. Tiering is at the block/extent layer, not the filesystem layer.
- ZFS. Covered in a separate proposal.
- NFS/SMB/iSCSI target configuration. Covered in the sharing-protocols proposal.
- Cluster or multi-node storage. Single host only.
- dm-cache. Kept as a separate optional concern; not part of the tiering model.

---

## Architecture

### Tier model

Three tiers are fixed at the system level:

| Tier | Rank | Typical hardware | Heat band |
|------|------|-----------------|-----------|
| NVME | 1 | NVMe SSDs | Hot |
| SSD | 2 | SATA/SAS SSDs | Warm |
| HDD | 3 | Spinning disks | Cold |

Each tier is optional. A tier with no backing array is inactive and unavailable for placement. An active tier is backed by exactly one mdadm RAID array; the relationship is one-to-one.

With only two tiers populated (e.g. SSD + HDD), the heat model collapses to hot/cold. With all three, it is hot/warm/cold.

### Storage model

All tier arrays share a single LVM volume group (`smoothnas`). Each array is added as a PV in that VG and tagged with its tier identity (`tier:nvme`, `tier:ssd`, `tier:hdd`). PV tags are metadata; placement enforcement is at the `tierd` call site via explicit PV device paths passed to `lvcreate` and `lvextend`.

```
Tier NVME (rank 1) → /dev/md0 → PV tag "tier:nvme" → VG "smoothnas"
Tier SSD  (rank 2) → /dev/md1 → PV tag "tier:ssd"  → VG "smoothnas"
Tier HDD  (rank 3) → /dev/md2 → PV tag "tier:hdd"  → VG "smoothnas"
```

`tierd` resolves tier name to PV device path at LV creation and extension time, and passes the resolved path as a positional argument to `lvcreate`/`lvextend`. It records the selected tier for each LV in the database. Tier-changing operations that bypass the migration workflow are rejected.

### Extent placement

An LV is not assigned to a single tier. Its physical extents are distributed across tier PVs based on heat. A single LV can have some extents on NVMe (hot regions), some on SSD (warm regions), and some on HDD (cold regions) simultaneously.

**New extent allocation:** When an LV is created or extended, new extents are allocated on the fastest active tier with available capacity (NVME → SSD → HDD). If the fastest tier is at or above its `full_threshold` (default 90%), allocation falls to the next colder active tier — cascade order NVME → SSD → HDD. If all tiers are full, creation fails with a clear capacity error.

### Supported RAID levels

All standard mdadm RAID levels are supported on any tier:

| RAID Level | Min Disks | Description |
|------------|-----------|-------------|
| RAID-0 | 2 | Striping. No redundancy. |
| RAID-1 | 2 | Mirror. |
| RAID-4 | 3 | Dedicated parity disk. |
| RAID-5 | 3 | Distributed parity (1-disk fault tolerance). |
| RAID-6 | 4 | Distributed double parity (2-disk fault tolerance). |
| RAID-10 | 4 | Striped mirrors. |
| LINEAR | 2 | Concatenation. No redundancy. |

Single-disk configurations are also supported. Tiers do not need to use matching RAID levels.

---

## Heat Measurement

`tierd` enables dm-stats on each active LV's device-mapper target at startup and when new LVs are created. The LV is divided into fixed-size **regions** (default: 256 MB, configurable). dm-stats tracks read and write IOPS independently per region.

On each poll interval (default: 5 minutes), `tierd` reads cumulative per-region IOPS from dm-stats and maintains a rolling window average per region (default: 24 hours). This rolling average is the region's **heat score**.

The region size should be a multiple of the LVM physical extent size. Smaller regions give finer-grained migration at the cost of more metadata and more frequent migrations. Larger regions reduce migration churn but may carry cold blocks along with hot ones.

### Tier bands

Two configurable IOPS thresholds divide the heat scale into three bands, applied per region:

- Heat ≥ `nvme_threshold` → region belongs on NVME (hot)
- Heat ≥ `ssd_threshold` and < `nvme_threshold` → region belongs on SSD (warm)
- Heat < `ssd_threshold` → region belongs on HDD (cold)

The `ssd_threshold` must always be strictly less than `nvme_threshold`. `tierd` rejects configuration that violates this.

With only two active tiers, only the relevant threshold applies (e.g. with SSD + HDD, only `ssd_threshold` is used).

---

## Policy Engine

The policy engine runs on a configurable evaluation interval (default: 30 minutes). On each cycle it:

1. For each region of each unpinned LV, determines which tier band its current heat score falls in.
2. If the region's target tier is faster than its current PV tier, queues a **promotion** for that region directly to the target tier. A region on HDD with hot-level heat migrates straight to NVME without passing through SSD first.
3. If the region's target tier is slower than its current PV tier, queues a **demotion** of exactly one tier step (NVME→SSD or SSD→HDD). Demotion is gradual to avoid over-reacting to transient cold periods.
4. A region must be in the wrong-tier state for N consecutive evaluation cycles before migration is queued (default: 3 cycles). This prevents thrashing on transient load spikes.

Migrations queued by the policy engine are marked `triggered_by: policy`.

### Pinning

A `pinned` flag on an LV excludes all of its regions from policy evaluation. Pinned LVs are never promoted or demoted automatically. Pin state is togglable per LV in the UI.

---

## Migration

### Mechanism

All tier migrations use `pvmove` targeting specific physical extent (PE) ranges. `tierd` maps each heat region to the LVM PEs that back it, then issues `pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>` to move only those extents online, without unmounting the filesystem or interrupting I/O. This is the only migration path — there is no manual extent copy or dm suspend/resume approach.

The region-to-PE mapping is derived from `lvs -o seg_pe_ranges` and updated whenever an LV is extended or resized.

### State machine

`tierd` tracks each migration as a state machine:

```
idle → queued → migrating → verifying → complete
                    ↓                       ↑
               cancelling ────────────────→ (aborted)
                    ↓
                 failed
```

Steps:

1. Validate destination tier has free capacity above reserve (default: 10% of destination tier capacity).
2. Mark the region `migrating` in the database.
3. Resolve the region's PEs from `lvs -o seg_pe_ranges`.
4. Run `pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>` with an I/O rate cap applied.
5. Monitor `pvmove` progress; update `bytes_moved` / `bytes_total` in the database.
6. On completion, verify the region's PEs are now on the destination tier's PV.
7. Update the region's `current_tier`; mark `complete` or `failed`.

Only one migration runs at a time per host. Queued migrations wait.

### Throttling

`pvmove` is run with a configurable MB/s cap. `tierd` monitors overall host I/O utilisation and pauses or slows migration when the host exceeds a high-water mark.

### Crash recovery

On startup, `tierd` reconciles DB migration state against actual PE placement via `lvs -o seg_pe_ranges` and `pvs`. If a region is recorded as `migrating` but its PE placement is inconsistent with both source and destination tiers, `tierd` surfaces the discrepancy in the UI as a recoverable error. The operator can re-queue or manually mark it complete.

---

## OS Image

### Base

Debian 13 (Trixie) minimal server, built as a standard autoinstall ISO.

### Install-time disk configuration

The OS root filesystem lives on a separate disk or disks from the managed storage. During installation, the user can:

- Select which disk(s) to use for the OS
- Optionally configure RAID-1 (mdadm mirror) for the OS disks
- The installer partitions OS disk(s) with: EFI system partition, `/boot`, and an LVM VG for `/` and swap

Disks not selected for the OS are left unpartitioned for `tierd` to manage.

### Packages

| Package | Purpose |
|---------|---------|
| `mdadm` | Software RAID |
| `lvm2` | Logical volume management |
| `smartmontools` | Disk health monitoring (SMART) |
| `hdparm` / `nvme-cli` | Disk identification and tuning |
| `ipmitool` | Hardware health monitoring via IPMI/BMC (best-effort) |
| `lm-sensors` | CPU/board temperature and fan monitoring |
| `nginx` | Reverse proxy for the web UI |
| `tierd` (custom) | Go backend service |
| `tierd-ui` (custom) | Web frontend (static files served by nginx) |

### Service layout

| Service | Port | Description |
|---------|------|-------------|
| `nginx` | 443 (HTTPS) | Reverse proxy; serves the web frontend; proxies `/api/*` to tierd |
| `tierd` | 8420 (localhost) | Go backend API. Listens on localhost only. |

nginx terminates TLS with a self-signed certificate generated on first boot. The user can replace it through the UI.

---

## Web UI

### Frontend

Single-page Angular application served as static files by nginx. Communicates with the Go backend via REST API at `/api/`.

### Backend — `tierd`

Go service running as a systemd unit. Executes storage operations by invoking CLI tools as subprocesses (`mdadm`, `lvm`, `pvmove`, `smartctl`). Does not link against C libraries for storage management.

### Authentication

Local accounts in SQLite on the OS disk. Passwords hashed with bcrypt. Session tokens issued as HTTP-only secure cookies.

**First boot:** The installer generates a random admin password and displays it on the console. The user must change it on first login.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/auth/login` | POST | Authenticate, returns session cookie |
| `/api/auth/logout` | POST | Invalidate session |
| `/api/auth/password` | PUT | Change password for current user |
| `/api/users` | GET | List users (admin only) |
| `/api/users` | POST | Create user (admin only) |
| `/api/users/{id}` | DELETE | Delete user (admin only) |

### API: Disk inventory

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/disks` | GET | List all block devices with type (HDD/SSD/NVMe), size, model, serial, SMART status, and assignment (unassigned, os, mdadm-array) |
| `/api/disks/{id}/smart` | GET | Full SMART data: all attributes with current/worst/threshold values, raw value, flags, overall health, temperature, power-on hours, error log summary |
| `/api/disks/{id}/smart/history` | GET | SMART attribute history (hourly snapshots). Time-series data for trend charts. Filterable by attribute ID and time range. |
| `/api/disks/{id}/smart/test` | POST | Start an on-demand SMART self-test (short or extended) |
| `/api/disks/{id}/smart/test` | GET | List SMART self-test results (pass/fail, duration, LBA of first error) |
| `/api/disks/{id}/identify` | POST | Blink the disk's activity LED for physical identification |

### API: SMART alarms

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/smart/alarms` | GET | List alarm rules with current state (OK, warning, critical) per disk |
| `/api/smart/alarms` | POST | Create alarm rule (SMART attribute ID, warning/critical thresholds, scope: all disks or specific disk) |
| `/api/smart/alarms/{id}` | PUT | Update alarm rule |
| `/api/smart/alarms/{id}` | DELETE | Delete alarm rule |
| `/api/smart/alarms/history` | GET | Alarm event history. Filterable by disk, attribute, severity, time range. |

Default alarm rules (pre-configured, user-editable):

| SMART Attribute | Warning | Critical |
|-----------------|---------|----------|
| Reallocated Sector Count (5) | > 0 | > 50 |
| Current Pending Sector Count (197) | > 0 | > 10 |
| Offline Uncorrectable (198) | > 0 | > 5 |
| Reported Uncorrectable Errors (187) | > 0 | > 10 |
| Temperature (194) | > 50°C | > 60°C |
| Power-On Hours (9) | > 35000 | > 50000 |
| Wear Leveling Count (177, SSD) | < 20% | < 5% |
| Media Wearout Indicator (233, SSD) | < 20% | < 5% |

### API: Arrays

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/arrays` | GET | List mdadm arrays with state, RAID level, size, member disks, rebuild progress, and tier assignment |
| `/api/arrays` | POST | Create a new mdadm array (RAID level, member disks) |
| `/api/arrays/{id}` | GET | Detailed array status |
| `/api/arrays/{id}` | DELETE | Stop and destroy array (requires confirmation; array must not be assigned to a tier) |
| `/api/arrays/{id}/tier` | PUT | Assign array to a tier (NVME, SSD, or HDD); adds array as PV in shared VG with tier tag |
| `/api/arrays/{id}/tier` | DELETE | Remove tier assignment (only permitted when tier has no LVs) |
| `/api/arrays/{id}/disks` | POST | Add a disk to the array (grow) |
| `/api/arrays/{id}/disks/{disk}` | DELETE | Remove/fail a disk |
| `/api/arrays/{id}/disks/{disk}/replace` | POST | Replace a failed disk (triggers rebuild) |
| `/api/arrays/{id}/scrub` | POST | Start a parity check |

### API: Tiers

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/tiers` | GET | Status of all three tiers: active/inactive, backing array, capacity, used, LV count, current migration queue depth |
| `/api/tiers/policy` | GET | Current policy configuration (thresholds, evaluation interval, poll interval, rolling window, hysteresis cycles, I/O cap, reserve percentage) |
| `/api/tiers/policy` | PUT | Update policy configuration |

### API: Volumes

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/volumes` | GET | List LVs with size, filesystem, mount point, usage, pin state, and a per-tier capacity breakdown (how many bytes of this LV are on each tier) |
| `/api/volumes` | POST | Create LV (size, filesystem type, mount point, optional tier override) |
| `/api/volumes/{id}` | DELETE | Unmount and remove LV |
| `/api/volumes/{id}/resize` | PUT | Grow or shrink LV and filesystem |
| `/api/volumes/{id}/mount` | POST | Mount the volume |
| `/api/volumes/{id}/unmount` | POST | Unmount the volume |
| `/api/volumes/{id}/pin` | PUT | Pin LV to its current tier (exclude from policy engine) |
| `/api/volumes/{id}/pin` | DELETE | Remove pin; return LV to heat-based migration |

### API: Health

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/api/health` | GET | None | Service health: 200 if tierd is running and responsive, 503 if degraded. Returns uptime, API version, timestamp. |
| `/api/health/hardware` | GET | None | Hardware health: 200 if all checks pass, 503 if any degraded or critical. Per-component status below. |

Hardware health components:

| Component | Source | Degraded | Critical |
|-----------|--------|----------|----------|
| CPU | `/sys/class/thermal/` | Temperature approaching threshold | Throttling active |
| Memory | `/proc/meminfo` | Available < 10% | Available < 5% |
| Disks | `smartctl` | Reallocated or pending sectors > 0 | SMART health failed |
| Network | `ip link`, `/sys/class/net/` | Bond degraded | Primary interface down |
| PSU/IPMI | `ipmitool` (if available) | Warning thresholds | Critical thresholds or PSU failure |
| Fan/Thermal | `ipmitool` or `/sys/class/hwmon/` | Speed warning or elevated temps | Fan failure or thermal critical |

IPMI checks are best-effort; if unavailable, those components report `unknown` and do not affect overall status.

### API: System

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/system/status` | GET | Overall appliance status: array health, tier state, LV count, disk health summary |
| `/api/system/tuning` | GET | Current kernel tuning parameters (page cache) |
| `/api/system/tuning` | PUT | Update tuning parameters |
| `/api/system/tls` | PUT | Upload replacement TLS certificate and key |
| `/api/system/alerts` | GET | Active alerts: degraded arrays, SMART warnings, tier pressure, hardware alerts |

### Page cache tuning

| Parameter | Appliance default | Rationale |
|-----------|------------------|-----------|
| `vm.dirty_ratio` | 20% | Hard write-block threshold. Higher values risk longer stalls on crash. |
| `vm.dirty_background_ratio` | 10% | Background flush threshold. Balances write absorption with timely flushing. |
| `vm.dirty_expire_centisecs` | 3000 | 30s page age before eligible for writeback. Appropriate for a storage appliance. |

### UI pages

| Page | Description |
|------|-------------|
| **Dashboard** | Array health, tier state (capacity per tier, migration queue), LV count, active alerts |
| **Disks** | All physical disks: type, size, SMART status, array assignment, identify button |
| **SMART** | Per-disk attribute tables, trend charts (hourly history), alarm configuration, alarm history, on-demand self-test |
| **Arrays** | Create, grow, replace disk, scrub, destroy arrays. Tier assignment per array. |
| **Tiers** | Tier overview: which array backs each tier, capacity and usage per tier, LVs per tier, migration queue. Policy configuration panel. |
| **Volumes** | Create, resize, mount/unmount, destroy LVs. Per-LV: tier distribution (bytes on each tier), heat map (per-region heat scores and current tier), active migration progress, pin toggle. |
| **Settings** | Page cache tuning, TLS certificate, user management |

---

## Setup Workflow

1. **Login** — authenticate with the initial admin credentials displayed during install
2. **Change password** — forced on first login
3. **Identify disks** — review detected disks, blink LEDs for physical identification
4. **Create arrays** — create one or more mdadm arrays, assign each to a tier (NVME/SSD/HDD)
5. **Create volumes** — create LVs on the tiered VG; each goes to the fastest available tier by default

The user can create additional arrays and volumes at any time. There is no locked-in setup path.

---

## Ongoing Management

### Health monitoring

`tierd` polls storage state every 30 seconds.

| Check | Source | Alert condition |
|-------|--------|----------------|
| Disk SMART | `smartctl` | Reallocated sectors > 0, pending sectors > 0, or SMART health failed |
| Filesystem usage | `statvfs` | Usage > 85% |
| Array state | `/proc/mdstat`, `mdadm --detail` | Array degraded or rebuilding |
| Rebuild progress | `mdadm --detail` | Estimated time remaining during rebuild |
| Tier pressure | LVM PV metadata | Any tier at or above `full_threshold` with no migration making progress |
| Migration stalled | tierd internal state | Migration in `migrating` state for > 2× expected duration |
| CPU temperature | `/sys/class/thermal/` | Approaching or exceeding thermal threshold |
| Memory pressure | `/proc/meminfo` | Available < 10% |
| Network link state | `ip link`, `/sys/class/net/` | Configured interface down or bond degraded |
| Fan/thermal | `ipmitool` or `/sys/class/hwmon/` | Fan failure or thermal warning/critical (best-effort) |

Alerts are surfaced on the dashboard and available via `/api/system/alerts`.

### Disk replacement workflow

1. User sees a degraded-array alert on the dashboard
2. User identifies the failed disk (UI highlights it; LED blink available)
3. User physically replaces the disk
4. User clicks Replace in the UI, selecting the new disk
5. `tierd` calls `mdadm --manage --remove` on the failed disk, then `mdadm --manage --add` with the new disk
6. Rebuild starts automatically; progress shown on the dashboard

### Scrub scheduling

`mdadm --action=check` verifies parity on all arrays. Default: weekly. Configurable per array through the UI.

---

## Security

### Network access

| Port | Service | Access |
|------|---------|--------|
| 443 | nginx (HTTPS) | Web UI and API. Self-signed TLS by default, user-replaceable. |
| 22 | SSH | Standard Debian SSH. Enabled by default for emergency access. |

`tierd` listens on localhost:8420 only. No other ports are exposed.

### Privilege model

`tierd` runs as root. All user-supplied parameters (disk paths, mount points, array and volume names) are validated against strict allowlists before being passed to subprocess calls. Disk paths must match `/dev/sd[a-z]+`, `/dev/nvme[0-9]+n[0-9]+`, or `/dev/md[0-9]+`. Mount points must be under `/mnt/`. RAID levels and tier names must match the known set. Array and volume names must be alphanumeric with hyphens and underscores only. No shell expansion is used; all subprocess calls use `exec.Command` with explicit argument lists.

### Session security

- Session tokens: cryptographically random, 256-bit
- Cookie flags: `HttpOnly`, `Secure`, `SameSite=Strict`
- Session expiry: 24 hours, sliding window
- Failed login rate limiting: 5 attempts per minute per IP, then 15-minute lockout

---

## Data Model

**Tier:**
- `name` (NVME | SSD | HDD)
- `rank` (1 | 2 | 3)
- `array_id` (nullable — foreign key to mdadm array)
- `pv_device` (nullable — block device path, e.g. `/dev/md0`)
- `vg_name` (always `smoothnas` when active)
- `full_threshold` (percentage, default 90)

**LV additions:**
- `pinned` (boolean, default false)

**LV Region** (one row per region per LV):
- `lv_id` (foreign key)
- `region_index` (integer — 0-based offset within LV)
- `region_offset_bytes` (integer)
- `region_size_bytes` (integer — configurable, default 256 MB)
- `current_tier` (nullable — NVME | SSD | HDD)
- `heat_score` (float — current rolling average IOPS)
- `heat_sampled_at` (timestamp)
- `consecutive_wrong_tier_cycles` (integer; reset on migration or heat band change)
- `migration_state` (idle | queued | migrating | cancelling | verifying | complete | failed)
- `migration_triggered_by` (nullable — policy | spillover)
- `migration_dest_tier` (nullable — NVME | SSD | HDD)
- `migration_bytes_moved` (integer)
- `migration_bytes_total` (integer)
- `migration_started_at` (timestamp)
- `migration_ended_at` (nullable timestamp)
- `migration_failure_reason` (nullable string)
- `last_movement_reason` (nullable — policy | spillover)
- `last_movement_at` (nullable timestamp)

**Policy config:**
- `nvme_threshold` (IOPS per region, default TBD)
- `ssd_threshold` (IOPS per region, default TBD; must be < `nvme_threshold`)
- `consecutive_cycles_before_migration` (default 3)
- `poll_interval_minutes` (default 5)
- `rolling_window_hours` (default 24)
- `evaluation_interval_minutes` (default 30)
- `region_size_mb` (default 256; must be a multiple of the LVM PE size)
- `tier_reserve_pct` (default 10 — free capacity to maintain on each tier before blocking further migration in)
- `migration_iops_cap` (MB/s)
- `migration_io_high_water_pct`

---

## Trade-offs

**Per-block tiering at region granularity.** The physical extent region is the migration unit, not the LV. A single LV can have hot regions on NVMe and cold regions on HDD simultaneously. The region size (default 256 MB) is a trade-off: smaller regions give finer granularity and more precise heat tracking but generate more migrations and metadata; larger regions reduce churn but may carry cold blocks along with hot ones.

**pvmove for zero-downtime migration.** `pvmove` is well-tested, crash-aware, and delivers online migration without unmounting. The alternative — a custom extent-copy engine with dm suspend/resume — would be more complex, harder to recover from on crash, and provide no additional benefit for LV-level tiering.

**Single shared VG.** All tier arrays are PVs in one VG. Cross-tier migration via `pvmove` requires this — `pvmove` cannot move extents between VGs. The downside is that the shared VG is a single namespace; the upside is that the migration path is trivial and well-understood.

**Separate VG per tier rejected.** Cross-tier migration would require a copy workflow and an I/O interruption window. Zero-downtime migration is a hard requirement; `pvmove` within a shared VG is the only credible path to it.

**One migration at a time.** Running multiple concurrent `pvmove` operations on the same VG risks I/O contention that outweighs the throughput gain. Migrations are queued and run serially. The I/O cap and high-water mark prevent the single migration from starving normal I/O.

**Demotion one step at a time.** A volume demotes NVME→SSD, then SSD→HDD on subsequent evaluation cycles. Jumping directly to the coldest tier on a transient dip in IOPS would cause excessive migration on workloads with periodic cold windows. Gradual demotion, combined with hysteresis, keeps migration stable.

**Go shelling out to CLI tools.** Calling `mdadm`, `lvm`, and `pvmove` as subprocesses is less efficient but far more maintainable than C library bindings. The CLI interfaces are stable across kernel and LVM releases. The fork/exec overhead is irrelevant for storage management operations.

---

## Acceptance Criteria

### Infrastructure

- [ ] NVME, SSD, and HDD tiers exist in the database on first boot; all initially inactive.
- [ ] Assigning an mdadm array to a tier adds it as a PV in VG `smoothnas` with the correct tier tag.
- [ ] A tier can only be assigned to one array; assigning a second array to an occupied tier is rejected.
- [ ] New LV extents are allocated from the PV matching the fastest active tier with available capacity.
- [ ] Extending an LV allocates new extents from the fastest active tier with available capacity.
- [ ] Removing a tier assignment is rejected when the tier has any LV extents on it.
- [ ] The Arrays page shows the tier label for each array.

### Spillover

- [ ] New extents target the fastest active tier; if that tier is at or above `full_threshold`, allocation falls to the next colder active tier.
- [ ] Spillover cascades: NVME full → SSD; SSD also full → HDD.
- [ ] If all tiers are full, LV creation or extension fails with a clear capacity error.

### Migration

- [ ] Region-to-PE mapping is derived from `lvs -o seg_pe_ranges` and kept current.
- [ ] Migration runs online via `pvmove <source_pv>:<pe_start>-<pe_end> <dest_pv>` with no downtime.
- [ ] Only one migration runs at a time.
- [ ] Migration respects the configured I/O cap.
- [ ] Migration backs off when host I/O utilisation exceeds the high-water mark.
- [ ] Migration progress (bytes moved / total) is tracked and visible in the UI.
- [ ] On restart after a crash mid-migration, `tierd` detects the inconsistency and surfaces it in the UI.
- [ ] After a failed migration, the region's PEs are in a consistent state on either the source or destination tier.

### Policy engine

- [ ] `tierd` creates dm-stats regions of the configured size on each active LV at startup and LV creation.
- [ ] Per-region IOPS are collected on the configured poll interval and maintained as a rolling window average.
- [ ] A region with sustained hot-level heat is promoted directly to NVME, regardless of current tier.
- [ ] A region with sustained warm-level heat is promoted to SSD.
- [ ] A cold region on SSD demotes to HDD; a cold region on NVME demotes to SSD first.
- [ ] A region must be in the wrong-tier state for the configured number of consecutive cycles before migration is queued.
- [ ] `ssd_threshold` < `nvme_threshold` is enforced; configuration violating this is rejected.
- [ ] All regions of a pinned LV are excluded from policy evaluation.
- [ ] Pin state is toggleable per LV in the UI.
- [ ] Per-region heat scores and current tier are visible in the UI.
- [ ] Global policy thresholds and region size are configurable in Settings.
