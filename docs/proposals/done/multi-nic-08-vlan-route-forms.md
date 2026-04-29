# Proposal: Multi-NIC Phase 8 — VLAN Form + Static Route Polish

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-07-per-nic-stats.md`](multi-nic-07-per-nic-stats.md)

---

## Context

Phases 1–7 wired the default bond, the four-card GUI, the
Edit-IP form, the Change Mode action, Break Bond / Re-create
Bond, the Multi-flow status card, and the per-NIC stats drill-
down. The proposal's last phase finishes the network-page
operator surface: an Add VLAN form (the existing VLAN list was
read-only in Phase 2) and a real Static-routes card with
add/delete affordances.

Backend support for both already exists: the Phase 1+ network
package has `VLANConfig`, `RouteConfig`, the `createVLAN` /
`deleteVLAN` / route POST/DELETE handlers, and the safe-apply
flow each writes through. This phase is frontend-only.

## Scope

1. **`api.deleteVlan(name)`** — new frontend API helper over
   `DELETE /api/network/vlans/{name}` (POST + GET were already
   wired).
2. **VLAN add form** — opens from an "Add VLAN" button on the
   VLANs card. Fields:
   - Parent: dropdown populated from bonds + physical NICs (so
     a VLAN on `bond0` or on a per-NIC after Break Bond both
     work).
   - VLAN ID: 1–4094.
   - DHCP toggle + IPv4 CIDR + IPv4 gateway when DHCP is off.
   Client-side validation mirrors the backend's
   `ValidateVLANID` and the IP form rules.
3. **VLAN delete** — per-row Delete button gated by
   `window.confirm`.
4. **Static routes card** — new card between VLANs and
   Multi-flow status. Lists existing routes (destination,
   gateway, interface, metric) with per-row Delete. Add Route
   form with the four fields; "default" is accepted as a
   special destination matching the backend's
   `ValidateRouteCIDR`.

## Acceptance Criteria

- [x] Add VLAN form takes parent (bond or NIC), VID, IP config;
      writes via `VLANConfig` through the existing
      `POST /api/network/vlans` route.
- [x] VLAN delete reaches `DELETE /api/network/vlans/{name}`.
- [x] Static-route create / delete reaches `RouteConfig` via
      the existing `/api/network/routes` POST/DELETE handlers.
- [x] Both forms validate inputs client-side before submit.
- [x] `make test-frontend` (`tsc -b`) and `make lint` clean.

## Multi-NIC umbrella status

With Phase 8 landing, every phase from the
[`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
umbrella proposal is delivered:

| Phase | Slice | Doc |
|---|---|---|
| 1 | Default-bond policy at tierd startup | [`multi-nic-01-default-bond-policy.md`](multi-nic-01-default-bond-policy.md) |
| 2 | Network page card layout (read-only) | [`multi-nic-02-network-page-cards.md`](multi-nic-02-network-page-cards.md) |
| 3 | Edit-IP form (DHCP-or-static) | [`multi-nic-03-edit-ip-form.md`](multi-nic-03-edit-ip-form.md) |
| 4 | Bond mode change | [`multi-nic-04-bond-mode-change.md`](multi-nic-04-bond-mode-change.md) |
| 5 | Break Bond / Re-create Bond | [`multi-nic-05-break-bond.md`](multi-nic-05-break-bond.md) |
| 6 | Multi-flow status + SMB Multichannel | [`multi-nic-06-multi-flow-status.md`](multi-nic-06-multi-flow-status.md) |
| 7 | Per-NIC stats drill-down | [`multi-nic-07-per-nic-stats.md`](multi-nic-07-per-nic-stats.md) |
| 8 | VLAN form + static route polish | (this doc) |

Deferred sub-phases (none block the umbrella):

- Pre-bond per-NIC static-config persistence (proposal §9.2's
  "unless a previous static config was saved" parenthetical).
  v1 ships DHCP-default on Break Bond.
- LIO-side per-portal-IP fan-out at write time (Phase 6 surfaces
  the would-be portal count; the actual targetcli fan-out is
  operator-driven).
