# Proposal: mdadm Heat Engine — UI

**Status:** Done — partial delivery archived; remaining UI work split to
[`mdadm-heat-engine-ui-remainder.md`](../pending/mdadm-heat-engine-ui-remainder.md).
**Part of:** mdadm-complete-heat-engine (Step 8 of 9)
**Depends on:** mdadm-heat-engine-07-api

---

## Problem

The heat engine has no operator-facing UI. Without frontend pages for volumes, heat maps, migration status, and policy configuration, operators cannot see or manage the heat engine's state. All heat engine data is invisible through the current web interface.

---

## Specification

### Volumes page

Add a new top-level route at `/volumes`. Add a link to the navigation sidebar between Tiers and Sharing.

The Volumes page has two views: a list view (default) and a detail view for a single volume.

#### Volume list view

Show a table with one row per managed volume across all tier instances:

| Column | Content |
|--------|---------|
| Pool | Tier instance name |
| Name | `lv_name` |
| Size | Human-readable `size_bytes` |
| Mount | `mount_point` |
| Placement | Stacked bar showing `bytes_by_tier` as coloured segments (NVME = blue, SSD = green, HDD = amber; custom tiers use auto-assigned colours by rank) |
| Heat | Aggregate heat indicator (average `heat_score` across all regions — rendered as a 5-bar heat icon) |
| Pinned | Lock icon when `pinned = true` |
| Migration | Active migration badge with percentage complete when any region is `in_progress` |
| Actions | Resize, Pin/Unpin, Delete buttons |

**Create volume button:** Opens a modal with fields for Pool, Name, Size (MiB), and Filesystem. Submits to `POST /api/volumes`. Refreshes the list on success.

**Resize action:** Opens a modal pre-filled with the current size. Validates that the new size is larger. Submits to `PUT /api/volumes/{id}`.

**Pin/Unpin action:** Sends `PUT /api/volumes/{id}/pin` or `DELETE /api/volumes/{id}/pin` with an optimistic UI update.

**Delete action:** Shows a confirmation dialog. Disabled (greyed out with tooltip) if the volume is pinned or has active migrations. Sends `DELETE /api/volumes/{id}`.

#### Volume detail view

Clicking a row navigates to `/volumes/{id}`.

Show the same summary fields as the list view, plus a region heat map.

**Region heat map:** A horizontal strip of equal-width cells, one per region. Cells are coloured by heat score using a gradient from grey (cold, score = 0) to red (hot, max observed score). Hovering a cell shows a tooltip with:

- Region index
- Current tier
- Heat score
- Migration state
- Last moved at / last movement reason

Cells with `migration_state = in_progress` show an animated pulse overlay. Cells with `spilled = true` show a diagonal stripe overlay.

The heat map is capped at 500 cells visible at once. If the volume has more than 500 regions, cells are grouped into buckets and the maximum heat score in each bucket is displayed.

Below the heat map, show a migration history table from the last 50 `done` or `failed` region transitions (sourced from `managed_volume_regions.last_movement_at` and related fields).

---

### Tiers page updates

The Tiers page already exists and shows tier instances. Extend it with two additions.

#### Tier level table

For each tier instance, add a collapsible "Tier Levels" section below the existing content. Show a table:

| Column | Content |
|--------|---------|
| Name | Level name (NVME, SSD, HDD, or custom) |
| Rank | Rank integer |
| Array | `array_path` |
| Capacity | Human-readable capacity bytes |
| Used | Used bytes and fill percentage bar |
| Target fill | `target_fill_pct`% with an inline edit field |
| Full threshold | `full_threshold_pct`% with an inline edit field |

Inline editing on Target fill and Full threshold sends `PUT /api/tiers/{name}/levels/{level_name}`.

An "Add level" button at the bottom of the table opens a modal with Name, Rank, Array path, Target fill, and Full threshold fields. Submits to `POST /api/tiers/{name}/levels`.

A delete button on each row sends `DELETE /api/tiers/{name}/levels/{level_name}`. Disabled with a tooltip if regions still reside on that tier.

#### Policy panel

Add a "Heat Engine Policy" collapsible panel to the Tiers page (not per-tier — one global panel). Show all fields from `GET /api/tiers/policy` as editable inputs:

| Field | Input type | Label |
|-------|-----------|-------|
| `poll_interval_minutes` | Number | Sampling interval (minutes) |
| `rolling_window_hours` | Number | Heat rolling window (hours) |
| `evaluation_interval_minutes` | Number | Policy evaluation interval (minutes) |
| `consecutive_cycles_before_migration` | Number | Cycles before migration |
| `migration_reserve_pct` | Number | Migration free-space reserve (%) |
| `migration_iops_cap_mb` | Number | Migration throughput cap (MB/s) |
| `migration_io_high_water_pct` | Number | I/O high-water deferral threshold (%) |

A "Save policy" button sends `PUT /api/tiers/policy` with all fields. Validation errors from the API are shown inline below the relevant field.

---

### Dashboard additions

Extend the existing Dashboard with three new metric cards in the storage section:

**Active migrations:** Count of regions currently in `in_progress` state across all volumes. Clicking navigates to the Volumes page filtered to volumes with active migrations.

**Migration backlog:** Count of regions in `queued` state. Shown alongside active migrations.

**Tiers near spillover:** Count of tier levels where `used_bytes / capacity_bytes >= full_threshold_pct * 0.9` (i.e. within 10% of full threshold). Each such tier is shown as a warning badge. Clicking expands a list of the at-risk tiers with their current fill percentage and threshold.

---

## Acceptance Criteria

- [ ] A `/volumes` route exists and is linked from the navigation sidebar.
- [ ] The volume list shows all managed volumes with placement bar, heat indicator, and migration badge.
- [ ] Create, resize, pin, unpin, and delete actions work and surface API error messages to the operator.
- [ ] The volume detail view shows a region heat map with per-region tooltips.
- [ ] Heat map cells show migration-in-progress and spilled overlays.
- [ ] The Tiers page shows a tier level table per tier instance with inline fill-percentage editing.
- [ ] The Tiers page shows the global policy panel and allows saving all policy fields.
- [ ] The Dashboard shows active migration count, migration backlog, and tiers-near-spillover with clickable navigation.
- [ ] All new UI components use the existing component library and colour tokens — no new design system dependencies.
