# Proposal: SmoothNAS i18n Phase 8 — Per-user language persistence

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-07-iso-bootmenu.md`](i18n-07-iso-bootmenu.md)

---

## Context

The parent proposal listed cross-browser per-user language
persistence as the one acceptance criterion not yet shipped:

> A user can switch the UI language via the header picker;
> the choice persists across sessions for that user.

Today the picker writes to `localStorage 'smoothnas.lang'`
which is per-browser. An admin who logs in from their phone
sees the browser default (or the installer default), not the
language they previously chose on their laptop. This slice
adds server-side per-user persistence.

## Scope

1. **Database** (`tierd/internal/db/migrations/00010_user_prefs.sql`):
   - New `user_prefs` table keyed by username with a `language`
     column. Future per-user UI prefs add columns here without
     a separate table.

2. **Store accessors** (`tierd/internal/db/user_prefs.go`):
   - `GetUserLanguage(username)` returns the stored code or `""`
     (no preference recorded).
   - `SetUserLanguage(username, language)` upserts. Empty value
     clears the preference.
   - Six unit tests cover missing/set/upsert/independent-users/
     clear/empty-username-rejection.

3. **HTTP handler** (`tierd/internal/api/user_prefs.go`):
   - Authenticated `GET /api/users/me/language` returns
     `{"language": "<code>"}`.
   - Authenticated `PUT /api/users/me/language` with body
     `{"language": "<code>"}` validates against the same
     `validLocaleTag` whitelist as `/api/locale` and upserts.
   - Routed in `router.go`'s authenticated mux at
     `/api/users/me/language`.

4. **Frontend wiring** (`tierd-ui/src/App.tsx`,
   `tierd-ui/src/api/api.ts`):
   - New `api.getMyLanguage()` and `api.setMyLanguage()`.
   - `<LanguageSync>` child of `<AuthProvider>` watches
     `loggedIn`. On the rising edge it fetches the stored
     language and applies it via `setLanguage()` IF the user
     has not made an explicit override in this browser session
     (`hasUserOverride()` from Phase 6).
   - `onLanguageChange` (when the LanguagePicker fires) writes
     to the server via `api.setMyLanguage()` in addition to
     localStorage. Failures are silently ignored — 401 when
     unauthenticated, 500 if backend is down — so the local
     experience is never blocked.

## Final detection chain

1. `?lang=<code>` (sync)
2. `localStorage 'smoothnas.lang'` (sync)
3. **`GET /api/users/me/language`** (async, post-login, NEW)
4. `GET /api/locale` (async, installer default, Phase 6)
5. `navigator.language` (sync)
6. `FALLBACK_LANGUAGE` (`en`)

The new step is gated on `hasUserOverride()` (same as Phase 6)
so an explicit URL override or a fresh picker click always
wins. The server value catches up after the picker write-back
flushes.

## Acceptance Criteria

- [x] Migration adds `user_prefs` table with `username`
      primary key and `language` column.
- [x] `GetUserLanguage` / `SetUserLanguage` store accessors
      pass six unit tests (missing / set-then-get / upsert /
      independent-users / clear-by-empty / empty-username-rejected).
- [x] Authenticated GET / PUT routes work and are gated by
      the existing `RequireAuth` middleware.
- [x] PUT validates the language tag against the same
      whitelist as `/api/locale`.
- [x] LanguageSync fetches the saved preference on login and
      applies it when no in-browser override exists.
- [x] Picker writes flow through to the server alongside
      localStorage.
- [x] All test suites clean: `make test-frontend`, `make lint`,
      `make test-backend`, `bash iso/i18n_test.sh`.

## Closes

This slice closes the last partial-tick acceptance criterion
in the parent proposal — a user switching from English to
Dutch on their laptop now sees Dutch on their phone too,
across separate browser localStorage scopes.

## Out of scope

- Server-side propagation of the `/api/locale` system default
  to a user's preference at first login. Today the system
  default is honoured only when the user has no saved
  preference (`""`), which is the natural default-on-first-
  login behaviour.
- Migration of existing per-browser localStorage values into
  the per-user store. Operators who set Dutch via the picker
  before this slice will get Dutch on their current browser
  via localStorage and on next browsers from the system
  default until they re-pick.
