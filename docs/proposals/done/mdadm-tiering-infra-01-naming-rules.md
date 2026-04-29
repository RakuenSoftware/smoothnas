# Proposal: mdadm Tiering Infrastructure — Naming Rules and Validation

**Status:** Pending
**Part of:** mdadm-tiering-infrastructure (Step 1 of 14)
**Depends on:** base appliance (tierd service)

---

## Problem

Pool names flow directly into LVM VG names (`tier-{name}`) and filesystem mount paths (`/mnt/{name}`). Without explicit validation, invalid characters or reserved values can produce silent LVM errors, fail `vgcreate` with a cryptic message, or collide with OS-managed mount points.

---

## Specification

All pool names must pass the following rules before any LVM or filesystem operation is issued.

**Pattern:** `^[a-z0-9][a-z0-9_-]{0,30}$`
- Lowercase alphanumeric, hyphens, and underscores only.
- Must start with a lowercase alphanumeric character.
- Maximum 31 characters total.

**Reserved names (must be rejected):**
`data`, `root`, `home`, `boot`, `tmp`, `var`, `sys`, `proc`, `run`, `dev`, `mnt`, `srv`, `opt`, `etc`, `lost+found`

**VG name length:** The full VG name `tier-{name}` must not exceed 127 characters (LVM limit). Given the 31-character pool name cap this is always satisfied, but the check should be explicit.

Validation runs as the first action in any handler that accepts a pool name — creation, deletion, and array assignment. No LVM command is issued before validation passes.

---

## Acceptance Criteria

- [ ] Names matching the pattern and not in the reserved list are accepted.
- [ ] Names outside the pattern are rejected with `400 Bad Request` and an error message naming the specific violated rule.
- [ ] Names in the reserved list are rejected with `400 Bad Request`.
- [ ] Validation runs before any LVM or filesystem command is issued.
