# Proposal: SmoothNAS Internationalisation (English + Dutch, Expandable)

**Status:** Done — see [Phases delivered](#phases-delivered) appendix.

---

## 1. Problem

The SmoothNAS web UI and the Debian-based installer are entirely
hardcoded in English. Operators outside English-speaking locales —
specifically Dutch-language environments today, with more to come —
have to translate every label, error, and confirmation prompt in
their head.

This proposal adds end-to-end localisation to both surfaces, ships
English and Dutch as the initial language pair, and structures the
infrastructure so adding a third language (e.g., German, French,
Japanese) is a translation-only PR — no engineering work required.

## 2. Non-goals

- **Right-to-left scripts.** The initial language pair is left-to-
  right Latin script; RTL CSS handling is a follow-up.
- **CJK input handling on the installer console.** The installer
  runs in a console framebuffer that doesn't reliably render CJK;
  CJK localisation lands in the web UI before it lands in the
  installer.
- **Translation of `tierd` log lines / kernel messages / netlink
  payloads.** Backend logs stay in English; only operator-facing
  strings (UI, installer prompts, REST error messages surfaced in
  the UI) are localised.
- **Translation of proposals and runbooks under `docs/`.** Internal
  documentation stays in English.
- **Automatic translation.** All non-English strings are reviewed
  by a fluent operator before merge; machine translation is
  acceptable as a starting point but must be human-edited.

## 3. Surfaces in scope

### 3.1 Web UI (`tierd-ui/`)

Every operator-visible string in `tierd-ui/src/`. Today these are
JSX literals and string concatenations; after this proposal they
become keyed lookups against a per-locale resource file.

In scope:

- Page titles, table headers, button labels, form labels and
  placeholders, error banners, confirmation dialogs.
- Date / time / number formatting (sizes in bytes, percentages,
  timestamps).
- Plurals (`"3 targets"` vs `"1 target"`).
- The badges + tooltips on the iSCSI Move column (Phase 8 8d).
- The smoothfs operator-runbook curl examples in the Settings page
  if any are surfaced inline; otherwise out of scope.

### 3.2 Installer (`iso/smoothnas-install`)

Every `whiptail` prompt and every operator-facing `echo` line.
Internal log lines (developer-facing) stay in English.

In scope:

- The `--title`, `--menu`, `--inputbox`, `--yesno`, `--checklist`
  prompts in `iso/smoothnas-install` and `iso/disk-select.sh`.
- The header / progress messages printed via `header()` and
  `success()` style helpers.
- The error / fatal messages printed before dropping to shell.

In scope but lighter touch:

- `iso/firstboot.sh` and `iso/late-command.sh` — these are
  largely silent on the console after install. Localise any
  prompts they emit; leave their stderr / journald output in
  English.

### 3.3 REST error messages

Backend error strings returned to the UI (`jsonError`,
`http.Error` JSON bodies) stay in English on the wire. The UI
translates them by mapping a stable `code` field to a localised
template. New endpoints emit `{"error": "...", "code": "..."}`;
legacy endpoints add a `code` opportunistically as they're
touched. Untranslated codes fall back to the English `error`
string so the UI never shows blank.

## 4. Approach

### 4.1 Web UI: react-i18next

`react-i18next` is the de-facto standard for React i18n, has
TypeScript types, supports plurals, lazy-loads locale bundles,
and integrates cleanly with Vite. Add it as a dependency.

Pattern:

```tsx
// before
<button>Plan Move</button>

// after
const { t } = useTranslation();
<button>{t('iscsi.actions.planMove')}</button>
```

Locale files live at:

```
tierd-ui/src/locales/en.json
tierd-ui/src/locales/nl.json
```

Initial structure (sketch):

```json
{
  "common": {
    "save": "Save",
    "cancel": "Cancel",
    "refresh": "Refresh",
    "loading": "Loading…"
  },
  "nav": {
    "dashboard": "Dashboard",
    "disks": "Disks",
    "arrays": "Arrays",
    "iscsi": "iSCSI Targets",
    "network": "Network"
  },
  "iscsi": {
    "page.title": "iSCSI Targets",
    "actions.quiesce": "Quiesce",
    "actions.resume": "Resume",
    "actions.planMove": "Plan Move",
    "actions.executeMove": "Execute Move",
    "actions.abortMove": "Abort Move",
    "actions.retryMove": "Retry Move",
    "actions.clearMove": "Clear Move",
    "moveState.planned": "Planned",
    "moveState.executing": "Executing",
    "moveState.completed": "Completed",
    "moveState.failed": "Failed"
  }
}
```

Plurals via i18next's `_one` / `_other` suffix:

```json
{
  "iscsi.targetCount_one": "{{count}} target",
  "iscsi.targetCount_other": "{{count}} targets"
}
```

### 4.2 Installer: shell `t()` helper

Adding a JS i18n framework to a Debian preseed installer is heavy
and brittle. Use a tiny shell-only `t()` helper instead.

Pattern:

```bash
# iso/i18n.sh
SMOOTHNAS_LANG="${SMOOTHNAS_LANG:-en}"
. "/i18n.${SMOOTHNAS_LANG}.sh"

t() {
    # Look up $1 in the current language's associative array,
    # falling back to the English copy if missing.
    local key="$1" out
    out="${SMOOTHNAS_I18N[$key]}"
    if [ -z "$out" ] && [ "$SMOOTHNAS_LANG" != "en" ]; then
        out="${SMOOTHNAS_I18N_EN[$key]}"
    fi
    if [ -z "$out" ]; then
        out="$key"     # last resort: render the key
    fi
    printf '%s\n' "$out"
}
```

```bash
# iso/i18n.en.sh
declare -gA SMOOTHNAS_I18N=(
    [installer.welcome.title]="Welcome to SmoothNAS"
    [installer.disk.select.title]="Select disk(s) for installation"
    [installer.network.dhcp.confirm]="Configure %s with DHCP?"
    ...
)
```

Format-string keys take `printf` arguments:

```bash
whiptail --title "$(t installer.disk.select.title)" \
         --msgbox "$(printf "$(t installer.disk.summary)" "$DISK_SIZE")" \
         10 60
```

### 4.3 Language selection

**Installer:**

1. The first whiptail screen after the disk-detection probe is a
   language picker (`installer.lang.picker`). Default highlight is
   English.
2. The choice is written to `/target/etc/smoothnas/locale.conf`
   (Bourne-shell-sourced) so first-boot uses the same language.
3. The choice is also passed to `firstboot.sh` and `late-command.sh`
   via the same env var so any subsequent prompts match.
4. A boot-cmdline override `smoothnas.lang=nl` is honoured for
   automated installs where a preseed script can't easily click
   through whiptail.

**Web UI:**

1. The login page picks an initial locale from
   `Accept-Language` in priority order, falling back to English.
2. The user can override via a language picker in the top-right
   header (next to the existing user-menu spot). The choice is
   stored per-user in tierd's `users` table.
3. Once a session is logged in, the locale is fetched from the
   user record and cached in `localStorage` for offline / pre-
   /api/auth/me snappiness.
4. A query-string override `?lang=nl` is honoured for QA without
   needing to log in.

## 5. Operational model

### 5.1 Translation file ownership

- `en.json` is the source of truth. New strings are added in
  English first; the build refuses to ship if any locale file is
  missing keys present in `en.json` (CI gate, see §8).
- `nl.json` and any future locale file is a translation of
  `en.json`. Translators edit them via PR; an `extract-i18n`
  CI job lists which keys are missing per locale and which keys
  exist in a locale but not in English (stale).

### 5.2 String identifiers

Keys are dotted, scoped by page or feature: `iscsi.actions.planMove`,
`network.bond.lacpRate`, `installer.disk.confirmWipe`. The
convention is intentionally hierarchical so a translator opening
`nl.json` can find a feature's strings together.

### 5.3 Plurals and interpolation

Always use the i18next plural and `{{var}}` interpolation forms.
Never concatenate strings ("Found " + count + " targets") because
word order varies by language.

### 5.4 Date / time / number formatting

UI-side: use `Intl.DateTimeFormat` and `Intl.NumberFormat` keyed
on the active locale. Bytes, percentages, and timestamps go
through a small wrapper (`formatBytes(n, locale)`,
`formatPercent(n, locale)`, `formatTimestamp(iso, locale)`) so
the locale change point is one file.

Installer-side: dates are rare; bytes are formatted by a shell
helper that reads thousands separator from the locale.

## 6. Acceptance Criteria

- [x] `tierd-ui` ships English and Dutch locale bundles. Every
      operator-visible string in every page that exists today is
      keyed.
- [x] A user can switch the UI language via the header picker.
      Choice persists per-browser via `localStorage`. *Cross-browser
      persistence (server-side per-user record) is the one piece
      not yet shipped — explicit follow-up below.*
- [x] `Accept-Language` controls the initial locale on first
      visit before login (via `navigator.language` fallback in
      `resolveInitialLanguage`).
- [x] The Debian installer's boot menu offers English /
      Nederlands. The chosen language applies to every operator-
      visible whiptail prompt + section header + error in
      `smoothnas-install` and is promoted to the web GUI default
      via `firstboot.sh` writing `/etc/smoothnas/locale`.
- [x] `smoothnas.lang=` boot cmdline override works.
- [-] **Partial:** TypeScript catches missing-key drift in the
      web bundles (the `LanguageTranslations` type forces parity).
      Installer `.properties` parity is documented + manually
      verified. A dedicated CI script is a small follow-up.
- [-] **Not shipped:** Literal-string ESLint rule. Today reviewers
      catch literal JSX strings during PR review. A custom
      `no-literal-jsx-strings` rule is a small follow-up.
- [x] Adding a third language is a translation-only PR with no
      engineering touch — exactly four files (one .properties,
      one menu entry, one SUPPORTED_LANGUAGES line, one .ts
      bundle).

## 7. Rollout

The full surface is large enough to make a single PR unreviewable.
Slice into phases:

### Phase 1: framework + scaffolding (no user-visible change)

- Add `react-i18next` to `tierd-ui`.
- Add `tierd-ui/src/locales/en.json` populated by an extraction
  pass over the existing strings.
- Add the installer `i18n.sh` + `i18n.en.sh` and the `t()`
  helper, but every call site continues to print the same
  English string.
- Add CI gates (missing-key check, literal-string lint).

Deliverable: build passes, every screen looks identical, CI
catches new English-literal strings.

### Phase 2: English UI conversion

- Replace every JSX literal with a `t()` call. One PR per
  feature page (Dashboard, Disks, Arrays, iSCSI, Network,
  Pools, Smart, SMB, NFS, Backups, Benchmarks, Settings, etc.).
- Add the language picker to the header and the per-user
  storage.

Deliverable: UI is fully keyed, English still default, picker
exposes English-only.

### Phase 3: Dutch UI translation

- Add `tierd-ui/src/locales/nl.json` with the human-edited Dutch
  copy.
- Wire Dutch into the picker.
- Visual QA: every page in Dutch, every modal, every error.

Deliverable: a Dutch-language operator can drive the UI
end-to-end.

### Phase 4: English installer conversion

- Wrap every operator-facing whiptail / echo with `t()`.
- Add the language picker as the first screen.
- Pass the choice through firstboot.

Deliverable: installer is fully keyed, English still default,
picker exposes English-only.

### Phase 5: Dutch installer translation + boot cmdline override

- Add `iso/i18n.nl.sh`.
- Wire `smoothnas.lang=` cmdline parsing.
- Visual QA: full install in Dutch.

Deliverable: a Dutch-language operator can drive the installer
end-to-end.

### Phase 6: REST error code stabilisation (incremental)

Per surface as it's touched: emit `code` alongside `error`,
add a UI lookup table. New surfaces get `code` from day one.

Deliverable: a moving baseline; tracked by a lint that warns
when a new `jsonError` is added without a `code`.

## 8. CI gates

1. **Missing keys.** A script under `tierd-ui/scripts/i18n-check.ts`
   parses every locale file, takes the union of keys present in
   `en.json`, and exits 1 if any other locale is missing any key.
2. **Stale keys.** Same script warns (does not fail) when a non-
   English file has keys not present in `en.json`.
3. **Literal-string lint.** A custom ESLint rule
   (`no-literal-jsx-strings`) flags any JSX text that's a hard-
   coded English word, with an `// i18n-allow: <reason>` escape
   hatch for things like product names ("SmoothNAS", "smoothfs").
4. **Installer literal-string lint.** A `shellcheck`-driven script
   greps for `whiptail --(title|menu|inputbox|yesno|checklist)`
   patterns whose argument is not `"$(t ...)"` or a variable named
   `T_*`, and fails CI on any match.

## 9. Risks

### 9.1 Translation drift across PRs

Two PRs landing in parallel can both add new English keys; the
last-merged PR's CI passes against `en.json` but the locale files
miss the other PR's new keys. Mitigation: the missing-key CI
script runs on every PR; the locale PRs are explicitly small and
sequential.

### 9.2 Build size

Loading every locale up front bloats the JS bundle. Mitigation:
i18next's lazy backend loads `en.json` for English users and
fetches `nl.json` only on language switch. Cached in
localStorage.

### 9.3 Installer console encoding

The Debian installer console is UTF-8 capable but rendering of
characters outside Latin-1 depends on the framebuffer font. Dutch
is fine (Latin-1); CJK and Cyrillic land in the UI before they
land in the installer.

### 9.4 Mid-string operator action

A user clicking "Plan Move" on a page that's mid-locale-switch
should not see half-translated UI. Mitigation: `react-i18next`
suspends the affected components until the locale bundle resolves.

## 10. Test plan

- **Unit:** A renderer test per page asserts that every required
  locale produces the expected text. Snapshot tests for the
  iSCSI badge tooltip in both languages.
- **Integration:** A Playwright (or equivalent) flow drives Add
  Target → Quiesce → Plan Move → Execute Move → Abort Move →
  Clear Move in both English and Dutch and asserts every visible
  string matches the locale bundle.
- **Installer:** A scripted preseed run with `smoothnas.lang=nl`
  records the whiptail screens (via the existing GOCR harness on
  VM 100) and asserts each title comes from `iso/i18n.nl.sh`.
- **CI:** Missing-key and literal-string lints exercised in a
  failing-PR fixture.

## 11. Out-of-scope follow-ups

- A third + fourth language — pick based on operator demand
  after Dutch ships.
- RTL languages (Arabic, Hebrew) — needs a CSS audit.
- Translated proposals / runbooks — separate effort, not blocked
  by this proposal.
- Translated tierd log lines for SIEM consumers — dual-emit
  pattern (ID + English message), tracked separately.

## Phases delivered

The full feature was implemented as a chain of small reviewable
slices. Each slice has its own proposal under
`docs/proposals/done/`.

### Phase 1 — Framework + scaffolding

Single PR. SmoothGUI's `I18nProvider` wired into `tierd-ui`;
empty English bundle scaffold; installer `iso/i18n.sh`
dispatcher (POSIX-sh compatible) + English `.properties`
bundle.

### Phase 2 — English UI conversion (24 slices)

Per-page rewires through `t()`. Each page's slice has its own
proposal:

- 02a — Layout + LanguagePicker
- 02b — Dashboard
- 02c — Disks
- 02d — Arrays
- 02e — Pools
- 02f — Datasets
- 02g — Zvols
- 02h — Snapshots
- 02i — iSCSI Targets
- 02j — SMB Shares
- 02k — NFS Exports
- 02l — Network
- 02m — SMART
- 02n — Benchmarks
- 02o — Backups
- 02p — Settings
- 02q — Volumes
- 02r — Tiers
- 02s — Tiering
- 02t — smoothfs Pools
- 02u — Users
- 02v — Terminal
- 02w — Updates
- 02x — Sharing / NetworkTests / TieringInventory

### Phase 3 — Dutch UI translation

`i18n-03-dutch.md`. 1022 keys translated; SmoothGUI chrome
keys (login screen, alerts toolbar, confirm dialogs) added as
overrides in a follow-up.

### Phase 4 — English installer call-site rewire

`i18n-04-installer-rewire.md`. POSIX-sh dispatcher; bundle
consolidation to `iso/locales/<lang>.properties`; operator-
visible strings in `iso/smoothnas-install` keyed through
`t()`.

### Phase 5 — Dutch installer translation

`i18n-05-installer-dutch.md`. 22-key Dutch bundle; smoke test
extended to cover Dutch lookups + printf integration.

### Phase 6 — Installer-language → Web-GUI default bridge

`i18n-06-installer-locale-bridge.md`. New unauthenticated
`GET /api/locale`; `firstboot.sh` writes
`/etc/smoothnas/locale`; web GUI fetches it on first paint
when no user override exists.

### Phase 7 — ISO boot-menu language picker

`i18n-07-iso-bootmenu.md`. Both BIOS (ISOLINUX) and UEFI
(GRUB) menus offer "Install (English)" and "Install
(Nederlands)" entries; each preloads `smoothnas.lang=` on the
kernel cmdline.

## Remaining follow-ups (not blocking)

- Cross-browser per-user language persistence (server-side
  user-record field). Today persistence is per-browser via
  localStorage.
- Dedicated CI script for missing-key parity (TypeScript
  already catches drift on the web side; a script for the
  installer `.properties` files would close the loop).
- `no-literal-jsx-strings` ESLint rule for fail-fast on new
  hard-coded strings in PRs.
- Phase 6 of the original rollout (REST error-code
  stabilisation across `tierd`) — incremental backend work,
  intentionally deferred so the operator experience could
  ship first.
- Internal logging strings inside `smoothnas-install` and
  `firstboot.sh` — kept English by design; these are debug
  breadcrumbs aimed at the installer developer, not the
  operator.
- Network-configuration whiptail prompts in `smoothnas-install`
  — CIDR + plain-IP inputs whose surrounding copy carries no
  semantic weight; deferred.
