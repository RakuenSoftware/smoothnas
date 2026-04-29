# Proposal: mdadm Tiering Infrastructure — Database Schema

**Status:** Pending
**Part of:** mdadm-tiering-infrastructure (Step 4 of 14)
**Depends on:** mdadm-tiering-infra-01-naming-rules, mdadm-tiering-infra-02-pv-tags, mdadm-tiering-infra-03-state-machine

---

## Problem

The data model must capture pool identity, per-slot array assignments, state, and timestamps needed for reconciliation. The schema is the shared contract between the API handlers, the reconciler, and the auto-expansion monitor.

---

## Specification

### TierPool

| Column | Type | Constraints | Notes |
|--------|------|-------------|-------|
| `id` | integer | primary key | |
| `name` | text | unique, not null | Validated per Step 1. Used for VG name and mount point. |
| `filesystem` | text | not null, default `'xfs'` | `'xfs'` or `'ext4'`. |
| `state` | text | not null | Enum per Step 3: `provisioning`, `healthy`, `degraded`, `unmounted`, `error`, `destroying`. |
| `error_reason` | text | nullable | Human-readable reason when `state = 'error'`; null otherwise. |
| `created_at` | datetime | not null | Set at insert time. |
| `updated_at` | datetime | not null | Updated on every write to this row. |
| `last_reconciled_at` | datetime | nullable | Updated each time the boot-time reconciler processes this pool. |

### Tier (slot within a pool)

| Column | Type | Constraints | Notes |
|--------|------|-------------|-------|
| `id` | integer | primary key | |
| `pool_id` | integer | not null, foreign key → TierPool.id | |
| `name` | text | not null | `'NVME'`, `'SSD'`, `'HDD'`, or custom. Unique within pool. |
| `rank` | integer | not null | Lower = faster. 1-based. Unique within pool. |
| `state` | text | not null | Enum per Step 3: `empty`, `assigned`, `degraded`, `missing`. |
| `array_id` | integer | nullable, foreign key → mdadm array | Null when slot is `empty`. |
| `pv_device` | text | nullable | Block device path, e.g. `/dev/md0`. Null when slot is `empty`. |

### Constraints

- `(pool_id, name)` — composite unique: tier names are unique within a pool.
- `(pool_id, rank)` — composite unique: ranks are unique within a pool.
- `array_id` — unique across all Tier rows: one array can back at most one slot in any pool.

### Migration

The schema migration must:
1. Create the `tier_pools` table.
2. Create the `tiers` table with the foreign key and composite unique constraints above.
3. Add a DB-level check constraint on `tier_pools.state` and `tiers.state` to enforce the enum values.

---

## Acceptance Criteria

- [ ] Schema migration creates both tables with all columns above.
- [ ] `(pool_id, name)` and `(pool_id, rank)` are enforced as composite unique constraints.
- [ ] `array_id` uniqueness is enforced across all Tier rows.
- [ ] `state` columns enforce the enum values at the DB layer.
- [ ] `updated_at` is updated on every write to a TierPool row.
- [ ] `last_reconciled_at` is updated each time the reconciler processes a pool.
- [ ] `error_reason` is populated when `state = 'error'` and null otherwise.
