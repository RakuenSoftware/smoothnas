# Proposal: mdadm Heat Engine UI Remainder

**Status:** Done
**Split from:** [`mdadm-heat-engine-08-ui.md`](../done/mdadm-heat-engine-08-ui.md)

---

## Context

The original heat-engine UI proposal is partly delivered: tier level data is exposed through `/api/tiers/{name}/levels`, the Tiers page renders tier-level capacity/fill rows, and inline target/full-threshold editing is present.

The operator-facing volume workflow, heat-policy panel, and dashboard summary cards are now complete against the current smoothfs-aware managed-namespace API. Legacy mdadm managed LVs remain retired; volume lifecycle entry points route operators to tier, smoothfs pool, and sharing workflows while the Volumes page exposes namespace placement, pin state, file placement, and movement state.

## Scope

1. Add a `/volumes` UI route and navigation entry.
2. Build the managed-volume list and detail views against the existing `/api/volumes` contract if it is restored, or against the replacement smoothfs-aware API if mdadm managed volumes remain deprecated.
3. Support create, resize, pin/unpin, and delete actions with inline API errors.
4. Render the per-region heat map, including in-progress migration and spill overlays.
5. Add the global heat-engine policy panel to the Tiers page once the policy endpoint is present.
6. Add Dashboard storage cards for active migrations, migration backlog, and tiers near spillover.

## Acceptance Criteria

- [x] Operators can navigate to a volume list from the sidebar.
- [x] The volume detail page shows managed-file placement and active migration state.
- [x] Volume create/resize/delete workflows are explicitly retired with tier, smoothfs pool, and sharing replacement workflows; namespace pin/unpin is wired.
- [x] The heat-engine policy fields can be viewed and saved from the UI.
- [x] Dashboard storage cards summarize active migrations, migration backlog, and near-spillover tiers.
