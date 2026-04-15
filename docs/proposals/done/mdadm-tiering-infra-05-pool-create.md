# Proposal: mdadm Tiering Infrastructure — POST /api/tiers (Pool Creation)

**Status:** Done
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 5 of 14)
**Depends on:** mdadm-tiering-infra-04-data-model

---

## Problem

Creating a tier pool involves validating the name, creating an LVM VG, writing DB records, and handling partial failure at every step. The handler must be resilient to stale VGs left by previous failed attempts and must not leave orphaned LVM structures on failure.

---

## Specification

### Request

`POST /api/tiers`

```json
{
  "name": "production",
  "filesystem": "xfs",
  "tiers": [
    { "name": "NVME", "rank": 1 },
    { "name": "SSD",  "rank": 2 },
    { "name": "HDD",  "rank": 3 }
  ]
}
```

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `name` | Yes | — | Validated per Step 1. |
| `filesystem` | No | `"xfs"` | `"xfs"` or `"ext4"`. |
| `tiers` | No | Standard 3-tier list | Full list of tier slots for this pool. All entries must have unique `name` and unique `rank`. At least one entry required if provided. Ranks must be positive integers; gaps are permitted. |

The `tiers` field is the only window for defining custom tiers. Once the pool is created, the tier list is fixed.

### Stale instance recovery

Before issuing any LVM command, check whether a TierPool row with the given `name` already exists in `state = 'provisioning'` or `state = 'error'` **and** all its tier slots are `empty` (no arrays have ever been assigned). If so, this is a re-creation attempt on a failed prior run. Clean up the stale state:

1. Run `vgremove --force tier-{name}` (idempotent if the VG does not exist).
2. Delete the stale Tier and TierPool rows.
3. Proceed with fresh creation.

This addresses the failure mode fixed in `f71e84d`.

### Provisioning sequence

Set pool `state = 'provisioning'`, then execute the following in order. If any step fails, execute the rollback column and set `state = 'error'` with `error_reason` populated.

| # | Action | Rollback if this fails |
|---|--------|----------------------|
| 1 | Validate name (Step 1). | — (nothing created) |
| 2 | Check `/mnt/{name}` does not exist as a file or active mount point. | — |
| 3 | Check no TierPool with this name exists in a non-stale state (reject `409` if so). | — |
| 4 | Run `vgcreate tier-{name}` (empty VG, no PVs). | — |
| 5 | Insert TierPool row (`state = 'provisioning'`) and Tier rows (`state = 'empty'`). | `vgremove --force tier-{name}` |

After step 5, the pool exists with all slots `empty`. The LV is created only after the first array is assigned (Step 6). The pool remains in `provisioning` state until the LV is created, formatted, and mounted.

### Response

**201 Created:**
```json
{
  "name": "production",
  "filesystem": "xfs",
  "state": "provisioning",
  "mount_point": "/mnt/production",
  "capacity_bytes": 0,
  "used_bytes": 0,
  "tiers": [
    { "name": "NVME", "rank": 1, "state": "empty", "array_id": null, "pv_device": null, "capacity_bytes": 0 },
    { "name": "SSD",  "rank": 2, "state": "empty", "array_id": null, "pv_device": null, "capacity_bytes": 0 },
    { "name": "HDD",  "rank": 3, "state": "empty", "array_id": null, "pv_device": null, "capacity_bytes": 0 }
  ],
  "created_at": "2026-04-09T00:00:00Z",
  "updated_at": "2026-04-09T00:00:00Z",
  "last_reconciled_at": null
}
```

---

## Acceptance Criteria

- [ ] `POST /api/tiers` with a valid payload creates the VG and DB rows; pool enters `provisioning` state.
- [ ] Custom tier lists (more than 3 tiers, or non-standard names) are accepted.
- [ ] Duplicate pool names in an active state are rejected with `409 Conflict`.
- [ ] Invalid names are rejected with `400 Bad Request`.
- [ ] A `/mnt/{name}` path that already exists as a file or active mount is rejected with `409 Conflict`.
- [ ] Stale instances (`provisioning` or `error` with no assigned arrays) are cleaned up and replaced on re-creation.
- [ ] Provisioning failure at any step leaves no orphaned LVM structures and sets `state = 'error'` with a reason.
