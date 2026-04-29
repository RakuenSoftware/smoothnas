# Proposal: SmoothNAS i18n Phase 9 — Cleanup pass

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-08-per-user-persistence.md`](i18n-08-per-user-persistence.md)

---

## Context

Phase 8 closed the last unticked acceptance criterion in the
parent proposal. This slice picks up the small backlog items
that aren't user-blocking but tighten the feature's edges:

1. The three remaining hard-coded English strings in
   `iso/smoothnas-install`'s manual-network whiptail prompts.
2. A CI gate that catches key-parity drift between
   `iso/locales/en.properties` and other language bundles.
   (TypeScript already does this for the web bundles via the
   `LanguageTranslations` type, but the installer side had no
   automated check.)

## Scope

1. **Three new keys** added to both `iso/locales/en.properties`
   and `iso/locales/nl.properties` (parity preserved at 25
   keys each, zero diff):
   - `installer.network.manual.prompt` — the long
     "DHCP failed on all interfaces ... Enter IP address"
     prompt; `%s` interpolation for the interface name.
   - `installer.network.gateway.prompt` — "Enter gateway IP:"
   - `installer.network.dns.prompt` — "Enter DNS server IP:"

2. **`smoothnas-install`** rewires the three manual-network
   `run_whiptail --inputbox` calls through `t()` and uses
   `printf "$(t installer.network.manual.prompt)" "$manual_iface"`
   for the interpolation.

3. **`iso/i18n_test.sh`** new assertion #9: walks every
   `iso/locales/*.properties` file, extracts keys, and uses
   `comm` to assert each non-English bundle has exactly the
   same key set as `en.properties`. Reports missing AND extra
   keys per bundle. Verified by intentional-drift smoke test
   (added an extra key, ran the suite, observed a clean FAIL
   message; removed the key; suite back to green).

## Acceptance Criteria

- [x] All three manual-network whiptail prompts in
      `smoothnas-install` route through `t()`.
- [x] `installer.network.manual.prompt`,
      `installer.network.gateway.prompt`, and
      `installer.network.dns.prompt` exist in both
      `en.properties` and `nl.properties`.
- [x] Test suite extended to 12 assertions; all pass under
      bash and dash.
- [x] Bundle parity check correctly fails on intentional
      drift (manually verified) and passes on intact bundles.
- [x] `make test-frontend`, `make lint`, `make test-backend`
      clean.

## Closes

This slice plus Phase 8's per-user persistence closes the
parent proposal's "Remaining follow-ups (not blocking)"
section except for two items that are genuinely separate
features:

- **`no-literal-jsx-strings` ESLint rule** — medium-effort
  custom rule with allowlist for product names. Would
  fail-fast on new hard-coded JSX strings in PRs. Not
  shipped here; tracked as a future hygiene PR.
- **REST error-code stabilisation across `tierd`** (Phase 6
  of the original rollout) — large incremental backend work
  that makes backend error messages localisable client-side.
  Worth its own proposal cycle.

## Out of scope

Same as parent proposal Section 11.
