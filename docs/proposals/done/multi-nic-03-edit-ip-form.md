# Proposal: Multi-NIC Phase 3 — Edit-IP Form (DHCP-or-static)

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-02-network-page-cards.md`](multi-nic-02-network-page-cards.md)

---

## Context

Phase 2 reshaped the Network page into the four-card layout but
left every row read-only. This phase adds the Edit-IP form, a
single component used by the bond row and the per-NIC rows of the
Active topology card. Submission goes through the existing
safe-apply / pending-confirm flow already wired into
`PUT /api/network/interfaces/{name}` and
`PUT /api/network/bonds/{name}` — that wiring just wasn't being
exercised from the UI.

## Scope

1. **Backend:** add `validateIPConfig(ipv4, ipv6, gw4, gw6, mtu)`,
   shared by `configureInterface` and `updateBond`. Rejects bad
   IPv4 CIDR, IPv6 CIDR, IPv4 gateway, IPv6 gateway, and MTU
   out of `576..9000` with `400 Bad Request` before the
   safe-apply window opens. MTU `0` is the "don't change"
   sentinel and is accepted.
2. **Backend tests:** `network_validate_test.go` covers empty
   config, valid IPv4, valid IPv6, dual-stack, bad IPv4 CIDR,
   bad IPv6 CIDR, bad IPv4 gateway, bad IPv6 gateway, bad MTU,
   and the MTU-zero sentinel.
3. **Frontend API:** add `api.updateBond(name, bond)` over
   `PUT /network/bonds/{name}` (the GET / POST already existed).
4. **Frontend form:** state-driven inline form on the Network
   page, opened via per-row "Edit IP…" buttons on both the bond
   row and per-NIC rows. Fields:
   - DHCP toggle (covers IPv4 + IPv6 RA in one click)
   - IPv4 CIDR + IPv4 gateway
   - IPv6 CIDR + IPv6 gateway
   - MTU (576–9000, blank to leave default)
   - DNS overrides (comma-separated)
5. **Client-side validation** (`editFormError`) runs before
   submit and disables the Apply button with a tooltip while
   inputs are invalid. Mirrors the backend `validateIPConfig`
   so the operator sees the error inline rather than as a 400
   round-trip.
6. **Bond submission** preserves the existing bond mode and
   member list (Phase 4 / Phase 5 introduce the dedicated
   change-mode and break-bond actions); per-NIC submission hits
   the existing `configureInterface` handler.
7. **Safe-apply hint** in the form footer reminds the operator
   that the change is pending and will auto-revert if their
   session can't reach tierd after apply.

## Acceptance Criteria

- [x] Per-NIC and bond rows in the Active topology card surface
      an "Edit IP…" button.
- [x] Clicking opens an inline form pre-populated with the
      current values (DHCP toggle, CIDRs, gateways, MTU, DNS).
- [x] DHCP toggle hides the static-IP fields and submits with
      `dhcp4 = dhcp6 = true`.
- [x] Static path requires at least one CIDR; rejects bad CIDR
      / bad gateway / bad MTU client-side, with the Apply
      button disabled until the form is valid.
- [x] Apply hits `PUT /api/network/interfaces/{name}` for NIC
      rows and `PUT /api/network/bonds/{name}` for the bond
      row, both via the existing safe-apply / pending-confirm
      flow.
- [x] Backend `validateIPConfig` rejects bad input with
      `400 Bad Request` before the safe-apply window opens.
- [x] `make test-backend`, `make test-frontend`, and
      `make lint` are clean.

## Out of scope (later phases)

- Change Mode (Phase 4).
- Break Bond / Re-create Bond (Phase 5).
- SMB Multichannel + NFS multi-path + iSCSI portal probes
  populating the Multi-flow status card (Phase 6).
- Per-NIC stats drill-down (Phase 7).
- Add VLAN form + static-route polish (Phase 8).
