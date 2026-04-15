# Proposal: mdadm Tiering Infrastructure — GET /api/tiers/{name}/map (PE Mapping)

**Status:** Pending
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 10 of 14)
**Depends on:** mdadm-tiering-infra-12-pv-allocation

---

## Problem

After LV creation and extension, there is no external-facing way to confirm that physical extents are laid out in rank order (fastest tier first). The map endpoint exposes the raw LVM segment data alongside a verified flag so that operators and the test suite can confirm allocation correctness without shelling into the host.

---

## Specification

### Request

`GET /api/tiers/{name}/map`

### Response

**200 OK:**
```json
{
  "pool": "production",
  "lv": "data",
  "segments": [
    { "rank": 1, "tier": "NVME", "pv_device": "/dev/md0", "pe_start": 0,    "pe_end": 2559   },
    { "rank": 2, "tier": "SSD",  "pv_device": "/dev/md1", "pe_start": 2560, "pe_end": 10239  }
  ],
  "verified": true,
  "verified_at": "2026-04-09T02:00:00Z"
}
```

| Field | Source | Notes |
|-------|--------|-------|
| `segments` | `lvs -o seg_pe_ranges,devices tier-{name}/data` | One entry per linear segment, in PE offset order. |
| `rank` / `tier` | Joined from Tier DB rows via `pv_device`. | |
| `verified` | Result of the segment verification algorithm (Step 12). | `true` if segments are in strict rank order. |
| `verified_at` | Timestamp of the last verification run. | Updated after every LV create, extend, or explicit map request. |

**404 Not Found** — if no pool with `{name}` exists.

**503 Service Unavailable** — if the `data` LV does not yet exist (pool still in `provisioning` state with no arrays assigned):
```json
{ "error": "LV does not exist yet; assign an array to a tier slot first" }
```

Calling this endpoint also triggers a fresh segment verification run and updates `verified_at`.

---

## Acceptance Criteria

- [ ] Response includes one entry per linear segment, in PE offset order.
- [ ] Each segment is annotated with the matching tier `rank` and `tier` name from the DB.
- [ ] `verified: true` when segments are in strict rank order (lowest rank / fastest tier first).
- [ ] `verified: false` when any segment is mapped to a PV whose tier rank is out of order.
- [ ] Calling the endpoint triggers a fresh verification run and updates `verified_at`.
- [ ] Returns `404` for unknown pool names.
- [ ] Returns `503` if the LV does not yet exist.
