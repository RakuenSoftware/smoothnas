# Proposal: SmoothNAS i18n Phase 2f — Datasets

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02e-pools.md`](i18n-02e-pools.md)

---

## Context

Phase 2e converted the Pools page chrome and tab strip but left
the three child tabs (Datasets / Zvols / Snapshots) literal.
This slice converts the Datasets child component — the create
form, the table, and the destroy confirm dialog.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Datasets/Datasets.tsx` routes through
   `t()`.
2. Compression option labels (`Off`, `LZ4`, `GZIP`, `ZSTD`)
   route through `t()` even though they're acronyms — the
   `Off` label in particular is a real word that should
   translate, and keeping the four under one `datasets.compression.*`
   block keeps them grouped for bundle authors.
3. The destroy confirm message uses `{name}` interpolation
   so the dataset path is interpolated rather than baked
   into English.
4. ZFS dataset names (`tank/data`), mount paths
   (`/mnt/data`), and IEC byte values rendered server-side
   stay literal.
5. Generic shared keys are reused: `arrays.col.actions`,
   `arrays.action.destroy`, `arrays.button.create`,
   `arrays.creating`, `arrays.error.{lostConnection,
   jobFailed, destroyArrayPrefix}`, `common.cancel`,
   `common.refresh`.

## Acceptance Criteria

- [x] `Create Dataset` and `Refresh` buttons render through
      `t()`.
- [x] Create form heading + every field label, option, and
      button render through `t()`.
- [x] Loading text and empty-state message render through
      `t()`.
- [x] Table column headers (Name / Mount / Used / Avail /
      Compression / Actions) render through `t()`.
- [x] Destroy confirm dialog title + message render through
      `t()`; dataset name is interpolated.
- [x] Toast and error messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Zvols / Snapshots child components (Phases 2g / 2h).
- iSCSI / SMB / NFS / Network / Smart / Benchmarks / Backups
  / Settings / Volumes / Tiers / Tiering / smoothfs Pools /
  Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
