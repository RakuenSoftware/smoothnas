# Proposal: SmoothNAS i18n Phase 2l — Network

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02k-nfs.md`](i18n-02k-nfs.md)

---

## Context

Phase 2k converted NFS Exports. Network is the largest sharing-
adjacent page — System / Active topology / VLANs / Static
routes / Multi-flow status, plus the Edit IP and Change Mode
modals, the Break Bond / Re-create Bond confirms, the per-NIC
stats drill-down, and a thicket of inline validation messages.
This slice converts every operator-visible string in
`Network.tsx`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Network/Network.tsx` routes through
   `t()`.
2. The five inline validation functions (`editFormError`,
   `vlanFormError`, `routeFormError`) map their error
   strings to `network.validate.*` keys.
3. Native `window.confirm()` strings (delete VLAN / delete
   route / break bond / re-create default bond) route
   through `t()`. The two long bond confirms are kept as one
   key each rather than split mid-sentence.
4. The mode-change hint and the safe-apply edit-IP hint each
   live as a single key (`network.changeMode.hint`,
   `network.editIp.hint`) — bundle authors translate one
   sentence rather than five fragments.
5. Composite phrases use named interpolation:
   `network.bond.ipSummary` `{addrs, mode}`,
   `network.changeMode.title` `{name}`,
   `network.editIp.titleBond` / `network.editIp.titleIface`
   `{name}`,
   `network.confirm.{deleteVlan,deleteRoute,breakBond}`
   `{name}` / `{id}`,
   `network.error.{bondNotFound,unknownMode}`
   `{name}` / `{mode}`,
   `network.pending.message` `{seconds}`,
   `network.stats.counters` `{rxBytes, rxPkts, rxDrop,
   txBytes, txPkts}`.
6. The multi-flow status row composites use English-style
   pluralisation (`pathOne` / `pathMany` etc.). For Phase 3
   we'll likely revisit this with a proper plural rule, but
   the named-interpolation pattern keeps the JSX clean.
7. Bond mode identifiers (`balance-alb`, `802.3ad`, …),
   IP/CIDR placeholders, MAC strings, link state values
   (`up`, `down`), and the speed-string `{speed_mbps} Mb/s`
   composite stay literal — protocol values, not labels.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] Pending-change banner, Confirm + Revert buttons render
      through `t()`.
- [x] Change Mode modal, Edit IP modal, hint paragraphs, and
      Apply / Cancel buttons render through `t()`.
- [x] All five cards (System / Active topology / VLANs /
      Static routes / Multi-flow status) render headers,
      table headers, empty states, and per-row labels
      through `t()`.
- [x] Per-NIC stats drill-down (Sampling line, RX/TX/Est
      labels, the boot-counter sentence) renders through
      `t()`.
- [x] Native `window.confirm()` strings (Break Bond, Re-
      create Bond, delete VLAN, delete route) route through
      `t()` with `{name}` / `{id}` interpolation.
- [x] All five inline validation functions return localised
      strings.
- [x] All error messages from `extractError(...)` callsites
      route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Smart / Benchmarks / Backups / Settings / Volumes /
  Tiers / Tiering / smoothfs Pools / Users / Terminal /
  Updates page conversions.
- Non-English bundles (Phase 3).
- Plural-rule rework for the multi-flow status row composites.
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
