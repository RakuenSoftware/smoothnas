# Proposal: SmoothNAS i18n Phase 2v — Terminal

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02u-users.md`](i18n-02u-users.md)

---

## Context

Phase 2u converted Users. Terminal hosts an xterm.js shell
session over a WebSocket. This slice routes its labels
through `t()` — the page chrome and the two status strings
(connected / disconnected, the "--- session ended ---"
banner inside the terminal pane).

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Terminal/Terminal.tsx` routes through
   `t()`.
2. The "session ended" banner that's written into the
   terminal frame (via xterm `term.write`) routes through
   `t()` — its surrounding ANSI escapes stay literal since
   they are protocol bytes.
3. The xterm theme dictionary, ANSI colour palette, and
   terminal data bytes stay literal — protocol values, not
   labels.

## Acceptance Criteria

- [x] Page header, connected/disconnected badge, Reconnect
      button, and the WebSocket-error banner render through
      `t()`.
- [x] The `--- session ended ---` banner inside the terminal
      pane routes through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Updates page conversion.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
