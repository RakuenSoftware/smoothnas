# Proposal: mdadm Tiering Infrastructure — GET /api/tiers (List All Pools)

**Status:** Pending
**Part of:** mdadm-tiering-infrastructure (Step 8 of 14)
**Depends on:** mdadm-tiering-infra-04-data-model

---

## Problem

The UI and any monitoring tooling need a single endpoint to enumerate all pools with their tier slots, states, and live capacity figures.

---

## Specification

### Request

`GET /api/tiers`

No parameters.

### Response

**200 OK** — array of pool objects. Empty array `[]` if no pools exist.

```json
[
  {
    "name": "production",
    "filesystem": "xfs",
    "state": "healthy",
    "mount_point": "/mnt/production",
    "capacity_bytes": 10995116277760,
    "used_bytes": 2199023255552,
    "error_reason": null,
    "tiers": [
      {
        "name": "NVME",
        "rank": 1,
        "state": "assigned",
        "array_id": 1,
        "pv_device": "/dev/md0",
        "capacity_bytes": 2199023255552
      },
      {
        "name": "SSD",
        "rank": 2,
        "state": "assigned",
        "array_id": 2,
        "pv_device": "/dev/md1",
        "capacity_bytes": 8796093022208
      },
      {
        "name": "HDD",
        "rank": 3,
        "state": "empty",
        "array_id": null,
        "pv_device": null,
        "capacity_bytes": 0
      }
    ],
    "created_at": "2026-04-09T00:00:00Z",
    "updated_at": "2026-04-09T01:00:00Z",
    "last_reconciled_at": "2026-04-09T02:00:00Z"
  }
]
```

Capacity fields (`capacity_bytes` on both pool and tier slots) are derived from `vgs`/`pvs` output at request time, not stored in the DB. `used_bytes` at the pool level is derived from `df` or `statvfs` on the mounted filesystem; `0` if not mounted.

---

## Acceptance Criteria

- [ ] Returns all pools with their tier slots, states, and live capacity figures.
- [ ] An empty pool list returns `[]` with `200 OK`.
- [ ] `capacity_bytes` and `used_bytes` reflect current LVM/filesystem state, not stale DB values.
- [ ] Tier slots in `empty` state return `array_id: null`, `pv_device: null`, `capacity_bytes: 0`.
