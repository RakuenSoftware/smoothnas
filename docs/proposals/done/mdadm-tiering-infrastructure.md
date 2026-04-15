# Proposal: mdadm Tiering — Phase 1: Infrastructure

**Status:** Pending
**Date:** 2026-04-09
**Implements:** mdadm-tiering
**Depends on:** base appliance (tierd service, web UI shell, disk inventory, mdadm array management)
**Followed by:** mdadm-complete-heat-engine (Phase 2)

---

## Problem

The mdadm backend has no concept of storage tiers. All arrays and volumes are treated as equivalent. Before automatic heat-based migration or spillover can be built, the foundational plumbing must exist: isolated tier pools, dedicated LVM volume groups, and automatic volume provisioning.

---

## Non-Goals (Phase 1)

The following are explicitly deferred to Phase 2 (`mdadm-complete-heat-engine`):

- Managed logical volume model (multiple LVs per pool)
- Per-region heat tracking and dm-stats sampling pipeline
- Migration queue, pvmove-based extent migration
- Policy engine (promotion, demotion, spillover)
- UI or API for volume heat, pinning, or migration status

Phase 1 delivers the pool abstraction only: isolated VGs, a single 100%FREE LV per pool, format, mount, reconciliation, and auto-expansion.

---

## Goals

- **Isolated Tier Pools:** Each pool is a self-contained storage domain. Data movement is strictly contained within the pool's assigned disks.
- **Dedicated Volume Groups:** Each pool gets a dedicated LVM VG named `tier-{name}`.
- **1-to-1 Pool-Volume Relationship:** Creating a pool automatically creates a single volume (LV) using 100% of the pool's capacity.
- **Dynamic Tiering:** Support the default 3 tiers (NVME, SSD, HDD) with the ability to add more tiers *only* at the time of pool creation.
- **Automatic Provisioning:** No "Create Volume" dialog. Provisioning the pool handles VG creation, LV creation (100%FREE), formatting, and mounting.
- **PV Tagging:** Identify PVs with both pool and tier identity tags for precise allocation.
- **Boot-time Reconciliation:** Automatically discover, verify, and mount pools on system startup.
- **Auto-Expansion:** Automatically grow the pool's volume when an underlying mdadm array is expanded.

---

## Architecture

### Tier Pool Isolation

A **Tier Pool** is a set of mdadm arrays managed as a single LVM unit. There is no "global" mobility; data created in "Pool A" can never be migrated or spilled over to "Pool B".

Each pool is backed by a dedicated LVM volume group: `tier-{pool_name}`.

### The "Single Volume" Model

To simplify the user experience, a Tier Pool **is** the volume.
- When a user creates a pool, `tierd` creates the VG `tier-{name}`.
- It immediately creates a single LV named `data` using `100%FREE` of all physical extents in the VG.
- This LV spans all tiers (PVs) assigned to the pool.
- The volume is formatted (XFS by default, ext4 optional) and mounted at `/mnt/{pool_name}` automatically.

### Performance & Allocation Safety (Anti-Regression)

To prevent the "all-tier write" performance bug, `tierd` must enforce strict linear allocation:
- **No Striping:** LVs must never be created with striping (`-i`) across tiers.
- **Ordered Allocation:** The `lvcreate` and `lvextend` commands MUST pass PV paths as positional arguments in order of **Rank** (Fastest → Slowest).
- **Verification:** The backend must verify that the LV's linear segments map to the correct PVs in the intended rank order after creation and after each extension.

### Dynamic Tiering Model

By default, every pool supports three standard tiers:

| Tier | Rank | Typical hardware |
|------|------|-----------------|
| NVME | 1 | NVMe SSDs |
| SSD  | 2 | SATA/SAS SSDs |
| HDD  | 3 | Spinning disks |

**Extension Rule:** Users can specify **more than three tiers** only during the pool creation process. Once created, the number and ranking of tiers in a pool are fixed.

### Boot-time Reconciliation

On system startup, `tierd` performs a **Storage Discovery Scan** after mdadm has assembled its arrays. It identifies all managed VGs via PV tags, cross-references them against the database, mounts healthy pools, and surfaces degraded or inconsistent state in the UI.

### Automatic Capacity Maintenance

When an mdadm array assigned to a pool is expanded, `tierd` detects the block device size change and automatically runs `pvresize`, `lvextend`, and `xfs_growfs`/`resize2fs` to propagate the new capacity to the filesystem.

---

## API Summary

| Endpoint | Method | Step | Description |
|----------|--------|------|-------------|
| `/api/tiers` | GET | 8 | List all pools with tier slots and capacity. |
| `/api/tiers` | POST | 5 | Create a new Tier Pool. |
| `/api/tiers/{name}` | GET | 9 | Get a single pool's detail. |
| `/api/tiers/{name}` | DELETE | 11 | Delete a Tier Pool (requires name-match challenge). |
| `/api/tiers/{name}/tiers/{tier}` | PUT | 6 | Assign an mdadm array to a tier slot. |
| `/api/tiers/{name}/tiers/{tier}` | DELETE | 7 | Remove an array from a tier slot. |
| `/api/tiers/{name}/map` | GET | 10 | Physical extent mapping and segment verification result. |

---

## Implementation Steps

Each step is a standalone proposal with its own specification and acceptance criteria.

| Step | Proposal | Summary |
|------|----------|---------|
| 1 | [mdadm-tiering-infra-01-naming-rules](mdadm-tiering-infra-01-naming-rules.md) | Naming rules and validation |
| 2 | [mdadm-tiering-infra-02-pv-tags](mdadm-tiering-infra-02-pv-tags.md) | PV tag schema |
| 3 | [mdadm-tiering-infra-03-state-machine](mdadm-tiering-infra-03-state-machine.md) | Pool and tier state machine |
| 4 | [mdadm-tiering-infra-04-data-model](mdadm-tiering-infra-04-data-model.md) | Database schema |
| 5 | [mdadm-tiering-infra-05-pool-create](mdadm-tiering-infra-05-pool-create.md) | POST /api/tiers — pool creation |
| 6 | [mdadm-tiering-infra-06-array-assign](mdadm-tiering-infra-06-array-assign.md) | PUT /api/tiers/{name}/tiers/{tier} — array assignment |
| 7 | [mdadm-tiering-infra-07-array-unassign](mdadm-tiering-infra-07-array-unassign.md) | DELETE /api/tiers/{name}/tiers/{tier} — array unassignment |
| 8 | [mdadm-tiering-infra-08-list-pools](mdadm-tiering-infra-08-list-pools.md) | GET /api/tiers — list all pools |
| 9 | [mdadm-tiering-infra-09-get-pool](mdadm-tiering-infra-09-get-pool.md) | GET /api/tiers/{name} — single pool detail |
| 10 | [mdadm-tiering-infra-10-pe-map](mdadm-tiering-infra-10-pe-map.md) | GET /api/tiers/{name}/map — PE mapping |
| 11 | [mdadm-tiering-infra-11-pool-delete](mdadm-tiering-infra-11-pool-delete.md) | DELETE /api/tiers/{name} — pool deletion |
| 12 | [mdadm-tiering-infra-12-pv-allocation](mdadm-tiering-infra-12-pv-allocation.md) | Ordered PV allocation and segment verification |
| 13 | [mdadm-tiering-infra-13-reconciliation](mdadm-tiering-infra-13-reconciliation.md) | Boot-time reconciliation |
| 14 | [mdadm-tiering-infra-14-auto-expansion](mdadm-tiering-infra-14-auto-expansion.md) | Auto-expansion |
