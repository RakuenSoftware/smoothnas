# Proposal: SmoothNAS i18n Phase 6 — finish `tiering.go` error codes

**Status:** Done
**Predecessor:** Phase 6 slices 1–13 (PRs #373–#384)
**Implementation:** Slice 14 (this PR)
**Parent proposal:** [`smoothnas-i18n-en-nl.md`](smoothnas-i18n-en-nl.md)

---

## Outcome

Slice 14 closed this proposal end-to-end: 32 literal-message
sites in `tiering.go` converted to 12 stable codes under the
`tiering.*` namespace, en.ts and nl.ts grew by 12 keys each
(parity preserved at 92 total), and `uncodedBaseline` reached
**0** — every literal-message error in the tierd api package
now round-trips a stable code through `useExtractError`.

The final code list (12 codes for 32 sites; deduped — namespace
covered 10 sites, object covered 8, snapshot covered 4, target
covered 2):

  - `tiering.domain_not_found`              (×1)
  - `tiering.target_not_found`              (×2)
  - `tiering.name_required`                 (×1)
  - `tiering.placement_domain_required`     (×1)
  - `tiering.namespace_not_found`           (×10)
  - `tiering.snapshot_adapter_unavailable`  (×1)
  - `tiering.snapshot_not_found`            (×4)
  - `tiering.object_not_found`              (×8)
  - `tiering.move_params_required`          (×1)
  - `tiering.move_cross_domain`             (×1)
  - `tiering.movement_not_found`            (×1)
  - `tiering.reconcile_rate_limited`        (×1)

Differences from the suggested groupings in the original
proposal: the conversion didn't need separate codes for
movement state (`already_in_progress` / `already_finished`) or
drain / stage gates — those errors are returned via
passthrough `err.Error()` from the engine layer and remain
exempt from the baseline ratchet by design.

---

## Context

Phase 6 of the i18n initiative localises backend REST errors via
stable codes that round-trip through `apiFetch` → `ApiError.code` →
SmoothGUI's `useExtractError` hook → `error.<code>` keys in the
locale catalog.

Slices 1–13 converted **220+ error sites** across 14 surfaces,
landed **80 `error.<code>` keys** in en.ts/nl.ts, and pinned the
remaining work behind `TestUncodedErrorBaseline` (currently 32
sites). Surfaces fully coded: auth, helpers (universal),
arrays.go, disks.go, backup.go, smoothfs.go, system.go, sharing.go,
zfs.go, terminal.go, jobs.go, network.go, tiers.go.

The one remaining surface is **`tiering.go`** — the largest
single file in the api package (1100+ lines), with 32 literal
error sites concentrated in inventory / movement / staging
endpoints. It was deferred from slice 13 because:

1. tiering.go's error sites are tightly coupled to specific
   movement state machines (drain phases, IO gates, range
   staging) — converting them requires careful per-handler
   review to pick semantically meaningful codes rather than
   one-shot mechanical replacement.
2. Many of the strings are user-actionable ("X is already
   draining", "stage IO is currently gated"), so they merit
   carefully-translated Dutch text rather than rushed renderings.

## Scope

1. Convert the 32 literal-message sites in `tierd/internal/api/tiering.go`
   to `jsonErrorCoded(...)` with stable codes under the
   `tiering.<code>` namespace.

2. Add the matching `error.tiering.<code>` keys to
   `tierd-ui/src/i18n/locales/en.ts` and `nl.ts`. Preserve
   the existing en/nl key parity.

3. Decrement `uncodedBaseline` in
   `tierd/internal/api/error_codes_baseline_test.go` from 32 to
   close to 0 (the residual is purely passthrough `err.Error()`
   forwards, which are exempt by design — the baseline test
   doesn't count those).

## Suggested code groupings

A first pass at code naming, to be refined during conversion:

- Inventory / namespace lookup:
  - `tiering.namespace_not_found`
  - `tiering.domain_not_found`
  - `tiering.snapshot_not_found`
  - `tiering.object_not_found`
- Movement state:
  - `tiering.movement_already_in_progress`
  - `tiering.movement_not_found`
  - `tiering.movement_already_finished`
- Stage / drain:
  - `tiering.stage_io_gated`
  - `tiering.drain_already_active`
  - `tiering.drain_not_active`
- Validation:
  - `tiering.target_tier_required`
  - `tiering.snapshot_id_required`

The operator should review the final code list before merging,
since a few strings could be argued either way (e.g. "domain not
found" might collapse into `tiering.namespace_not_found` if the
two are semantically identical; the converter should check the
handlers).

## Out of scope

- **Passthrough errors.** `jsonError(w, err.Error(), status)`
  forwards the underlying engine's message verbatim and is
  exempt from the baseline ratchet. Localising those would
  require coding the engine errors themselves, which is a
  separate (larger) initiative.

- **Phase 6 retrospective.** A short retrospective on the
  conversion approach (slice cadence, code naming convention,
  what the ratchet caught vs missed) belongs in a separate
  follow-up doc once tiering.go is done — it's the natural
  closing artefact for Phase 6.

## Acceptance criteria

- [ ] `tierd/internal/api/tiering.go` has zero matches for the
      `jsonErrLiteralPattern` and `httpErrorPattern` regexes
      defined in `error_codes_baseline_test.go`.
- [ ] `make test` passes; `TestUncodedErrorBaseline` is at the
      new (lower) baseline.
- [ ] `make lint` passes.
- [ ] en.ts and nl.ts have parity for every new
      `error.tiering.*` key.
- [ ] No passthrough error site is accidentally converted to a
      coded one (the message would lose its dynamic content).

## Estimated size

One PR, ~2 hours of focused work. The mechanical conversion is
quick; the slow part is reading each handler to pick the right
code namespace and writing two-language locale entries.
