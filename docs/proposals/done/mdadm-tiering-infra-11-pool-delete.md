# Proposal: mdadm Tiering Infrastructure — DELETE /api/tiers/{name} (Pool Deletion)

**Status:** Done
**Part of:** mdadm-tiering-infrastructure (Step 11 of 14)
**Depends on:** mdadm-tiering-infra-05-pool-create

---

## Problem

Deleting a pool is irreversible and destroys the filesystem and all data on it. The endpoint must guard against accidental invocation, active share consumers, and partial failures that leave the system in an ambiguous state.

---

## Specification

### Request

`DELETE /api/tiers/{name}`

**Required body (purge challenge):**
```json
{
  "confirm_pool_name": "production"
}
```

If `confirm_pool_name` does not exactly match the URL `{name}` parameter, reject immediately with `400 Bad Request`. No LVM command is issued.

### Pre-flight checks

Run both checks before setting `state = 'destroying'`:

1. **Pool state:** If the pool is already in `destroying` state, reject with `409 Conflict`.
2. **Active consumers:** Check whether any NFS export, SMB share, or iSCSI target is currently backed by `/mnt/{name}`. If any are active, reject with `409 Conflict` and list the blocking consumers:
   ```json
   {
     "error": "pool has active consumers; remove them before deleting",
     "consumers": ["nfs:/mnt/production/exports/media", "smb:share-name"]
   }
   ```

### Destruction sequence

1. Set pool `state = 'destroying'`.
2. Unmount `/mnt/{name}` (`umount /mnt/{name}`). If the mount is busy, try `umount -l` (lazy unmount) and proceed.
3. Remove the fstab entry for this pool.
4. Remove the `/mnt/{name}` directory if it is now empty.
5. Run `lvremove -f tier-{name}/data`.
6. For each assigned Tier slot (in any order): run `vgreduce tier-{name} {pv_device}`, then `pvremove {pv_device}`. Update each Tier row to `state = 'empty'`.
7. Run `vgremove --force tier-{name}`.
8. Delete all Tier rows for this pool, then the TierPool row.

### Partial failure handling

If any step in the destruction sequence fails, the pool remains in `destroying` state with an updated `error_reason`. It is **not** rolled back to its previous state. The operator must inspect the system and complete cleanup manually. Leaving the pool in `destroying` makes the failed state visible in the API rather than silently hiding it.

### Response

**200 OK** on successful deletion:
```json
{ "deleted": "production" }
```

---

## Acceptance Criteria

- [ ] Mismatched `confirm_pool_name` is rejected with `400 Bad Request` before any LVM command.
- [ ] Active NFS/SMB/iSCSI consumers block deletion with `409 Conflict` and list the blocking consumers.
- [ ] A pool already in `destroying` state is rejected with `409 Conflict`.
- [ ] Successful deletion: unmounts filesystem, removes fstab entry, removes `/mnt/{name}`, removes LV, removes all PVs from VG, removes VG, deletes DB rows.
- [ ] Partial failure leaves the pool in `destroying` state with `error_reason` populated; does not roll back.
