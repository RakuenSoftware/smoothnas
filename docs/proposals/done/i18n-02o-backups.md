# Proposal: SmoothNAS i18n Phase 2o — Backups

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02n-benchmarks.md`](i18n-02n-benchmarks.md)

---

## Context

Phase 2n converted Benchmarks. Backups owns the rsync /
cp+sha256 backup configs, the per-row run panel with progress
bar + cancel + dismiss, and the long form with five tooltips
explaining the rsync transport and compression knobs. This
slice routes every operator-visible string through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Backups/Backups.tsx` routes through
   `t()` — both the parent `Backups` component and the inner
   `BackupRunPanel`.
2. The five long tooltips (Use SSH transport, Compression,
   Delete extraneous, SSH User, SSH Password, SMB Password)
   each live as a single key — bundle authors translate one
   sentence at a time.
3. The delete-confirm dialog uses `{name}` interpolation;
   the file-count line uses `{done, total}` interpolation;
   the "Backup stopped: X" toast uses `{err}` interpolation.
4. The two badges (`↑ push`, `↓ pull`) and the `(password)`
   credential indicator route through their own keys; the
   protocol-name badges (`SSH`, `NFS`, `SMB`, `rsync`,
   `cp+sha256`, `zstd`, `delete`) stay literal — protocol
   identifiers, not labels.
5. Path placeholders (`/volume1/backup`, `/exports/backup`,
   `192.168.1.10`, `/mnt/mypool`, `backups/server1`,
   `nightly-push`, `root`, `backup`) stay literal — protocol
   examples, not labels.

## Acceptance Criteria

- [x] Page header + Add Backup Config button render through
      `t()`.
- [x] Form heading toggles between Edit / New through `t()`;
      every field label, option, placeholder for "(unchanged)",
      and Save/Update button render through `t()`.
- [x] All five form tooltips render through `t()`.
- [x] Loading line, empty-state line, and table column
      headers render through `t()`.
- [x] Per-row direction badge (push/pull), the "(password)"
      indicator, and Run / Edit / Delete buttons render
      through `t()`.
- [x] BackupRunPanel renders all status/error/cancelling
      strings, the Cancel / Dismiss buttons, and the
      `{done}/{total} files` cell through `t()`.
- [x] All toasts and the delete-confirm dialog render through
      `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Settings / Volumes / Tiers / Tiering / smoothfs Pools /
  Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
