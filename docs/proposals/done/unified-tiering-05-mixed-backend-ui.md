# Proposal: Unified Tiering — 05: Mixed-Backend UI and Coordinated Snapshots

**Status:** Pending
**Date:** 2026-04-09
**Updated:** 2026-04-12
**Depends on:** unified-tiering-02-mdadm-adapter, unified-tiering-04-zfs-managed-adapter
**Part of:** unified-tiering-control-plane
**Preceded by:** unified-tiering-04b-zfs-managed-adapter
**Followed by:** unified-tiering-06-coordinated-snapshots

---

## Problem

With both the mdadm adapter (proposal 02) and the managed ZFS adapter (proposal 04) shipping, the unified tiering GUI needs to present targets from both backends in one inventory without misleading operators into thinking the backends are equivalent. Additionally, the managed ZFS adapter (proposal 04) ships with `SnapshotMode = none` because coordinated multi-dataset snapshots were deferred. This proposal delivers both.

---

## Goals

1. Show mdadm and managed ZFS tier targets in one tiering inventory, grouped by placement domain.
2. Keep capability badges prominent so operators can immediately distinguish movement granularity, recall behavior, and snapshot support.
3. Preserve backend-specific deep admin pages (arrays, tiers, pools, datasets) as first-class.
4. Keep policy evaluation and movement domain-scoped.
5. Provide navigation links between the unified inventory and backend-specific admin pages.

---

## Non-goals

- Changes to the mdadm or ZFS backend data planes.
- New tiering policy features beyond what proposals 01–04 define.
- Cross-domain automatic movement.
- Coordinated namespace snapshots (covered by proposal 06).

---

## GUI: Placement-Domain Grouping

Targets in the tiering inventory are grouped by `placement_domain`. Each domain is a collapsible section with a header showing:

- domain name
- backend kind
- aggregate health (degraded if any member target is degraded)
- aggregate used and capacity

Within a domain, targets are sorted by rank ascending. Rank numbers are shown relative to the domain only — no visual treatment should imply that `rank=1` in one domain is comparable to `rank=1` in another.

Operators may filter the inventory to a single domain. The default view shows all domains.

When a movement job spans two targets, both must be in the same domain. The GUI must not present a cross-domain movement option.

---

## GUI: What Is Shown by Default

Each target row shows:

- target name
- backend kind
- placement domain
- rank
- used and capacity
- target fill and full threshold
- health
- activity band
- queue depth
- movement granularity badge
- pin capability badge

---

## GUI: What Is Shown for Advanced Users

Expanding a target row reveals:

- backend-native heat detail
- recall behavior and recall mode (synchronous or asynchronous)
- FUSE mode (passthrough or fallback), for managed ZFS targets
- snapshot support model
- checksum support
- compression support
- backing pool or tier references
- degraded-state details

---

## GUI: What Must Not Be Implied

The GUI must not imply:

- that rank is globally interchangeable across backends
- equal heat metrics across backends
- equal movement latency across backends
- equal snapshot semantics across backends
- that direct raw ZFS dataset access remains supported for managed ZFS tiering namespaces
- that a movement can occur between a target in one domain and a target in another

---

## Degraded State Presentation

Degraded states surface in the unified tiering view as follows:

- **Domain header badge**: a coloured dot (yellow = warning, red = critical) is shown on the domain section header if any member target has an active degraded state. The badge shows the count of active critical states.
- **Target row inline indicator**: a warning or critical icon is shown inline on any target row with an active degraded state. Hovering (or tapping) the icon shows the degraded-state code and message.
- **Dedicated degraded states panel**: the bottom of the tiering page shows a collapsible panel listing all active degraded states across all domains, sourced from `GET /api/tiering/degraded`. Each row shows: backend kind, domain, scope ID, severity, code, message, and `updated_at`.

Resolved degraded states (with `resolved_at` set) do not appear in the unified view. Backend-specific degraded states (e.g. mdadm array health) continue to surface in their respective backend admin pages and in `/api/health`.

---

## Movement Workflow

The unified tiering view exposes movement status and cancellation but does not expose manual movement initiation. Movements are planned and started by the control-plane scheduler based on policy. This is intentional: manual movement initiation across heterogeneous backends with different granularities (region vs file) would require backend-specific UI that duplicates the backend admin pages.

