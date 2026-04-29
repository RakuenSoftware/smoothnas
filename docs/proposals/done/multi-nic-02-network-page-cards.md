# Proposal: Multi-NIC Phase 2 — Network Page Card Layout (Read-Only)

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-01-default-bond-policy.md`](multi-nic-01-default-bond-policy.md)

---

## Context

Phase 1 wired the backend default-bond policy at tierd startup so a
fresh appliance auto-creates `bond0` over every physical Ethernet
NIC. The Network page in the UI was still the pre-multi-NIC flat
list (Hostname / Interfaces / Bonds / VLANs as separate sections,
no shape).

This phase reshapes the page into the four-card layout the
proposal describes, read-only, using only the existing endpoints.
Edit affordances (Edit IP, Change Mode, Break Bond, Re-create
Bond, Add VLAN) land in Phase 3+.

## Scope

1. **System** card — hostname, DNS, default route. Pulls from
   `/api/network/hostname`, `/api/network/dns`, and
   `/api/network/routes` (with a fallback to interface-level
   `gateway4` if routes haven't materialised on a fresh box).
2. **Active topology** card — two shapes:
   - Default-bond shape: bond name, mode badge, IP + IP-mode
     (DHCP / Static), per-member rows with link state, speed,
     MAC. Plus an explanatory note on per-stream / per-NIC
     pinning and the operator path to single-stream aggregation.
   - Broken-bond shape: independent NIC table with link, speed,
     IP, IP-mode, MAC.
3. **VLANs** card — existing list, with IP column.
4. **Multi-flow status** card — counts the number of IPs the
   topology exposes (one path per bond, or per-NIC if broken)
   and shows placeholder rows for SMB Multichannel / NFS multi-
   path / iSCSI portals that read "probe lands in a future
   phase". Phase 6 fills these in.

The pending-change banner (`/api/network/pending`) and Refresh
button stay where they were.

## Acceptance Criteria

- [x] Network page renders four cards (System, Active topology,
      VLANs, Multi-flow status) in that order.
- [x] Active topology shows the default bond + member rows when
      a bond exists; falls back to an independent-NICs table
      when no bonds are configured.
- [x] VLANs and bond device names are filtered out of the
      independent-NICs view so a broken-bond box doesn't show
      its own (now-absent) bond as a phantom physical NIC.
- [x] DNS / hostname / default-route values come from the
      existing endpoints; no new backend.
- [x] Multi-flow status counts the number of IP-bearing
      topologies and tells the operator when only one path is
      exposed.
- [x] `tsc -b` passes.

## Out of scope (later phases)

- Edit-IP form (Phase 3).
- Change Mode (Phase 4).
- Break Bond / Re-create Bond (Phase 5).
- SMB Multichannel + NFS multi-path + iSCSI portal probes
  populating the Multi-flow status card (Phase 6).
- Per-NIC stats drill-down (Phase 7).
- Add VLAN form + static-route polish (Phase 8).
