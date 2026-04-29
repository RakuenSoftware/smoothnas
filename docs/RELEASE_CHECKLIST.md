# SmoothNAS Release Checklist

This checklist is the minimum first-release gate for changes that touch storage lifecycle, NFS, SMB, backup, update, or installer behavior.

## Automated Gate

Run the non-destructive gate against the test appliance before opening or updating a release PR:

```bash
SMOOTHNAS_HOST=192.168.0.204 SMOOTHNAS_PASS='...' scripts/release-gate.sh
```

The gate verifies:

- `tierd`, SMB, and NFS services are active.
- `/api/health` responds locally on the appliance.
- Generated Samba config is async by default and stays in performance mode unless compatibility mode is enabled.
- NFS exports are async by default unless an export explicitly requests sync.
- No systemd unit is failed.
- Quick protocol create/delete smoke tests run when the NFS/SMB test mounts exist.

Destructive lifecycle tests must only run against dedicated disposable disks. They are opt-in by design:

```bash
SMOOTHNAS_RELEASE_GATE_DESTRUCTIVE=1 SMOOTHNAS_RELEASE_GATE_DISKS='/dev/...' scripts/release-gate.sh
```

## Storage Lifecycle

- Create, import, destroy, and wipe must work from the UI for mdadm and ZFS states, including partially destroyed pools and imported pools that still have mounted datasets.
- Destroy paths must forcibly release mounts, active volume groups, swap, and process holders before returning failure.
- Wipe remains a valid recovery operation for disks with stale mdadm, ZFS, LVM, or partition-table signatures.
- Reboot reconciliation must not resurrect destroyed arrays, stale shares, or stale backup configs.
- The Arrays page must expose applicable import, destroy, and wipe actions for all visible stale storage states.

## Protocols

- NFS defaults to async. Sync is an explicit per-export UI toggle.
- SMB defaults to async performance mode: `strict sync = no`, `sync always = no`, `case sensitive = yes`, and `mangled names = no`.
- SMB compatibility mode must be toggleable from the SMB page and persist across daemon restart.
- The SmoothFS Samba VFS module stays opt-in. Default SMB behavior must not require the module to be installed.
- Validate both small-file create/delete and larger sequential transfers for NFS and SMB.

## Backup

- Backup jobs must refuse local paths that resolve to the root filesystem because the intended mount is absent.
- Running backups must survive UI reload and expose progress, rate, cancel, and terminal state.
- Backup config deletion must cleanly block or cancel active runs before removing state.
- SmoothFS bulk-ingest routing must only activate when the target path resolves to a mounted SmoothFS pool.

## Update And Install

- Fresh install boots to a reachable UI and healthy `tierd`.
- Applying a package/update preserves DB migrations, protocol config, shares, users, and backup configs.
- Failed update or package application leaves the existing appliance serviceable.
- Release artifacts include the expected kernel/module/package versions for SmoothNAS and SmoothFS.
