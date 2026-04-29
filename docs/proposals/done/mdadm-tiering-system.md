# Proposal: mdadm Multi-Tier Storage System

**Status:** Pending
**Depends on:** storage-tiering (mdadm backend must be implemented first)

---

## Problem

The existing mdadm backend is fixed at two tiers: one SSD array acting as a dm-cache write-back cache in front of one HDD origin array. This covers the common case, but leaves a gap for operators who have more diverse hardware (e.g. NVMe drives alongside SSDs alongside HDDs) or who need to assign different workloads to different storage classes without the overhead of a cache relationship. There is currently no way to:

- Use NVMe as an additional fast tier above the SSD cache
- Create multiple independent HDD arrays at different RAID levels for different workloads and treat them as distinct tiers
- Express a storage placement preference for a volume ("put this LV on SSD, not HDD")
- See a unified view of how disks are grouped into performance tiers

---

## Goals

1. Allow operators to define named storage tiers, each backed by one mdadm array
2. Support three or more tiers simultaneously (e.g. NVMe, SSD, HDD)
3. Allow LVM logical volumes to be explicitly placed on a chosen tier
4. Provide a clear UI view of tiers, their disk membership, and their capacity
5. Support migration of a volume from one tier to another

---

## Non-goals

- Automatic/transparent background data migration based on heat or access frequency. All movement is either explicit (operator-initiated) or cache-based (dm-cache, already covered).
- File-level tiering or HSM. This operates at the block/LV level.
- ZFS tiering. ZFS has its own mechanisms (L2ARC, SLOG); this proposal is mdadm-only.
- Cross-host tiering or networked storage tiers.
- Changing the underlying mdadm or LVM commands — the implementation builds on what already exists.

---

## Background: Current Architecture

The current mdadm backend models exactly two roles:

| Role | Backed by | dm-cache relationship |
|------|-----------|-----------------------|
| `origin` | HDD mdadm array | Slow device; receives migrated dirty blocks |
| `cache` | SSD mdadm array | Fast device; absorbs writes in writeback mode |

These are joined by `lvconvert --type cache`, and the result is a single cached LV exposed to the filesystem. This is an opaque relationship — the two arrays are not independently usable once joined.

A tiering system would sit above (and alongside) this, giving arrays first-class identities as tiers, and LVs first-class placement.

---

## Options

### Option A: Named Tier Pools with Explicit Volume Placement

Each mdadm array is declared as a named tier. A volume group spans all tier arrays. When creating an LV, the operator picks which tier's physical extents to allocate from. dm-cache relationships between tiers are optional and separate.

```
Tier: "nvme"   → NVMe mdadm array → PV in VG → allocation tag "nvme"
Tier: "ssd"    → SSD mdadm array  → PV in VG → allocation tag "ssd"
Tier: "hdd"    → HDD mdadm array  → PV in VG → allocation tag "hdd"

LV "database"   allocated from "nvme" tag
LV "media"      allocated from "hdd"  tag
LV "workdir"    dm-cache: "ssd" cache over "hdd" origin (existing behavior)
```

**How it works**

LVM supports PV tags (`pvchange --addtag`) and LV allocation via `--alloc anywhere` with PV argument filtering. Tiers map cleanly to LVM PV tags. `tierd` tracks the tier-to-array mapping in the SQLite database and translates tier selection into PV arguments when calling `lvcreate`.

**Migration:** Volume migration between tiers is `pvmove /dev/md0 /dev/md1` (LVM built-in), run under the hood when the operator requests a tier change in the UI. The LV stays mounted during migration; LVM moves extents online.

**Pros**
- Clean conceptual model: each tier is an independently usable storage unit
- LVM `pvmove` handles live migration without unmounting
- No new kernel features required
- Easy to extend: add a tier by adding an array to the VG with a new tag
- Preserves existing dm-cache behavior as an opt-in on any pair of tiers

**Cons**
- All tiers must share a single volume group, which means all tier arrays are visible to LVM together. An operator error (accidentally allocating from the wrong PV) is only caught by `tierd`, not by the kernel.
- Resizing or removing a tier requires migrating all extents off it first (`pvmove` then `vgreduce`)
- The VG-spanning model may surprise operators who expect tiers to be isolated pools

---

### Option B: Separate Volume Groups Per Tier, with Cross-Tier Clone Migration

Each tier is its own VG. LVs live entirely within one tier's VG. There is no shared VG. Migration is a block-level copy (dd or lvmcopy) with a brief remount window.

```
VG "tier-nvme"  → NVMe mdadm array
VG "tier-ssd"   → SSD mdadm array
VG "tier-hdd"   → HDD mdadm array

LV "database"   in VG "tier-nvme"
LV "media"      in VG "tier-hdd"
```

**How it works**