What the unified view does show:

- **Active movements panel**: a list of `running` and `pending` movement jobs sourced from `GET /api/tiering/movements`. Each row shows: namespace name, source target, destination target, backend kind, progress (bytes moved / total bytes), triggered-by, and started_at.
- **Cancel button**: each running job has a cancel button that calls `DELETE /api/tiering/movements/{id}`. The UI confirms before sending the request. After cancellation, the job row shows `cancelled` state until the next page refresh.
- **Movement history**: a toggle reveals recently completed movement jobs (last 30 days). Failed jobs show the failure reason.

---

## Movement History Retention

The movement history toggle shows completed movement jobs from the last 30 days, matching the `movement_jobs` purge window defined in proposal 01. The UI must not assume jobs are retained longer than 30 days.

`failed` jobs show the failure reason. `stale` jobs that were never replanned show the reason `stale_timeout`. `cancelled` jobs show `cancelled by operator` or `cancelled by pin`.

---

## Navigation

Each target row in the unified tiering view includes a link to the backend-specific admin page for that target:
- mdadm targets link to the `Arrays` → `Tiers` page for the relevant tier instance.
- Managed ZFS targets link to the `Pools` page for the pool that contains the tier's backing datasets. Because a single managed ZFS namespace spans multiple tier datasets all within the same pool (enforced at namespace creation time), there is always exactly one backing pool to link to.

The link is labelled "Manage in [backend name]" and opens the backend page in the same tab.

---

## Coordinated Snapshot UI

For managed ZFS namespaces, snapshot functionality is delivered in proposal 06. Until proposal 06 ships, no Snapshot button appears. If `SnapshotMode = none` due to multi-pool backing layout, the namespace detail panel shows an informational note: "Coordinated snapshots require all tier datasets to be in the same ZFS pool."

---

## Effort

**M** — GUI grouping layer is S; movement workflow, degraded state presentation, and navigation are S each. Total is M.

---

## Acceptance Criteria

- [ ] The unified tiering GUI shows mdadm and managed ZFS targets in one inventory grouped by placement domain.
- [ ] Each domain group is collapsible and shows aggregate health and capacity.
- [ ] Targets are sorted by rank within their domain section.
- [ ] Capability badges (movement granularity, recall, FUSE mode, snapshot mode) are visible on each target row.
- [ ] The GUI does not present a cross-domain movement option.
- [ ] Backend-specific deep admin pages (arrays, tiers, pools, datasets, zvols, snapshots) remain accessible and first-class.
- [ ] The unified tiering view shows a degraded-state panel listing all active degraded states with code, message, backend kind, and severity.
- [ ] Domain headers show warning/critical badge counts when member targets have active degraded states.
- [ ] The active movements panel shows running and pending movement jobs with cancel buttons.
- [ ] Each target row includes a "Manage in [backend]" link to the backend-specific admin page.
- [ ] Movement history shows jobs from the last 30 days only.
- [ ] Managed ZFS "Manage in" link navigates to the pool page for the single backing pool.
- [ ] Snapshot button and snapshot list panel are absent from this proposal; they appear only after proposal 06 ships.

## Test Plan

- [ ] UI tests: targets in the same domain appear together in the inventory; targets in different domains are in separate collapsible sections.
- [ ] UI tests: rank numbers are shown per domain; no UI element implies cross-domain rank equivalence.
- [ ] UI tests: capability badges for movement granularity, recall behavior, and FUSE mode are visible and correct per backend.
- [ ] UI test: cross-domain movement option is absent from the movement UI.
- [ ] UI test: movement history shows jobs up to 30 days old; older jobs are absent.
- [ ] UI test: managed ZFS "Manage in" link navigates to the correct pool page.
- [ ] UI test: degraded-state panel shows all active states from `/api/tiering/degraded`; resolved states are absent.
- [ ] UI test: domain header shows critical badge count when a member target has a critical degraded state.
- [ ] UI test: active movements panel shows running jobs with cancel button; cancelled job shows `cancelled` state.
- [ ] UI test: "Manage in mdadm" link navigates to the correct Tiers page.
- [ ] UI test: no Snapshot button appears on any namespace before proposal 06 ships; multi-pool namespace shows the informational note.
