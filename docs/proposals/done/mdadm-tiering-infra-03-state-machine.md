# Proposal: mdadm Tiering Infrastructure — Pool and Tier State Machine

**Status:** Pending
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 3 of 14)
**Depends on:** base appliance (tierd service)

---

## Problem

Without a formalised state model, the UI, API, and reconciler each make independent assumptions about what a pool can be doing and what operations are valid at any given moment. This leads to inconsistent behaviour — for example, trying to mount a pool that is mid-provisioning, or deleting a pool whose LVM structures are in an error state.

---

## Specification

### TierPool states

| State | Meaning |
|-------|---------|
| `provisioning` | Creation in progress; VG created, no LV yet. Waiting for first array assignment. |
| `healthy` | VG exists, `data` LV exists, filesystem mounted at `/mnt/{name}`. |
| `degraded` | One or more tier arrays are missing or failed; LV is mounted only if safe. |
| `unmounted` | VG and LV exist but the filesystem is not currently mounted. |
| `error` | Provisioning failed or an unrecoverable inconsistency detected (e.g. segment order violation). |
| `destroying` | Deletion in progress. |

### Tier slot states

| State | Meaning |
|-------|---------|
| `empty` | No array assigned to this slot. |
| `assigned` | Array assigned and PV healthy in the VG. |
| `degraded` | Backing mdadm array is degraded or rebuilding. |
| `missing` | PV recorded in DB but not found by LVM at reconciliation time. |

### Transitions

**TierPool:**
```
provisioning ──→ healthy       (first array assigned, LV created, formatted, mounted)
provisioning ──→ error         (any provisioning step fails)
healthy      ──→ degraded      (a tier slot becomes degraded or missing)
degraded     ──→ healthy       (all slots return to assigned)
healthy      ──→ destroying    (DELETE /api/tiers/{name} accepted)
degraded     ──→ destroying    (DELETE /api/tiers/{name} accepted)
unmounted    ──→ destroying    (DELETE /api/tiers/{name} accepted)
error        ──→ destroying    (DELETE /api/tiers/{name} accepted)
destroying   ──→ (removed)     (destruction sequence completes successfully)
```

**Tier slot:**
```
empty    ──→ assigned   (array assignment, PUT /api/tiers/{name}/tiers/{tier})
assigned ──→ empty      (array unassignment, DELETE /api/tiers/{name}/tiers/{tier})
assigned ──→ degraded   (reconciler detects backing array degraded)
assigned ──→ missing    (reconciler cannot find PV at startup)
degraded ──→ assigned   (backing array returns to healthy)
missing  ──→ assigned   (PV reappears at reconciliation time)
```

### Operation guards

| Operation | Blocked when pool is in state |
|-----------|-------------------------------|
| Array assignment | `destroying` |
| Array unassignment | `destroying` |
| Pool deletion | `destroying` (already in progress) |
| Mount | `error`, `destroying` |

---

## Acceptance Criteria

- [ ] TierPool and Tier DB rows each have a `state` column with the enumerations above enforced at the DB layer.
- [ ] State transitions occur only via the defined paths; no handler sets an arbitrary state value.
- [ ] The API always returns the current `state` field for pools and their tier slots.
- [ ] Operations blocked by the guard table return `409 Conflict` with the current state in the error body.
