# Proposal: Multi-NIC Phase 5 — Break Bond / Re-create Bond

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-04-bond-mode-change.md`](multi-nic-04-bond-mode-change.md)

---

## Context

Phases 1–4 wired the default bond, the four-card GUI, the Edit-IP
form, and the Change Mode action. With Break Bond / Re-create
Bond an operator can flip between the single-IP shape (default
bond) and the multi-IP shape (independent NICs) the proposal
describes.

The single-IP shape gives parallel-client throughput at line rate
per NIC. The multi-IP shape exposes N separate paths so MPIO-aware
clients can multi-path. Both are valid steady states; the
operator picks whichever matches the workload.

## Scope

1. **Backend `network.BreakBond(networkDir, name, members)`** —
   removes the bond's `.netdev` + `.network` files (handles both
   the operator-bond `05-`/`10-` shape and the appliance default
   bond's `90-default-` shape), removes the catch-all
   `99-default-bond-members.network` so newly-plugged NICs don't
   try to rejoin a now-absent bond, and rewrites each member's
   `10-<member>.network` as a DHCPed standalone interface. The
   `network.bootstrap_complete` marker is left set so Phase 1's
   startup reconcile is a no-op after Break Bond.
2. **Backend `network.RecreateDefaultBond(store, networkDir, sysRoot)`** —
   bypasses the bootstrap-marker gate, removes every per-NIC
   `10-<name>.network` file, and rewrites the default-bond files
   (`90-default-bond0.netdev`, `90-default-bond0.network`,
   `99-default-bond-members.network`).
3. **Backend `writeDefaultBondFiles` helper** factored out of
   `ApplyDefaultBondPolicy` so both the bootstrap and the
   operator-driven recreate path share the file-write logic.
4. **API routes:**
   - `POST /api/network/bonds/{name}/break` (handler `breakBond`)
   - `POST /api/network/default-bond/recreate` (handler
     `recreateDefaultBond`)
   Both wrap the network helper in the existing
   `safeApply.Apply` pending-confirm flow.
5. **`NewNetworkHandler` now takes `*db.Store`** so the recreate
   path can re-use the bootstrap marker. The router instantiation
   was the only call site.
6. **Frontend buttons:**
   - **Break Bond** on the bond row, gated by a confirm dialog.
   - **Re-create Bond** on the independent-NICs view footer, also
     confirm-gated, with a hint that the action is destructive
     (drops every per-NIC IP).
7. **`api.breakBond` / `api.recreateDefaultBond`** helpers added.

## Acceptance Criteria

- [x] `Break Bond` removes the bond's netdev + network files
      (both prefix shells), drops the bond IP, removes the
      default-bond catch-all, and writes per-member
      `InterfaceConfig` records (DHCP each).
- [x] `Re-create Bond` rebuilds the default-bond policy across
      every physical NIC, dropping the per-NIC IPs.
- [x] Bootstrap marker stays set across Break Bond so Phase 1's
      startup reconcile doesn't auto-recreate the bond after a
      tierd restart.
- [x] Bond name + member name validation runs before any file
      mutation.
- [x] Both endpoints go through the safe-apply / pending-confirm
      flow; the change is rolled back if the operator's session
      can't reach tierd inside the window.
- [x] Frontend buttons are confirm-gated with a description of
      what the action does and what's reversible.
- [x] `make test-backend`, `make test-frontend` (`tsc -b`), and
      `make lint` are clean.

## Pre-bond static-config persistence (deferred)

The proposal's parenthetical "(DHCP each unless a previous static
config was saved)" allows DHCP-default in the v1 implementation.
Tracking the operator's pre-bond per-NIC config would require
either parsing systemd-networkd files (fragile) or adding a
parallel tierd-owned per-NIC config table; either is more change
than this slice carries. If operator pain materialises (a
common-enough sequence of "set static IPs, build a custom bond
that consumes them, decide to break it"), the persistence layer
can land as a small follow-on without changing the BreakBond
contract.

## Out of scope (later phases)

- SMB Multichannel + NFS multi-path + iSCSI portal probes
  populating the Multi-flow status card (Phase 6).
- Per-NIC stats drill-down (Phase 7).
- Add VLAN form + static-route polish (Phase 8).
