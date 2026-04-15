# Proposal: mdadm Tiering Infrastructure — Boot-time Reconciliation

**Status:** Pending
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 13 of 14)
**Depends on:** mdadm-tiering-infra-04-data-model, mdadm-tiering-infra-12-pv-allocation

---

## Problem

On reboot, pool VGs and their LVs persist on disk but `tierd`'s in-memory state is gone. Without an active reconciliation pass, pools are not remounted, degraded states are not detected, and the UI has no accurate picture of storage health. Additionally, mdadm must assemble arrays before `tierd` can scan VGs — if the service starts too early, it will see no PVs and incorrectly mark all pools as `missing`.

---

## Specification

### Systemd ordering

`tierd.service` must declare:

```ini
After=mdadm-monitor.service
```

This guarantees mdadm arrays are assembled before `tierd` attempts VG discovery and LV mounting.

### Discovery algorithm

On startup `tierd` runs the following scan in order:

**1. Discover managed PVs.**
Run `pvs --noheadings -o pv_name,vg_name,tags`. Filter for PVs carrying a `smoothnas-pool:` tag. Build a map of `{pool_name → [(pv_device, tier_name)]}` from the tag values.

**2. Reconcile against DB.**
For each pool found in the DB:
- For each Tier slot with `state = 'assigned'`: check whether the expected `pv_device` appears in the discovered PV map.
  - Found → no state change (or update `state = 'assigned'` if it was `missing`).
  - Not found → set Tier `state = 'missing'`; set pool `state = 'degraded'`.
- For each Tier slot with `state = 'missing'`: check if the PV has reappeared.
  - Reappeared → set Tier `state = 'assigned'`; re-evaluate pool state.
- For each PV in the discovered map with no matching DB row: log a warning and skip — do not auto-import unrecognised VGs.

**3. Mount healthy and conditionally mount degraded pools.**
For each pool in `healthy` or `degraded` state:

a. Check the `data` LV exists: `lvs tier-{name}/data`. If it does not, log and skip (pool remains in current state).

b. Check `/mnt/{name}`:
   - Does not exist → create it as a directory.
   - Exists as a file → log ERROR, set pool `state = 'error'`, `error_reason = 'mount_path_is_file'`, skip.
   - Exists and is already mounted by a different device → log ERROR, set pool `state = 'error'`, `error_reason = 'mount_path_conflict'`, skip.

c. If the pool is `healthy`: mount `tier-{name}/data` at `/mnt/{name}` if not already mounted.

d. If the pool is `degraded`: only mount if `lvs` reports no missing PV segments in the extent map (i.e. the LV is accessible despite a degraded backing array). If mounting would risk filesystem corruption, leave unmounted and set `error_reason = 'degraded_unsafe_to_mount'`.

e. Ensure the fstab entry is present; add it if missing.

**4. Run segment verification.**
For each mounted LV, run the segment verification algorithm (Step 12). Update the verification result exposed by the map endpoint (Step 10).

**5. Update timestamps.**
Set `last_reconciled_at = now()` for every pool processed.

---

## Acceptance Criteria

- [ ] `tierd.service` has `After=mdadm-monitor.service` in its unit file.
- [ ] Healthy pools are discovered and mounted at startup without manual intervention.
- [ ] Pools with missing PVs are marked `degraded`; they are only mounted if the LV is still accessible.
- [ ] Unrecognised VGs (no DB row) are logged as warnings and not imported.
- [ ] `/mnt/{name}` that exists as a file sets pool `state = 'error'` with `error_reason = 'mount_path_is_file'`.
- [ ] `/mnt/{name}` already mounted by a different device sets pool `state = 'error'` with `error_reason = 'mount_path_conflict'`.
- [ ] Missing fstab entries are added during reconciliation.
- [ ] Segment verification runs for every mounted LV during reconciliation.
- [ ] `last_reconciled_at` is updated for every pool processed.
