# Proposal: mdadm Heat Engine — API Surface (Remainder)

**Status:** Pending
**Part of:** mdadm-complete-heat-engine (follow-up to Step 7 of 9)
**Depends on:** mdadm-heat-engine-05-migration-engine, mdadm-heat-engine-06-policy-engine, mdadm-heat-engine-02-managed-volumes-remainder

---

## Background

`mdadm-heat-engine-07-api.md` was moved to `done/` because the volume CRUD half of the API surface is in place: `tierd/internal/api/volumes.go:58–210` exposes `GET /api/volumes`, `POST /api/volumes`, `GET /api/volumes/{id}`, `DELETE /api/volumes/{id}`, and `PUT/DELETE /api/volumes/{id}/pin`. The router wires these in `tierd/internal/api/router.go:69-70`.

The rest of the proposal — heat-aware fields on existing endpoints, the resize endpoint, the policy API, and the tier-level management API — was never built. This follow-up tracks the remainder.

---

## Remaining work

### 1. Heat fields on `GET /api/volumes` and `GET /api/volumes/{id}`

Extend the response objects to include the heat-engine fields specified in the original proposal:

- `bytes_by_tier` — map of tier name → bytes currently placed on that tier (computed from `managed_volume_regions`).
- `spilled_bytes` — bytes whose `current_tier` differs from the tier at the top of the placement intent.
- `active_migration` — `null`, or `{ "from_tier": ..., "to_tier": ..., "bytes": ..., "started_at": ... }` derived from regions whose `migration_state IN ('queued','in_progress','verifying')`.

These computations belong in a new helper in `tierd/internal/db/managed_volumes.go` or in a new `tierd/internal/heat` package — not inline in the handler.

### 2. `PUT /api/volumes/{id}` resize endpoint

This endpoint is also called for in `mdadm-heat-engine-02-managed-volumes-remainder`. It is listed here as well because it is part of the API surface contract. Implementation lives in the volumes follow-up; this proposal is satisfied when the route exists, validates input, and returns the documented response shape.

### 3. Policy API

Implement two new endpoints under `/api/tiers/policy`:

- `GET /api/tiers/policy` — returns the global `tier_policy_config` row, falling back to documented defaults if no row exists. Backed by `db.GetTierPolicyConfig` (`tierd/internal/db/managed_volumes.go:365`).
- `PUT /api/tiers/policy` — accepts the full policy config object. Validates each field's range (poll_interval_minutes ≥ 1, evaluation_interval_minutes ≥ 1, hysteresis_cycles ≥ 0, target_fill_percent within 0–100, ema_alpha within 0–1, etc.). Persists via `db.UpsertTierPolicyConfig`.

Both endpoints must be wired into `router.go`. Add a unit test in `tierd/internal/api` covering happy-path GET, happy-path PUT, validation rejection, and round-trip (PUT then GET).

### 4. Tier-level management API

Today `tierd/internal/api/tiers.go` has a skeleton for routing tier levels but no working endpoints. Implement:

- `GET /api/tiers/{name}` — extend the existing tier detail response to include `region_size_mb` and a `levels` array. Each level entry must include `level_name`, `rank`, `target_fill_percent`, `full_fill_percent`, `bytes_total`, `bytes_used`, and `bytes_free` aggregated from the PVs in that level.
- `POST /api/tiers/{name}/levels` — add a new tier level to an existing tier. Body: `{ "level_name": ..., "rank": ..., "target_fill_percent": ..., "full_fill_percent": ... }`. Validates `rank` is unique within the tier and `target_fill_percent < full_fill_percent`. Persists via the existing `tier_levels` table.
- `PUT /api/tiers/{name}/levels/{level_name}` — update fill percentages on an existing level. Same validation rules as POST.
- `DELETE /api/tiers/{name}/levels/{level_name}` — remove a tier level. Must reject (HTTP 409) if any PV is currently assigned to that level.

All four endpoints belong in `tierd/internal/api/tiers.go` (or a new `tier_levels.go` if `tiers.go` becomes unwieldy).

### 5. Tests

- `tierd/internal/api/volumes_test.go` (or extend existing test): assert the new heat fields appear on `GET /api/volumes` once region rows exist.
- `tierd/internal/api/policy_test.go` (new): GET defaults, PUT happy path, PUT validation rejection.
- `tierd/internal/api/tier_levels_test.go` (new): full CRUD on tier levels including the rank-uniqueness and PV-still-assigned rejections.

---

## Acceptance criteria

- `GET /api/volumes` and `GET /api/volumes/{id}` return `bytes_by_tier`, `spilled_bytes`, and `active_migration` fields.
- `GET /api/tiers/policy` returns the current config or documented defaults; `PUT /api/tiers/policy` validates and persists.
- `GET /api/tiers/{name}` includes `region_size_mb` and a `levels` array with usage stats.
- `POST/PUT/DELETE /api/tiers/{name}/levels[/{level_name}]` create, update, and delete tier levels with the validation rules above.
- All new endpoints have unit tests in `tierd/internal/api/`.
- No regression in existing volume CRUD endpoints.

---

## Out of scope

- The volume resize implementation itself (covered by `mdadm-heat-engine-02-managed-volumes-remainder`).
- Migration enqueue/dequeue endpoints (the migration engine runs entirely server-side per `mdadm-heat-engine-05-migration-engine`).
- UI changes (covered by `mdadm-heat-engine-08-ui`).
