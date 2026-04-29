# Proposal: SmoothNAS i18n Phase 2x — Sharing / NetworkTests / TieringInventory

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02w-updates.md`](i18n-02w-updates.md)

---

## Context

Phase 2w landed Updates and was billed as the final per-page
slice. Three pages were missed in that count and are converted
in this slice:

- **Sharing** — small wrapper over the SMB / NFS / iSCSI tab
  components.
- **NetworkTests** — a near-duplicate of the Benchmarks
  Network Test tab; reuses the entire `benchmarks.net.*`
  block.
- **TieringInventory** — a domain → target → namespace tree
  with active-movements panel, movement-history panel, and
  degraded-states panel.

This is the actual end of Phase 2 — every operator-visible
string in `tierd-ui` now keys through SmoothGUI's
`I18nProvider`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Sharing/Sharing.tsx`,
   `tierd-ui/src/pages/NetworkTests/NetworkTests.tsx`, and
   `tierd-ui/src/pages/TieringInventory/TieringInventory.tsx`
   routes through `t()`.
2. NetworkTests reuses every `benchmarks.net.*` /
   `benchmarks.modal.*` / `benchmarks.history.*` /
   `benchmarks.result.*` / `benchmarks.io.{duration,mode}` /
   `benchmarks.running` key — only `networkTests.title` /
   `networkTests.subtitle` are added new.
3. TieringInventory adds a `tieringInventory.*` block with
   composite strings using named interpolation:
   `severity.critical` `{count}`,
   `severity.warning` `{count}`,
   `summary.targetOne` / `summary.targetMany` `{count}`,
   `confirm.deleteSnapshotMessage` `{id, name}`,
   `toast.snapshotCreatedFor` `{name}`.
4. Capability badges (`move` / `pin` / `recall` /
   `snapshot`) get small `tieringInventory.cap.*` keys so a
   non-English bundle can localise them.
5. State-machine values (`atomic`, `pending`, severity
   strings, scope_kind, error codes) and per-row protocol
   data (`d.code`, `d.message`, `target.activity_band`,
   `caps.movement_granularity`, `caps.recall_mode`,
   `caps.snapshot_mode`, `caps.pin_scope`) stay literal —
   protocol values, not labels. The yes/no /
   none / unknown fallbacks route through
   `common.{yes,no,none,unknown}` (lowercased to match the
   surrounding state-value cells).

## Acceptance Criteria

- [x] Sharing page header, subtitle, and SMB/NFS/iSCSI tab
      buttons render through `t()` (tab labels keep their
      `tab.toUpperCase()` derivation since `SMB` /
      `NFS` / `iSCSI` are protocol identifiers).
- [x] NetworkTests page header + form + result panel +
      history table + detail modal render through `t()`,
      reusing the existing `benchmarks.net.*` keys.
- [x] TieringInventory page header, the all-domains filter,
      every domain card (header + critical/warning badge +
      target table + capability badges + advanced detail
      grid + active degraded states), the namespaces section
      (per-namespace coordinated-snapshot table), the active
      movements panel + history toggle, and the degraded
      states panel render through `t()`.
- [x] All confirm dialogs and toast/error messages render
      through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Phase 2 wrap-up

This is the final slice in Phase 2. Combined with Phases
2a–2w every operator-visible string in `tierd-ui` keys
through the SmoothGUI `I18nProvider`. Phase 3 will land
non-English bundles against this fully-keyed catalog.

## Out of scope

- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
