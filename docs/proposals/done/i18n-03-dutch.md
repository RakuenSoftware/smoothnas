# Proposal: SmoothNAS i18n Phase 3 — Dutch (nl) bundle

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02x-remaining.md`](i18n-02x-remaining.md)

---

## Context

Phase 2 keyed every operator-visible string in `tierd-ui`
through SmoothGUI's `I18nProvider`. Phase 3 lands the first
non-English bundle: Dutch (nl), the language called for in
the parent proposal's title.

With Dutch registered, `SUPPORTED_LANGUAGES.length >= 2`, so
the `LanguagePicker` automatically renders in the top-bar.
The picker is wired through `useI18n()` and the
`I18nProvider`'s `setLanguage`, so flipping it switches every
`t()` call in the tree without a remount.

## Scope

1. New `tierd-ui/src/i18n/locales/nl.ts` mirrors every key
   from `en.ts` (1022 keys today). Key parity is enforced by
   the `LanguageTranslations` TypeScript type — adding a key
   to one bundle without the other fails `tsc -b`.
2. `tierd-ui/src/i18n/index.ts` registers `nl` in
   `SUPPORTED_LANGUAGES` (`{ code: 'nl', label: 'Nederlands' }`)
   and adds it to the `smoothnasTranslations` catalog.

## Translation conventions

- **Storage / protocol identifiers stay literal**: RAID-5,
  RAIDZ-1, mdadm, ZFS, iSCSI, NFS, SMB, IQN, CIDR, MTU, VLAN,
  DHCP, IPv4/IPv6, balance-alb, 802.3ad, etc. Dutch IT
  shops speak these in English.
- **Common verbs translate**: Save → Opslaan, Cancel →
  Annuleren, Apply → Toepassen, Delete → Verwijderen, Edit →
  Bewerken, Refresh → Vernieuwen, Loading → Laden.
- **User-facing nouns translate where idiomatic**: Network →
  Netwerk, Settings → Instellingen, Users → Gebruikers,
  Disks → Schijven, Sharing → Delen, Memory → Geheugen,
  Health → Gezondheid.
- **Technical jargon with no clean Dutch equivalent stays
  English**: Pool, Array, Snapshot, Tier, Backend, Cluster,
  Cache, Mount, Stripe, Mirror, Bond, Pin, Drain, Stage,
  Backup (kept as "Back-up" with hyphen, the standard Dutch
  spelling).
- **Composite messages keep their English placeholders**:
  `arrays.summary.usedWithPct` → `'{used} gebruikt ({pct}%)'`.
  Word order can flip naturally because every dynamic token
  is named, not positional.

## Out of scope

- SmoothGUI's built-in `englishTranslations` keys (alerts,
  login, confirm chrome, toasts, user-dropdown chrome) are
  not overridden in this slice — when the operator switches
  to `nl`, those bits still render in English. Adding a
  Dutch override block for SmoothGUI's catalog is a small
  follow-up if/when SmoothGUI exports a Dutch base catalog.
- Installer Dutch bundle (Phase 4 / 5 of the parent
  proposal): `iso/i18n.nl.sh` lands as a separate slice
  alongside the d-i / firstboot wiring.
- Per-user persistence of language choice over login. The
  Phase 1 chain already persists locally via `localStorage`
  and the `?lang=` query string; tying that to the user
  record would be a Phase 6+ feature.
- Plural-rule rework. Several composites use one-vs-many
  keys (`pathOne` / `pathMany`, `targetOne` / `targetMany`,
  `summary.targetOne` / `summary.targetMany`). Dutch has the
  same one-vs-many split as English, so the existing wiring
  is correct without further work.

## Acceptance Criteria

- [x] `tierd-ui/src/i18n/locales/nl.ts` exists and exports
      a `LanguageTranslations` object.
- [x] Every key in `en.ts` has a matching key in `nl.ts`
      (TypeScript-checked via `LanguageTranslations`).
- [x] `tierd-ui/src/i18n/index.ts` registers `nl` and
      includes it in `smoothnasTranslations`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.
- [ ] Manual: switch the language picker to "Nederlands" and
      confirm every page renders in Dutch with no key fall-
      through (visual smoke test pending end-to-end browser
      run on the appliance).

## Phase 2 → Phase 3 wrap-up

With this slice, the SmoothNAS web GUI ships in two
languages. The `LanguagePicker` auto-shows because
`SUPPORTED_LANGUAGES.length >= 2`. Adding a third language
is a single-file change: drop a new bundle into
`./locales/<code>.ts`, register it in `index.ts`, and the
type system tells the translator exactly which keys are
missing.
