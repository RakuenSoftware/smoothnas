# Proposal: SmoothNAS i18n Phase 2n — Benchmarks

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02m-smart.md`](i18n-02m-smart.md)

---

## Context

Phase 2m converted SMART. Benchmarks is the three-tab page —
I/O Benchmark / System Benchmark / Network Test — each with
its own form, live result panel, history table, and detail
modal. This slice routes every operator-visible string through
`t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Benchmarks/Benchmarks.tsx` routes
   through `t()`.
2. The mode-options array (`Random Read+Write`, etc.) is
   built inside the component so each label resolves through
   `t()`.
3. The system-bench intro paragraph and the three section
   description lines (`cpuSingleDesc`, `cpuMultiDesc`,
   `memBandwidthDesc`) live as single keys to keep bundle
   authors translating one sentence each.
4. The two history-row "ev/s" composite cells go through
   `benchmarks.system.evPerSec` `{value}` so a non-English
   bundle can flip the suffix without templating.
5. Detail-modal titles use named interpolation:
   `benchmarks.modal.ioTitle` `{mode, block}` and
   `benchmarks.modal.netTitle` `{type}`.
6. Protocol identifiers (`SMB`, `NFS`, `iSCSI`), bandwidth
   units (`MB/s`, `IOPS`, `Mbps`, `µs`, `ms`, `MiB/s`), and
   IP-address placeholders stay literal — protocol/unit
   values, not labels.

## Acceptance Criteria

- [x] Page header + tab strip render through `t()`.
- [x] I/O form (every label, option, hint, button, error) +
      result/history/modal render through `t()`.
- [x] System form (intro paragraph, duration, run button) +
      results section (CPU single / multi / memory) +
      history render through `t()`.
- [x] Network form (Local / External tabs, all fields,
      auto-server / choose-server radios, filter, loading,
      run button) + result/history/modal render through
      `t()`.
- [x] Toast/error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Backups / Settings / Volumes / Tiers / Tiering / smoothfs
  Pools / Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
