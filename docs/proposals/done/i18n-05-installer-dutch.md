# Proposal: SmoothNAS i18n Phase 5 — Installer Dutch (nl) bundle

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-03-dutch.md`](i18n-03-dutch.md)

---

## Context

Phase 1 of the i18n proposal landed `iso/i18n.sh` (the
dispatcher) and `iso/i18n.en.sh` (the English bundle). Phase
3 added the web GUI Dutch bundle. This slice (corresponding
to "Phase 5" in the parent proposal — "iso/i18n.nl.sh as the
first non-English bundle") completes the symmetry by adding
the installer's Dutch bundle.

Phase 4 of the parent proposal (rewiring existing whiptail
prompts and `echo` lines through `t()`) is a separate slice
not covered here — those call sites are still hard-coded
English. With this slice, *when* those call sites land,
they'll already have a Dutch translation available.

## Scope

1. New `iso/i18n.nl.sh` mirrors every key in `iso/i18n.en.sh`
   (22 keys today). Storage / protocol identifiers stay
   literal; user-facing prompts and titles translate.
2. `iso/i18n_test.sh` extended with two Dutch test cases
   (`SMOOTHNAS_LANG=nl` lookup; printf integration with the
   localised format-string).
3. The dispatcher (`iso/i18n.sh`) needs no changes — it
   already auto-loads `iso/i18n.${SMOOTHNAS_LANG}.sh` when
   the file exists, falling back to English silently when
   it doesn't.

## Translation conventions

Same as the web GUI Dutch bundle:
- **Protocol identifiers stay literal**: RAID-1, DHCP,
  enp1s0, etc.
- **Common verbs translate**: Welcome → Welkom, Continue →
  Doorgaan, Confirm → Bevestigen.
- **Technical jargon stays English where idiomatic**:
  first-boot kept as "First-boot-configuratie", debugging
  shell falls back to English shell prompt language.

## Acceptance Criteria

- [x] `iso/i18n.nl.sh` exists.
- [x] Every key in `iso/i18n.en.sh` has a matching key in
      `iso/i18n.nl.sh` (22 keys both, zero diff).
- [x] `iso/i18n_test.sh` covers both English and Dutch
      lookups + printf integration. All 8 assertions pass.
- [x] `make test-frontend` and `make lint` clean.

## Out of scope

- Phase 4: rewiring the installer's whiptail prompts and
  `echo` call sites through `t()`. Those still print hard-
  coded English. When that slice lands, this Dutch bundle
  will already cover every key.
- Per-NIC i18n probe (Phase 6+) for the d-i environment
  picking up `smoothnas.lang=nl` from the kernel cmdline
  and forwarding it to firstboot.
- ISO-side `lang_picker` whiptail menu wiring through the
  installed bundles.
