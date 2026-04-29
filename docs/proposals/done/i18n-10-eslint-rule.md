# Proposal: SmoothNAS i18n Phase 10 — ESLint `no-literal-jsx-strings` rule

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-09-cleanup.md`](i18n-09-cleanup.md)

---

## Context

Phase 9 closed the small backlog except for the
`no-literal-jsx-strings` ESLint rule the parent proposal
called for. Today reviewers catch new hard-coded JSX strings
during PR review; this slice adds a fail-fast CI gate.

## Scope

1. **ESLint introduced** to the frontend toolchain (the
   project previously had no JS/TS linter — `make lint` was
   `go vet` only):
   - Dev deps added: `eslint@9`, `typescript-eslint@8`,
     `@eslint/js@9`, `eslint-plugin-react@7`,
     `eslint-plugin-react-hooks@5`.
   - Flat config at `tierd-ui/eslint.config.js` (ESLint 9
     idiom).
   - `tierd-ui/package.json` gets `"type": "module"` and a
     `lint` script.
   - `make lint` extended with a new `lint-frontend` target.

2. **Custom rule** at
   `tierd-ui/eslint-rules/no-literal-jsx-strings.js`:
   - Flags JSX text and attribute values that look like
     user-facing English strings but aren't routed through
     `t()`.
   - Strict heuristic for attributes (`placeholder`, `title`,
     `aria-label`, `alt`): skips path-like values, single
     lowercase tokens, dotted version/IP-like sequences,
     digit+unit-suffix patterns.
   - Multi-line JSXText heuristic: requires either a
     multi-word string OR a single word with 5+ alpha chars.
   - Allowlist for product names (SmoothNAS, smoothfs,
     tierd) and protocol identifiers (ZFS, RAID, mdadm,
     SMB, NFS, iSCSI, etc.).
   - `// i18n-allow: <reason>` escape hatch on the same line
     OR the line above (JSX has no syntax for inline comments
     between attributes).
   - Skips text inside `<code>...</code>` (protocol bits).
   - Project-wide allowlist extension via the rule's
     `allow` and `allowRegex` options.

3. **`react-hooks` rules also enabled** while we're in the
   neighbourhood:
   - `react-hooks/rules-of-hooks` (error)
   - `react-hooks/exhaustive-deps` (warn)
   - The codebase already had explicit `// eslint-disable`
     directives for these in a few places, so the team's
     intent was clearly to enforce them — they just weren't
     wired to a real linter run.

4. **Disable-directive cleanup**:
   - Existing `// eslint-disable-next-line react-hooks/exhaustive-deps`
     comments were positioned correctly when the rule wasn't
     running but in the wrong place when it was. For
     multi-line `useEffect`, the warning is reported at the
     closing `}, []);` line, so the disable needs to be on
     the line above that closing line. Fixed across 7 files
     (Arrays, Backups, Dashboard, Disks, Terminal, Tiers,
     Updates, Volumes — single-line useEffects didn't need
     this fix).
   - `Backups.tsx`: also fixed the legitimate ref-cleanup
     warning by capturing `pollRefs.current` to a local var
     used in the cleanup, per the standard React pattern.

5. **Real lint catches converted through t()**:
   - `BenchChart.tsx` legend labels (`Read MB/s`, `Read IOPS`,
     `Write MB/s`, `Write IOPS`) — reuse existing
     `benchmarks.col.*` keys.
   - `NetworkTestChart.tsx` axis title + legend labels
     (`Latency (ms)`, `Download Mbps`, `Upload Mbps`,
     `Latency`) — three new `benchmarks.chart.*` keys plus
     reuse of `benchmarks.net.latency`.
   - `NamespaceFiles.tsx` section heading "Files in
     namespace" — new `namespaceFiles.title` key.
   - One legitimate placeholder annotated with
     `// i18n-allow:` (Backups.tsx form name placeholder
     "e.g. nightly-push" — example value, not user copy).

6. **Bundle parity** preserved (TypeScript would catch
   drift). en.ts and nl.ts both grow by 4 keys.

7. **Two orphan files** ignored in eslint config —
   `src/pages/Arrays/{Mdadm,Zfs}.tsx`. Superseded by the
   unified `Arrays.tsx` in commit a95ed26 ("merge mdadm
   and ZFS into unified Arrays page") and not imported
   anywhere; left in place per the operator's explicit
   instruction.

## Acceptance Criteria

- [x] `npm run lint` runs ESLint with the custom rule.
- [x] `make lint` includes the frontend lint.
- [x] Rule has zero false positives on the existing
      converted surface (after the small wave of real-catch
      fixes).
- [x] Rule fails-fast on new hard-coded JSX strings (verified
      mentally against the heuristics; future PRs will
      validate in practice).
- [x] `// i18n-allow:` escape hatch works on same line and
      line above.
- [x] Bundle parity: en.ts and nl.ts both gain the 4 new
      keys (chart legend + axis labels + namespace files).
- [x] `make lint`, `make test-frontend`, `make test-backend`,
      `bash iso/i18n_test.sh` — all clean.

## What this closes

The parent proposal's Acceptance Criterion #7 — "CI fails
if a JSX file uses a literal English operator-visible string
outside an `i18n-bypass` allowlist" — moves from "Not
shipped (small follow-up)" to **shipped**.

## Out of scope

- Migrating other ESLint hygiene rules
  (`@typescript-eslint/*`, full `react/*` ruleset, etc.).
  This slice keeps the lint surface tight: only the i18n
  rule + react-hooks (which the codebase already wanted).
- Auto-fix support for the i18n rule. The rule reports;
  authors apply the t() call themselves with the right key
  name.
- Server-side / installer string parity (still
  `iso/i18n_test.sh`'s job).
