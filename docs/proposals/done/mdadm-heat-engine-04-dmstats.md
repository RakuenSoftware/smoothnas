# Proposal: mdadm Heat Engine — dm-stats Heat Sampling

**Status:** Done
**Part of:** mdadm-complete-heat-engine (Step 4 of 9)
**Depends on:** mdadm-heat-engine-03-region-inventory

---

## Problem

The heat engine needs per-region I/O measurements to decide which regions to promote or demote. Without a heat source, all regions look identical and the policy engine cannot make heat-based placement decisions. dm-stats is the standard Linux mechanism for per-region I/O accounting on device-mapper targets and is the correct tool here.

---

## Specification

### dm-stats overview

`dmsetup stats` and `dmstats` manage I/O statistics regions on device-mapper targets. Each dm-stats region corresponds to a byte range on a DM device. When stats are enabled, the kernel accumulates read and write counts per region. The daemon reads these counters, computes deltas, and derives a heat score per managed volume region.

### DM device path

For a managed LV at `{vg_name}/{lv_name}`, the DM device path is:

```
/dev/mapper/{vg_name}-{lv_name}
```

(with hyphens in vg/lv names doubled to avoid ambiguity per LVM convention).

Obtain the canonical path at startup by calling `dmsetup info -c --noheadings -o blkdevname {vg}-{lv}` and caching the result.

### dm-stats package

Add `tierd/internal/dmstats` with:

```go
type Client struct { /* exec wrapper */ }

func (c *Client) ListRegions(ctx context.Context, devPath string) ([]DMStatsRegion, error)
func (c *Client) CreateRegion(ctx context.Context, devPath string, startSector, lengthSectors uint64) (int, error)
func (c *Client) DeleteRegion(ctx context.Context, devPath string, regionID int) error
func (c *Client) DeleteAllRegions(ctx context.Context, devPath string) error
func (c *Client) ReadCounters(ctx context.Context, devPath string) ([]DMStatsCounter, error)

type DMStatsRegion struct {
    RegionID      int
    StartSector   uint64
    LengthSectors uint64
}

type DMStatsCounter struct {
    RegionID   int
    ReadIOs    uint64
    WriteIOs   uint64
}
```

All methods wrap `dmstats` CLI calls. No CGo or kernel module binding is used.

### Lifecycle: enable stats for a volume

When a managed volume becomes active (create or startup), call `EnsureStatsRegistered`:

1. Call `ListRegions` to obtain existing dm-stats regions for the device.
2. Compare existing regions to the expected set derived from `managed_volume_regions`:
   - Expected region count: one dm-stats region per managed volume region.
   - Expected start: `region_offset_bytes / 512` (sectors).
   - Expected length: `region_size_bytes / 512`.
3. For missing regions, call `CreateRegion` and insert a row into `dmstats_regions` with the returned `dmstats_region_id`.
4. For extra regions (e.g. left by a previous resize), call `DeleteRegion`.
5. After reconciliation, the `dmstats_regions` table is the source of truth for region ID mapping.

### Lifecycle: disable stats for a volume

When a managed volume is deleted, call `DeleteAllRegions` before removing the LV. Clean up all `dmstats_regions` rows (cascade handles this via FK).

### Sampling loop

The sampler runs on the configured `poll_interval_minutes` from `tier_policy_config`.

For each active managed volume:

1. Call `ReadCounters` to get current cumulative `ReadIOs` and `WriteIOs` per dm-stats region ID.
2. For each region, look up the previous counters from `dmstats_regions` (`last_read_ios`, `last_write_ios`).
3. Compute the delta: `delta_iops = (ReadIOs - last_read_ios) + (WriteIOs - last_write_ios)`.
4. Handle counter rollover (if new value < old value, treat delta as the new value).
5. Update the rolling heat score on the corresponding `managed_volume_regions` row.
6. Update `dmstats_regions.last_read_ios`, `last_write_ios`, `last_sampled_at`.

### Rolling heat score

```
new_score = (old_score * (window_slots - 1) + delta_iops) / window_slots
```

where `window_slots = rolling_window_hours * 60 / poll_interval_minutes`.

This is an exponential moving average (EMA) over the configured rolling window. It does not require storing historical sample rows.

Update `managed_volume_regions.heat_score` and `heat_sampled_at` after each sample.

### Counter continuity across restart

On startup, before the first sample, load `last_read_ios` and `last_write_ios` from `dmstats_regions`. The first delta after restart uses the persisted baseline, not zero, so a restart does not produce a spurious heat spike.

### Sector size assumption

Assume 512-byte sectors for dm-stats region boundaries. This matches device-mapper's internal accounting regardless of the underlying hardware sector size.

---

## Acceptance Criteria

- [ ] `dmstats` package wraps `dmstats list`, `dmstats create`, `dmstats delete`, and `dmstats report` CLI calls.
- [ ] `EnsureStatsRegistered` reconciles dm-stats regions to match the managed volume's region inventory after every create or startup.
- [ ] Deleting a managed volume calls `DeleteAllRegions` before removing the LV.
- [ ] The sampler runs on the configured `poll_interval_minutes` interval.
- [ ] Per-region deltas are computed correctly, including rollover handling.
- [ ] Heat scores are updated as an EMA over the `rolling_window_hours` window.
- [ ] `dmstats_regions.last_read_ios` and `last_write_ios` are persisted after each sample.
- [ ] On restart, the first sample uses the persisted baseline and does not produce a spurious heat spike.
- [ ] If `dmstats` is not available on the system, the sampler logs a WARNING on startup and skips sampling (heat scores remain 0, policy engine still runs but treats all regions as equally cold).
