# Proposal: SmoothNAS i18n Phase 1 — Framework + Scaffolding

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)

---

## Context

Phase 1 of the SmoothNAS i18n proposal lands the framework that
later phases bolt onto. No user-visible change: every existing JSX
literal renders the same English text it always did, every
existing installer prompt prints the same string. Phase 2 begins
the per-page extraction; Phase 3 lands the Dutch bundle; Phases
4-5 wire and translate the installer; Phase 6 stabilises REST
error codes.

The web UI routes through SmoothGUI's `I18nProvider` rather than
adding a separate i18next dependency — SmoothGUI's own components
(AlertsButton, ConfirmDialog, LoginPage, UserDropdown, Toast)
already call `useI18n()` against their built-in keys, so threading
the SmoothNAS catalog through the same provider gives one shared
`t()` surface for both the framework and SmoothNAS pages.
SmoothGUI 0.3.0 was published with the i18n surface (release PR
RakuenSoftware/smoothgui#12); SmoothNAS bumps `^0.2.3 → ^0.3.0`.

The installer rolls a tiny shell `t()` helper that sources a
language-specific bundle of associative-array entries. Adding a
JS i18n framework into a Debian preseed environment would be
heavy and brittle; the shell version is ~80 lines and unit-
testable.

## Scope

### Web UI

1. Bump `@rakuensoftware/smoothgui` from `^0.2.3` to `^0.3.0` to
   pull in `I18nProvider` / `useI18n` / `englishTranslations`.
2. `tierd-ui/src/i18n/index.ts` exports:
   - `SUPPORTED_LANGUAGES` — source of truth for the language
     picker, currently `[{ code: 'en', label: 'English' }]`.
     Adding a third language is a translation-only PR.
   - `FALLBACK_LANGUAGE = 'en'`.
   - `smoothnasTranslations: TranslationCatalog` — initially
     `{ en: <bundle> }`.
   - `resolveInitialLanguage()` — picks the language at app boot
     from `?lang=` → `localStorage('smoothnas.lang')` →
     `navigator.language` → fallback. Stops at the first
     supported code.
   - `persistLanguage(code)` — stores the operator's choice in
     `localStorage` so the picker decision survives reloads.
3. `tierd-ui/src/i18n/locales/en.ts` — initial scaffolding bundle
   covering common verbs (`common.save`, `common.cancel`, …) and
   nav labels (`nav.dashboard`, `nav.network`, …). Per-page keys
   land page-by-page during Phase 2.
4. `App.tsx` wraps the existing component tree in `I18nProvider`
   with the resolved language + the SmoothNAS catalog. The
   `onLanguageChange` callback persists the choice through
   `persistLanguage` so the header picker (Phase 2) is a one-
   liner.

### Installer

1. `iso/i18n.sh` — dispatcher that picks the active language from
   `/smoothnas-installer-lang` → `SMOOTHNAS_LANG` env →
   `smoothnas.lang=` on `/proc/cmdline` → `en`.
2. `iso/i18n.en.sh` — English bundle keyed by dotted strings
   (e.g. `installer.welcome.title`,
   `installer.network.dhcp.confirm` with a `%s` placeholder).
   The dispatcher snapshots this as the fallback before
   overlaying the active-language bundle.
3. `t <key>` shell helper looks up the active-language entry,
   falls back to English, falls back to printing the key itself
   so a missing string is obvious during development.
4. `iso/i18n_test.sh` — bash-only smoke test covering: default
   `en`, env-var override, known-key lookup, unknown-key key-as-
   value fallback, missing-language-bundle silent fallback to
   English, and `printf` integration with format-string keys.
   Six assertions, exits 0 / 1.

### Out of scope (later phases)

- The literal-string lint rule (Phase 1 §8.3 of the umbrella
  proposal). It earns its own slice; landing it now would be a
  blunt-instrument change across every page.
- Per-page string extraction (Phase 2).
- Non-English bundles (Phase 3 UI / Phase 5 installer).
- The header language picker (Phase 2).
- Boot-cmdline `smoothnas.lang=` parsing in the installer's
  early-command flow (Phase 5; the dispatcher's read of
  `/proc/cmdline` is already wired but no installer code calls
  the dispatcher yet).
- REST error-code stabilisation (Phase 6).

## Acceptance Criteria

- [x] `@rakuensoftware/smoothgui@0.3.0` is published to
      `npm.pkg.github.com` and pulled in via `package.json`.
- [x] `App.tsx` wraps the tree in `I18nProvider` with the
      SmoothNAS catalog; `tsc -b` is clean.
- [x] `tierd-ui/src/i18n/locales/en.ts` is keyed by dotted
      strings and consumed by `smoothnasTranslations`.
- [x] `resolveInitialLanguage()` honours the documented detection
      chain and only returns supported codes.
- [x] `iso/i18n.sh` exports a `t()` that looks up + falls back to
      English + falls back to the key itself.
- [x] `iso/i18n_test.sh` passes all six assertions.
- [x] No existing call site rewires through the new helpers — the
      Phase 1 contract is "framework only, zero user-visible
      change".
- [x] `make test-backend`, `make test-frontend` (`tsc -b`), and
      `make lint` are clean.
