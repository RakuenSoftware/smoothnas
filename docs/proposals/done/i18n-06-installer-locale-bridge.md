# Proposal: SmoothNAS i18n Phase 6 — Installer-language → Web-GUI default bridge

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** SmoothGUI chrome overrides (#364) and Phase 5 installer Dutch (#363)

---

## Context

Phases 1–5 made every operator-visible string in the SmoothNAS
web GUI and installer translatable. The web GUI's
`resolveInitialLanguage()` chain (`?lang= → localStorage →
navigator → en`) lands a sensible default at first paint, but
it never asks "what language did the operator install in?"

The result is awkward: an operator who installs SmoothNAS in
Dutch on a Dutch-speaking system still gets the English login
screen on first boot if their browser sends `Accept-Language:
en`.

This slice adds a fourth source to the chain: the
installer-chosen language, persisted to a small file at install
time and exposed via an unauthenticated REST endpoint. The web
GUI reads it once on mount and overlays it on top of the sync
default — but only when the operator hasn't already made an
explicit picker choice.

## Scope

1. **tierd backend** (`tierd/internal/api/locale.go`):
   - New unauthenticated `GET /api/locale` endpoint mounted in
     the root mux next to `/api/health`. Returns
     `{"language":"<code>"}` or `{"language":""}` if not set.
   - Reads `/etc/smoothnas/locale` (one-line plain-text file).
   - Strict whitelist (`validLocaleTag`) on the file content
     so a corrupt or attacker-controlled file can't inject
     JSON into the response.
   - Test coverage: missing file, present file, garbage
     contents (5 attack strings), accepted tag forms, method
     not allowed.

2. **tierd-ui** (`src/i18n/index.ts`, `src/App.tsx`):
   - New `fetchSystemLocale(): Promise<LanguageCode | null>`
     calling `/api/locale`.
   - New `hasUserOverride(): boolean` — true iff `?lang=` or
     localStorage has a supported language. Gates whether the
     async fetch should overlay anything.
   - `App.tsx` adds a one-shot `useEffect`: if no user
     override exists, fetch the system locale and call
     `setLanguage()` if it returns a supported code.
   - First paint still uses the sync chain so the app never
     renders blank or flashes English-then-other.

3. **Installer** (`iso/smoothnas-install`,
   `iso/firstboot.sh`):
   - `smoothnas-install` writes
     `${TARGET}/etc/smoothnas/installer-lang` from the kernel
     cmdline (`smoothnas.lang=`), `SMOOTHNAS_LANG` env, or
     `/smoothnas-installer-lang` file (the same chain the
     existing `iso/i18n.sh` dispatcher already honours).
     Whitelisted to `en` / `nl` (and BCP-47 region variants).
   - `firstboot.sh` reads `installer-lang` and promotes it to
     `/etc/smoothnas/locale` — the file the new endpoint
     serves.

## Detection chain (final)

The full chain becomes:

1. `?lang=<code>` — QA / scripted, sync.
2. `localStorage 'smoothnas.lang'` — user's explicit picker
   choice, sync.
3. **`/api/locale`** ← installer's choice, async.
4. `navigator.language` — browser's Accept-Language, sync.
5. `FALLBACK_LANGUAGE` (`en`) — last resort.

(1) and (2) gate (3) via `hasUserOverride()` — if either is
set, the sync answer wins and the async fetch is a no-op. (3)
overrides (4) and (5) when none of the above is set.

## Acceptance Criteria

- [x] `GET /api/locale` returns 200 with the configured
      language when `/etc/smoothnas/locale` is present.
- [x] `GET /api/locale` returns 200 with `{"language":""}`
      when the file is missing.
- [x] Garbage / corrupt file content can't inject JSON into
      the response (validation tests pass).
- [x] Endpoint is unauthenticated — login screen can fetch
      it before user authenticates.
- [x] Web GUI on mount: with no `?lang` and no
      `localStorage`, fetches `/api/locale` once and applies
      the result.
- [x] Web GUI on mount: with `?lang` or `localStorage` set,
      DOES NOT override (user's picker choice wins).
- [x] `firstboot.sh` writes `/etc/smoothnas/locale` with the
      installer-chosen language on first boot.
- [x] `smoothnas-install` writes `/etc/smoothnas/installer-lang`
      inside the target filesystem so firstboot can promote it.
- [x] All existing tests stay green: `make test-frontend`,
      `make lint`, `make test-backend`, `bash iso/i18n_test.sh`.

## Out of scope

- Per-user persistence of language across logout (server-side
  user-record field). Today the choice persists per-browser
  via localStorage.
- ISO-side language picker that lets the operator change the
  install-time language interactively (today it's set via
  preseed / kernel cmdline only).
- Dynamic re-fetch of `/api/locale` after a language change
  is made on a different browser. The current fetch happens
  once on mount.
