# Proposal: SmoothNAS i18n Phase 7 — ISO boot-menu language picker

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-04-installer-rewire.md`](i18n-04-installer-rewire.md)

---

## Context

Phase 4 wired the installer's operator-visible strings through
`t()` and embedded the dispatcher + bundles into the initrd.
But the only way for an operator to choose a language was to
edit the kernel cmdline at boot time (`smoothnas.lang=nl`) —
and that requires knowing the option exists.

This slice adds a normal boot-menu picker: "Install (English)"
vs "Install (Nederlands)" entries in both the BIOS (ISOLINUX)
and UEFI (GRUB) menus. Each entry preloads the kernel cmdline
with the right `smoothnas.lang=` value. The installer
dispatcher already reads `/proc/cmdline` for `smoothnas.lang=`
(Phase 1), so no further wiring is needed.

## Scope

1. **`iso/build-iso.sh`** — `setup_boot()`:
   - ISOLINUX menu: split `smoothnas` entry into `smoothnas-en`
     (default) and `smoothnas-nl`. Each appends
     `smoothnas.lang=<code>` before `initrd=...`.
   - GRUB menu: split the single "SmoothNAS Install" entry into
     "SmoothNAS Install (English)" (default) and "SmoothNAS
     Install (Nederlands)". Each adds `smoothnas.lang=<code>`
     to the `linux` line.
   - "Boot from first hard disk" entry is unchanged.

## Operator flow

1. Operator boots the ISO. Boot menu appears with both
   language entries.
2. Operator picks "Install (Nederlands)".
3. Kernel boots; `/proc/cmdline` carries `smoothnas.lang=nl`.
4. `smoothnas-install` sources `iso/i18n.sh`; the dispatcher
   reads `/proc/cmdline`, finds `smoothnas.lang=nl`, and sets
   `SMOOTHNAS_LANG=nl`.
5. Installer renders Dutch banners, prompts, and progress
   messages.
6. Final stage writes `/etc/smoothnas/installer-lang` to the
   target filesystem.
7. firstboot.sh promotes that to `/etc/smoothnas/locale`.
8. Web GUI on first login fetches `/api/locale`, gets
   `{"language":"nl"}`, and switches.

End-to-end Dutch operator experience with zero kernel-cmdline
knowledge required.

## Acceptance Criteria

- [x] `iso/build-iso.sh` writes ISOLINUX config with two
      language entries.
- [x] `iso/build-iso.sh` writes GRUB config with two language
      entries.
- [x] English remains the default selection in both menus.
- [x] All 11 i18n test assertions still pass.
- [ ] End-to-end: build the ISO, boot in a VM, select
      Nederlands from the menu, confirm Dutch installer.

## Out of scope

- Per-user persistence of language across logout (Phase 8+).
- Plural-rule rework for languages with more than two plural
  forms (English/Dutch don't need this).
- REST error-code stabilisation so backend errors localise
  client-side (Phase 6 on the parent proposal — backend work).
- Adding a third language. The boot-menu picker, dispatcher,
  and web GUI all auto-pick up new locales on file drop:
  add `iso/locales/<code>.properties`, append a third menu
  entry to `iso/build-iso.sh`, register the language in
  `tierd-ui/src/i18n/index.ts`'s `SUPPORTED_LANGUAGES`, drop
  in `tierd-ui/src/i18n/locales/<code>.ts`. Three files
  changed for a translation-only PR.
