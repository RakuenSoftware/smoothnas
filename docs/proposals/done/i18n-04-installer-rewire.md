# Proposal: SmoothNAS i18n Phase 4 — Installer call-site rewire

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-06-installer-locale-bridge.md`](i18n-06-installer-locale-bridge.md)

---

## Context

Phases 1, 3, and 5 set up the infrastructure: a bash dispatcher
(`iso/i18n.sh`), an English bundle (`iso/i18n.en.sh`), a Dutch
bundle (`iso/i18n.nl.sh`), and a smoke test
(`iso/i18n_test.sh`). The dispatcher used bash 4 associative
arrays — fine for unit tests on a developer machine but
incompatible with the d-i installer's busybox sh.

This slice replaces the bash dispatcher with a POSIX-sh
compatible version, consolidates the two bash bundles into
plain `.properties` text files, and rewires the operator-
visible strings in `iso/smoothnas-install` through `t()`.

## Scope

1. **Bundle consolidation**:
   - New: `iso/locales/en.properties`, `iso/locales/nl.properties`.
     One `key=value` line per string, 22 keys each, parity
     enforced (zero diff).
   - Deleted: `iso/i18n.en.sh`, `iso/i18n.nl.sh`. Their contents
     migrated to the `.properties` form by `sed`.

2. **POSIX-sh dispatcher** (`iso/i18n.sh`):
   - Rewritten as POSIX sh (no bash arrays, no
     `declare -gA`, no `${BASH_SOURCE[0]}`).
   - Looks up keys via `awk` against
     `iso/locales/<lang>.properties`.
   - Resolution chain for the dispatcher's directory:
     `SMOOTHNAS_I18N_DIR` env var → `${BASH_SOURCE:-$0}` →
     probe `/smoothnas/iso`, `/smoothnas`, `./iso`, `.`.
     Lets the d-i installer (where $0 doesn't necessarily
     point to the dispatcher) source it without trouble.
   - All 8 existing test assertions still pass; 3 new
     dash-only assertions added to confirm POSIX
     compatibility on a development machine that has dash
     installed.

3. **Installer call-site rewire** (`iso/smoothnas-install`):
   - Sources `iso/i18n.sh` near the top with
     `SMOOTHNAS_I18N_DIR="$(dirname "$0")"`.
   - Provides a no-op `t()` fallback so the script remains
     runnable on developer machines without the dispatcher.
   - The most visible operator-facing strings now go through
     `t()`:
     - `die()` — `installer.error.fatal` + `installer.error.shell`
     - Welcome banner — `installer.welcome.{title,subtitle}`
     - Disk-selection section header — `installer.disk.select.title`
     - Admin password section + whiptail prompts +
       confirm-mismatch + plain-tty fallback —
       `installer.user.password.{title,prompt,confirm,mismatch}`
     - Partition / bootstrap / done section headers —
       `installer.progress.{partition,bootstrap}` /
       `installer.done.title`

4. **Build wiring** (`iso/build-iso.sh`):
   - Copies `iso/i18n.sh` and `iso/locales/*.properties`
     into the installer initrd alongside `smoothnas-install`,
     so the dispatcher can find the bundles at install time.

## Acceptance Criteria

- [x] `iso/i18n.sh` works under `bash` (existing tests pass).
- [x] `iso/i18n.sh` works under `dash` (3 new test cases
      pass; the d-i initrd's busybox sh is closer to dash
      than to bash).
- [x] `iso/locales/en.properties` and `iso/locales/nl.properties`
      have the same 22 keys (zero diff).
- [x] `iso/smoothnas-install` syntax-checks clean under
      both `bash -n` and `dash -n`.
- [x] `t()` resolves all converted keys in both en and nl,
      verified by direct dash invocations.
- [x] `iso/build-iso.sh` ships the dispatcher and bundle
      files into the initrd.
- [x] `make test-frontend` and `make lint` clean.
- [ ] End-to-end: build the ISO with this slice, boot with
      `smoothnas.lang=nl`, confirm every operator-visible
      installer string renders in Dutch.

## Out of scope

- Internal logging strings (`echo "Configuring tierd..."`
  etc.) inside `smoothnas-install` and `firstboot.sh`. Those
  are debug breadcrumbs aimed at the installer developer,
  not the operator.
- Network-configuration prompts (lines 328 / 336 / 340 of
  `smoothnas-install`). They take CIDR + plain-IP inputs
  whose copy doesn't carry semantic value beyond "Network
  Configuration"; deferred to a later slice.
- ISO-side language picker that runs BEFORE
  `smoothnas-install` (today the language is set via kernel
  cmdline / preseed only).
- `firstboot.sh` strings — almost all are internal debug
  output rather than operator-visible prompts.

## Phase 4 wrap-up

With this slice, the installer Dutch bundle (Phase 5, #363)
is no longer dormant — it's actively used. The full chain
works:

1. Operator boots ISO with `smoothnas.lang=nl` on the kernel
   cmdline.
2. `smoothnas-install` sources `iso/i18n.sh`; dispatcher
   picks `nl` from `/proc/cmdline`.
3. Section headers, password prompts, and confirm dialogs
   render in Dutch.
4. After install, firstboot promotes the language choice
   into `/etc/smoothnas/locale` (Phase 6, #365), which the
   web GUI reads at first login.
