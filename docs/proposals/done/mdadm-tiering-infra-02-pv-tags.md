# Proposal: mdadm Tiering Infrastructure — PV Tag Schema

**Status:** Pending
**Part of:** mdadm-tiering-infrastructure (Step 2 of 14)
**Depends on:** base appliance (tierd service)

---

## Problem

Physical volumes belonging to tier pool VGs must carry machine-readable identity so that boot-time reconciliation can discover which pool and tier slot each PV belongs to, without relying solely on the `tier-` VG name prefix. Without this, a VG imported from another host or renamed manually becomes unrecognisable to the reconciler.

---

## Specification

Every PV added to a pool VG carries two LVM tags:

| Tag | Format | Example |
|-----|--------|---------|
| Pool identity | `smoothnas-pool:{pool_name}` | `smoothnas-pool:production` |
| Tier identity | `smoothnas-tier:{tier_name}` | `smoothnas-tier:nvme` |

Tags are applied immediately after `pvcreate`, via:

```
pvchange --addtag smoothnas-pool:{name} --addtag smoothnas-tier:{tier_name} {device}
```

Tags are metadata only — they are not used for LVM allocation enforcement. Allocation order is controlled by explicit PV device path ordering at `lvcreate`/`lvextend` time (Step 12). Tags exist for two purposes:

1. **Boot-time reconciliation** (Step 13): the reconciler queries `pvs -o pv_name,tags` and matches `smoothnas-pool:` tags to identify managed PVs regardless of VG name.
2. **Operator debugging**: `pvs -o+tags` exposes both tags for manual inspection.

---

## Acceptance Criteria

- [ ] Every PV added to a pool VG carries both the `smoothnas-pool:` and `smoothnas-tier:` tags.
- [ ] Tags are applied at array assignment time, before `vgextend`.
- [ ] Boot-time reconciliation uses the `smoothnas-pool:` tag to discover managed PVs, not only the `tier-` VG name prefix.
- [ ] `pvs -o+tags` shows both tags on every managed PV.
