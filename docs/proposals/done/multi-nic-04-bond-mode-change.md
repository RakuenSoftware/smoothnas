# Proposal: Multi-NIC Phase 4 — Bond Mode Change

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-03-edit-ip-form.md`](multi-nic-03-edit-ip-form.md)

---

## Context

Phase 3 wired the Edit-IP form for the bond row and per-NIC rows.
This phase adds the dedicated Change Mode action so an operator
can flip the bond between `balance-alb` (default), `active-backup`
(simple failover), `802.3ad` (LACP, switch help required),
`balance-rr`, `balance-xor`, and `balance-tlb` without touching
the IP / DNS / MTU configuration.

Backend support already existed: `PUT /api/network/bonds/{name}`
accepts a full `BondConfig`, validates the mode through
`network.ValidateBondMode`, and writes both the `.netdev` and
`.network` files atomically through the safe-apply flow. The
piece this phase adds is the operator-facing UI plus the
guarantee that a static IP set on the bond survives a mode swap.

## Scope

1. **Frontend `BOND_MODES` constant** — list of accepted bond
   modes, kept in sync with `validBondModes` in
   `tierd/internal/network/bond.go`.
2. **`Change Mode…` button** on the bond row, between Edit IP
   and (a future-phase) Break Bond.
3. **Change Mode modal** — single-field form with the mode
   dropdown plus a hint that explains what each mode does and
   reminds the operator that IP / DNS / MTU survive the swap.
4. **`submitChangeMode` handler** — pre-loads the bond's full
   current state (IP, gateway, DHCP flags, MTU, DNS, members)
   and submits `PUT /api/network/bonds/{name}` with only the
   `mode` field changed. Mode-equals-current is a no-op.

## Acceptance Criteria

- [x] Bond row in the Active topology card surfaces a
      `Change Mode…` button.
- [x] Modal exposes every `BondConfig.Mode` value the validator
      accepts (`balance-alb`, `active-backup`, `802.3ad`,
      `balance-rr`, `balance-xor`, `balance-tlb`).
- [x] Submission preserves the bond's IP / DNS / MTU / members;
      only the netdev's `Mode=` changes from the operator's
      perspective.
- [x] Mode-equals-current submit is a no-op (closes the modal
      without round-tripping).
- [x] Submission goes through the safe-apply / pending-confirm
      flow (the backend `updateBond` handler already calls
      `safeApply.Apply`, no new wiring needed).
- [x] `tsc -b` and `make lint` pass.

## Out of scope (later phases)

- Break Bond / Re-create Bond (Phase 5).
- SMB Multichannel + NFS multi-path + iSCSI portal probes
  populating the Multi-flow status card (Phase 6).
- Per-NIC stats drill-down (Phase 7).
- Add VLAN form + static-route polish (Phase 8).
