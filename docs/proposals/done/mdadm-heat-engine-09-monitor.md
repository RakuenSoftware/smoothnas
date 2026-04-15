# Proposal: mdadm Heat Engine — Monitor Integration

**Status:** Done
**Date:** 2026-04-10
**Part of:** mdadm-complete-heat-engine (Step 9 of 9)
**Depends on:** mdadm-heat-engine-04-dmstats, mdadm-heat-engine-05-migration-engine, mdadm-heat-engine-06-policy-engine

---

## Problem

The background monitor currently handles SMART polling and mdadm array health checks. The heat engine's sampler, policy evaluator, and migration runner need to run as long-lived background goroutines managed by the same lifecycle. Without monitor integration, none of the heat engine's active components start, and the system is inert even after all other steps are implemented.

---

## Specification

### Monitor goroutine model

The monitor already runs a set of named goroutines started at `tierd` startup. Each goroutine loops with a configurable sleep interval. The heat engine adds three new goroutines:

1. **Sampler** — runs on `poll_interval_minutes`
2. **Policy evaluator** — runs on `evaluation_interval_minutes`
3. **Migration runner** — runs continuously, blocking on the queue

All three goroutines:
- Are started by the monitor's `Start()` method.
- Are stopped cleanly when the monitor's context is cancelled (e.g. on `tierd` shutdown signal).
- Log a startup message at INFO level when first started.
- Log an ERROR and continue (do not crash `tierd`) if a single iteration fails.

### Startup reconciliation

Before starting any heat engine goroutine, the monitor runs a synchronous reconciliation pass. Reconciliation must complete before the sampler or policy evaluator begin their first sleep.

Reconciliation steps (in order):

1. **Managed volume verification** — for each `managed_volumes` row:
   - Confirm the LV exists (`lvs`). If missing, log ERROR and skip this volume for the rest of reconciliation.
   - Confirm the mount point is mounted. If not, attempt remount; if remount fails, log ERROR.
   - Confirm `size_bytes` matches live LV size; update if drifted.

2. **Region inventory reconciliation** — for each verified managed volume:
   - Call `ReconcileRegions` (Step 3) to add missing region rows or remove stale rows.
   - Call `ResolveCurrentTiers` (Step 3) to refresh `current_tier` for all regions.

3. **dm-stats reconciliation** — for each verified managed volume:
   - Call `EnsureStatsRegistered` (Step 4) to align dm-stats regions with the managed volume region inventory.
   - Confirm `dmstats_regions` rows are present for each managed volume region.

4. **Migration state recovery** — scan `managed_volume_regions` for rows where `migration_state = in_progress`:
   - Set `migration_state = failed`, `migration_failure_reason = 'interrupted_by_restart'`, `migration_ended_at = now()`.
   - Log each interrupted migration at WARN level.

5. **Queue rebuild** — scan `managed_volume_regions` for rows where `migration_state = queued`:
   - Load them into the in-memory migration queue in heat-score descending order.
   - Log the count of pre-existing queued migrations at INFO level.

### Sampler goroutine

Loop:

1. Sleep `poll_interval_minutes`.
2. Re-read `tier_policy_config.poll_interval_minutes` from the DB (picks up config changes without restart).
3. For each managed volume that is not in an error state:
   - Call the dm-stats sampler (Step 4).
4. Log the number of volumes sampled and any per-volume errors at DEBUG level.

### Policy evaluator goroutine

Loop:

1. Sleep `evaluation_interval_minutes`.
2. Re-read `tier_policy_config.evaluation_interval_minutes` from the DB.
3. Run the policy evaluation algorithm (Step 6).
4. Log the evaluation summary at INFO level.

### Migration runner goroutine

Loop:

1. Block on the migration queue for a new item (with a configurable idle poll interval of 10 seconds when the queue is empty).
2. When an item arrives, execute the migration (Step 5).
3. After completion (success or failure), check I/O utilization before dequeuing the next item.
4. On context cancellation, drain the current pvmove gracefully (send SIGTERM to the pvmove process and wait for it to exit before returning).

### Graceful shutdown

On SIGTERM or context cancel:

1. Stop enqueuing new migrations (the policy evaluator goroutine exits at its next sleep point).
2. Allow the currently running pvmove to complete (do not kill it). Wait up to 5 minutes. If pvmove has not exited after 5 minutes, send SIGKILL and mark the region `failed`.
3. Stop the sampler at its next sleep point.
4. Exit.

### Health reporting

Extend `GET /api/health` (or the equivalent existing health endpoint) to include a `heat_engine` section:

```json
{
  "heat_engine": {
    "sampler_last_run_at": "2026-04-10T00:05:00Z",
    "sampler_last_error": null,
    "evaluator_last_run_at": "2026-04-10T00:15:00Z",
    "evaluator_last_error": null,
    "migration_runner_active": true,
    "active_migration_region_id": 42,
    "queue_depth": 3,
    "reconciliation_completed_at": "2026-04-10T00:00:01Z"
  }
}
```

These fields are populated from in-memory state updated by each goroutine. No additional DB queries are needed for the health endpoint.

---

## Acceptance Criteria

- [ ] The monitor starts the sampler, policy evaluator, and migration runner goroutines on `tierd` startup.
- [ ] Startup reconciliation runs synchronously before any heat engine goroutine begins.
- [ ] Reconciliation verifies LV existence and mounts, reconciles region inventory, reconciles dm-stats regions, and recovers interrupted migration state.
- [ ] The sampler goroutine reads `poll_interval_minutes` from the DB on each iteration.
- [ ] The policy evaluator goroutine reads `evaluation_interval_minutes` from the DB on each iteration.
- [ ] A single sampler or evaluator iteration failure logs an ERROR but does not stop the goroutine.
- [ ] On SIGTERM, any running pvmove is allowed to complete for up to 5 minutes before the runner exits.
- [ ] `GET /api/health` includes a `heat_engine` section with last-run timestamps, last error, queue depth, and active migration state.
- [ ] All three goroutines stop cleanly when the monitor context is cancelled.