Each tier is a completely isolated VG. Volume placement is the VG selection at `lvcreate` time. `tierd` tracks which VG corresponds to which tier in the SQLite DB.

Migration is explicit:
1. Create a new LV in the destination VG of the same size
2. Stop I/O to the source LV (unmount filesystem or quiesce iSCSI target)
3. `dd if=/dev/tier-ssd/vol of=/dev/tier-hdd/vol bs=4M` (or `lvmcopy` if available)
4. Remount from destination LV
5. Delete source LV

A UI migration workflow would walk the operator through this, showing progress and handling the brief downtime window.

**Pros**
- Tiers are completely isolated — no risk of cross-tier allocation accidents
- Each tier's VG can be independently managed (extended, removed, scrubbed)
- Cleaner mental model: tiers are pools, not tags
- Failure in one tier's array doesn't affect other tiers' VGs

**Cons**
- Migration requires a brief unmount (downtime for the LV), unlike Option A's live `pvmove`
- `dd`-based migration doesn't leverage LVM's internal extent tracking; it copies the full block device even if the LV is lightly used
- dm-cache relationships between tiers are harder to express (the cache LV and origin LV are in different VGs)
- More complex state management: `tierd` must track multiple VGs

---

### Option C: Stacked dm-cache (Transparent Multi-Level Caching)

Extend the existing dm-cache model to support two cache layers stacked on top of the HDD origin. NVMe caches SSD; SSD caches HDD. The operator sees one unified LV; data movement between tiers is fully automatic based on block access frequency.

```
LV (single)
  └── dm-cache: NVMe array (L1 cache, writeback)
        └── dm-cache: SSD array (L2 cache, writeback)
              └── HDD array (origin)
```

**How it works**

The existing mdadm backend already uses `lvconvert --type cache` to bind a cache pool to an origin LV. Stacking would apply the same operation twice: first bind the HDD origin with an SSD cache pool to produce a cached LV, then bind that cached LV with an NVMe cache pool.

dm-cache operates at the 4K block level; blocks promoted to NVMe are also present in SSD (L2) — the layers are independent caches, not exclusive tiers.

**Pros**
- Completely transparent to applications — one LV, no placement decisions
- dm-cache handles promotion/demotion automatically
- No migration plumbing needed
- Preserves the existing simple model (one LV = one volume)

**Cons**
- dm-cache stacking is not a supported or tested configuration; behavior under failure (e.g. NVMe dies while SSD cache is also dirty) is not well-defined
- Cache statistics (hit ratio, dirty blocks) would need to be tracked per layer, but `dmstats` merges them in confusing ways
- The operator has no control over which data lands where
- NVMe and SSD arrays would both need to be `role: cache` type, eliminating the ability to use them as independent writable tiers
- Stacking doubles the dm-cache metadata overhead and write amplification

---

### Option D: Tier-Aware Volume Scheduling (No LVM Changes, Policy-Only)

Keep the existing VG and LVM model unchanged. Add a tier concept purely in `tierd`'s database as metadata: label each mdadm array with a tier name, tier rank (0 = fastest), and disk type. When creating an LV, `tierd` uses tier labels to recommend which PV(s) to allocate from and enforces placement via PV filtering at `lvcreate` time. No `pvmove`, no stacked caches — just smarter allocation defaults.

Migration is handled the same as Option B (copy + remount) but with policy-aware scheduling: `tierd` can flag LVs as "misplaced" (a large infrequently-accessed LV on NVMe) and surface that as a UI recommendation without forcing action.

**Pros**
- Least invasive change to the existing architecture
- No new LVM structures; purely a `tierd` layer feature
- Tier metadata lives in the DB; easy to add/change labels without touching storage
- Can be introduced incrementally without disrupting existing arrays
- "Misplaced volume" recommendations add value without requiring automation

**Cons**
- Enforcement is soft: `tierd` controls allocation at creation time, but an operator who knows LVM commands can bypass the placement policy
- Does not address the desire for truly isolated tier pools
- Migration still requires downtime (same as Option B)
- No live migration path

---

## What Controls Movement Between Tiers

### Block-level movement (Option C only)

The existing dm-cache layer already moves data automatically at the **4K block level**, driven entirely by the kernel's dm-cache promotion/demotion algorithm. No user involvement; no policy configuration. This is the Option C model taken to its conclusion — fully transparent, but with no operator visibility or control over which data lives where.

### LV-level movement (Options A, B, D)

For options that treat placement as an explicit property of a logical volume, there are four possible triggers for movement. These are not mutually exclusive; a full implementation could support all of them.

**Decision: movement must be fully automatic.** The operator assigns disks to tiers; `tierd` handles all data placement and migration without requiring any further operator action. Storage Just Works.

This decision eliminates manual-only triggers and has significant implications for option selection — see [Implications for Options](#implications-for-options) below.

**1. I/O heat tracking**

`tierd`'s monitor goroutine already polls disk statistics. It tracks per-LV I/O over time (via `/sys/block/mdX/stat` combined with LVM extent mapping). A volume that has been cold for a configurable period is automatically migrated down to the next slower tier.

**2. Capacity pressure**

When a fast tier (NVMe, SSD) exceeds a fill threshold (e.g. 85%), `tierd` identifies the coldest volumes on that tier and migrates them down automatically. The fast tier is kept from filling by pushing cold data to slower tiers. This is the primary safety valve preventing a fast tier from silently becoming a bottleneck.

**3. Promotion on access**

When a volume on a slow tier sees a sustained I/O spike above a configurable threshold, `tierd` migrates it up to the next faster tier automatically. Volumes that become hot get faster storage without operator involvement.

**4. Scheduled policy evaluation**

`tierd`'s monitor evaluates heat and capacity rules on a configurable cadence (e.g. every hour). Migration is queued and executed in the background.

### The migration mechanism constrains full automation

For Options A/B/D, movement is either `pvmove` (live, zero-downtime, Option A only) or a copy + remount (requires a brief unmount window, Options B and D). The requirement for fully automatic, transparent migration means:

- **Option A (`pvmove`)** can satisfy the requirement completely — `pvmove` runs in the background with no impact on running shares or applications.
- **Options B and D (copy + remount)** cannot satisfy the requirement without disruption — unmounting a live SMB/NFS share or iSCSI target to copy it is not transparent to clients.
- **Option C (stacked dm-cache)** satisfies the requirement at the block level, but sacrifices all visibility and control over where data lives.

### Implications for Options

The "fully automatic, zero operator involvement" requirement is a strong constraint:

| Option | Satisfies fully automatic migration? | Notes |
|--------|--------------------------------------|-------|
| A (Shared VG + pvmove) | **Yes** | `pvmove` is live and non-disruptive; `tierd` can run it in the background |
| B (Separate VGs) | **No** | Copy + remount requires brief downtime; not transparent |
| C (Stacked dm-cache) | **Yes** (block level) | Fully transparent but no LV-level visibility or control |
| D (Policy-only) | **No** | Same copy + remount constraint as Option B |

Option A is the only explicit-placement option that can deliver fully automatic, transparent migration. If explicit LV-level placement and visibility are desired alongside full automation, **Option A is the only viable path**. Option C delivers full automation with no placement visibility at all.

---

## Comparison

| Concern | Option A (Shared VG + Tags) | Option B (Separate VGs) | Option C (Stacked Cache) | Option D (Policy-Only) |
|---------|----------------------------|------------------------|--------------------------|------------------------|
| Live migration | Yes (`pvmove`) | No (copy + remount) | N/A (transparent) | No (copy + remount) |
| Tier isolation | Soft (LVM tag enforcement) | Hard (separate VGs) | None (one LV) | Soft (allocation policy) |
| New kernel features needed | No | No | Risky (unsupported stacking) | No |
| Operator placement control | Explicit | Explicit | None | Explicit (at create time) |
| dm-cache compatibility | Yes (opt-in per LV) | Harder (cross-VG) | Yes (core mechanism) | Yes (unchanged) |
| Fully automatic migration | **Yes** | No (downtime) | Yes (block-level) | No (downtime) |
| LV-level visibility | Yes | Yes | No | Yes |
| Implementation complexity | Medium | Medium | High | Low |
| Migration downtime | None | Brief unmount | N/A | Brief unmount |
| Risk | Low | Low | High | Low |

---

## Open Questions

1. **Should tiers be user-named or fixed labels?** Fixed labels (e.g. "performance", "capacity", "archive") give the UI clear semantics. User-named tiers are more flexible but harder to present meaningfully.

2. **Should dm-cache be modeled as a tier relationship or kept as a separate concern?** In Options A and B, dm-cache is still a valid configuration between any two tier arrays. Keeping it separate avoids conflating "cache" (transparent, block-level) with "tier" (explicit, LV-level placement).

3. **How should the UI surface tiers?** Options include a dedicated Tiers page, integration into the existing Arrays page (with a tier label field), or a placement selector on the volume creation form.

4. **Is live migration (zero-downtime) a hard requirement?** If yes, Option A (shared VG + `pvmove`) is the only path. If brief unmount is acceptable, all other options are viable.

5. **Should tier rank drive any automatic behavior?** For example, auto-place new LVs on the highest-available tier unless the operator specifies otherwise.

6. ~~**Should movement ever be fully automatic, or always require operator confirmation?**~~ **Decided: fully automatic.** The operator assigns disks to tiers; `tierd` manages all placement and migration transparently. This eliminates Options B and D as viable paths for automatic migration, since their copy + remount mechanism cannot run without disrupting live shares.
