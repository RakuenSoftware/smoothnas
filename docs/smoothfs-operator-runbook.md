# smoothfs operator runbook

Day-to-day procedures for running a SmoothNAS appliance with smoothfs pools. Covers install, pool lifecycle, share creation, routine maintenance, kernel upgrades, and troubleshooting. SmoothFS itself now lives in `RakuenSoftware/smoothfs`; this runbook covers the appliance integration side. Pair with `smoothfs-support-matrix.md` for what's pinned to what.

## 1. First-time install

On a freshly-provisioned Debian 13 appliance:

```bash
# 1. Install the three debs. Order matters — tierd Recommends (not
#    Depends) the other two, so apt won't pull them automatically.
apt install ./smoothfs-dkms_0.1.0-1_all.deb
apt install ./smoothfs-samba-vfs_0.1.0-1_amd64.deb
apt install ./tierd_0.1.0-1_amd64.deb

# 2. Confirm the kernel module built + loaded.
bash /usr/share/smoothfs-dkms/kernel_upgrade.sh
bash /usr/share/smoothfs-dkms/module_signing.sh

# 3. On secure-boot hosts, enrol the DKMS MOK cert. This requires
#    a reboot + shim prompt — schedule accordingly.
bash /usr/share/smoothfs-dkms/enroll-signing-cert.sh
# (reboot; confirm at the shim MOK-management UI with the password
#  you entered during --import)

# 4. Confirm tierd is serving.
systemctl status tierd
curl -s http://127.0.0.1:8420/api/health
```

At this point the appliance has zero smoothfs pools. Log into the web UI (default 8420) or use `tierd-cli` to create one.

## 2. Creating a smoothfs pool

Smoothfs pools are operator-declared — tierd does **not** auto-stand-up pools from disk. Each pool needs a name, a UUID (auto-generated if not supplied), and an ordered list of tier mountpoints (fastest first).

### Via the web UI

Storage → smoothfs Pools → **Create Pool**. Fill in:

- **Name** — lowercase alnum + `._-`, ≤ 63 chars.
- **UUID** — leave blank to auto-generate.
- **Tier paths** — newline- or colon-separated. Must be existing directories. Fastest first.

tierd writes a systemd mount unit at `/etc/systemd/system/mnt-smoothfs-<name>.mount`, enables + starts it. Phase 2.5 auto-discovery wires the planner.

### Via CLI

```bash
tierd-cli smoothfs create-pool \
    --name tank \
    --tiers /mnt/nvme-fast:/mnt/sas-slow
```

The CLI drives the library directly, not the REST. CLI-created pools show up in the REST list only after tierd's mount-event auto-discovery kicks in (typically < 1 s after the mount succeeds).

### What you should see

```bash
mount | grep smoothfs                        # smoothfs pool mounted
ls -la /etc/systemd/system/mnt-smoothfs-*.mount  # unit file written
systemctl status mnt-smoothfs-tank.mount     # active (mounted)
curl -s http://127.0.0.1:8420/api/smoothfs/pools  # persisted row
```

## 3. Creating shares on a pool

Once the pool is mounted at `/mnt/smoothfs/<name>`, it's a normal POSIX path. Create shares using the existing Sharing flows:

- **NFS / SMB** — Sharing → Add Share, point the path at `/mnt/smoothfs/<name>/<subdir>`.
- **iSCSI (file-backed LUN)** — Sharing → iSCSI → Add Target → **File-backed**. Set Backing File to the absolute path of an existing sized file under `/mnt/smoothfs/<name>/`. The file is auto-pinned with `PIN_LUN` the moment tierd calls LIO.

For CLI:

```bash
# Create a sized backing file first.
truncate -s 256G /mnt/smoothfs/tank/luns/web-app.img

# Then create the LIO target.
tierd-cli iscsi create-fileio \
    --iqn iqn.2026-04.com.smoothnas:web-app \
    --file /mnt/smoothfs/tank/luns/web-app.img
```

`getfattr -n trusted.smoothfs.lun /mnt/smoothfs/tank/luns/web-app.img` should return `0x01`.

### Moving a file-backed iSCSI LUN between tiers (active-LUN movement)

The Phase 8 active-LUN move flow takes a quiesced, `PIN_LUN`-protected file-backed LUN through `planned → executing → unpinned → moving → cutover → repinning → completed`, with `failed` as the terminal state on any error and crash recovery on tierd startup. The state is persisted in the SmoothNAS config table and visible in the iSCSI page's Move column.

**UI workflow.** Sharing → iSCSI:

1. Quiesce the target.
2. Plan Move and pick the destination tier.
3. Execute Move. The badge progresses through the journal states; on failure the tooltip carries the reason (`recovery: ...`, `move plan: ...`, `copy lower file: ...`).
4. If a move stalls, Abort Move drops the intent back to `planned` for retry. Clear Move drops the intent entirely.

**REST workflow** (same-host, requires a logged-in session cookie):

```bash
# All examples assume $TIERD = http://127.0.0.1:8420 and a $SESSION
# cookie obtained from POST /api/auth/login.
IQN=iqn.2026-04.com.smoothnas:web-app
COOKIE="-b session=$SESSION"

# 1. Quiesce.
curl -X POST $COOKIE $TIERD/api/iscsi/targets/$IQN/quiesce

# 2. Record move intent.
curl -X POST $COOKIE -H 'Content-Type: application/json' \
     -d '{"destination_tier":"HDD"}' \
     $TIERD/api/iscsi/targets/$IQN/move-intent

# 3. Execute (returns 202; executor runs async).
curl -X POST $COOKIE $TIERD/api/iscsi/targets/$IQN/move-intent/execute

# 4. Poll status — the move_intent block on each target carries
#    state, state_updated_at, reason.
curl $COOKIE $TIERD/api/iscsi/targets | jq '.[] | select(.iqn=="'$IQN'")'
```

**Recovery commands.**

```bash
# Roll a stuck or failed intent back to planned for retry. Accepts
# any in-flight state (executing/unpinned/moving/cutover/repinning)
# plus the `failed` terminal — refused on `planned` (nothing to
# roll back from) and `completed` (use clear instead).
curl -X POST $COOKIE $TIERD/api/iscsi/targets/$IQN/move-intent/abort

# Cancel the move outright.
curl -X DELETE $COOKIE $TIERD/api/iscsi/targets/$IQN/move-intent

# After a tierd restart mid-move, the startup sweep re-pins the
# backing file and probes smoothfs for ground truth:
#
#   - If the kernel says the move actually finished cutover before
#     tierd died (current_tier == destination, movement_state in
#     {placed, cleanup_complete}), the intent is marked `completed`
#     with reason "recovery: kernel completed cutover before tierd
#     restart; resume to bring online". Run Resume and you're done.
#
#   - Otherwise the intent is marked `failed` with reason
#     "recovery: tierd restarted in state <X>; lun re-pinned, abort
#     to retry; <kernel detail>". Run abort + execute to retry, or
#     DELETE move-intent to cancel.
#
# The target stays quiesced in either case until you Resume manually.
curl -X POST $COOKIE $TIERD/api/iscsi/targets/$IQN/resume
```

**Acceptable destination_tier formats** (resolved in this order):

1. Numeric 0-based smoothfs tier index (`"0"`, `"1"`).
2. Absolute lower-tier path (must match one of the pool's `Tiers`).
3. Tier slot name on the same pool (`"NVME"`, `"HDD"`); 1-based slot
   rank → 0-based smoothfs index conversion happens in tierd.

## 4. Routine maintenance

### Quiesce (pause movement)

Stops in-flight cutovers + refuses new `MOVE_PLAN`s. Use before any manual intervention on a pool (manual `cp` between tiers, manual `setfattr`, etc.). Phase 2 makes quiesce safe on a live pool — readers + writers keep working, just no movement.

```bash
# UI: per-pool Quiesce button.
# CLI:
tierd-cli smoothfs quiesce --pool <uuid>
```

### Reconcile (resume movement)

Lifts the quiesce + re-arms heat drain.

```bash
# UI: per-pool Reconcile button (prompts for reason — recorded in movement log).
# CLI:
tierd-cli smoothfs reconcile --pool <uuid> --reason "manual inspection complete"
```

### Movement log

Storage → smoothfs Pools → Movement log (below the pool list). Renders newest 100 transitions from `smoothfs_movement_log` across all pools. Each row shows the state transition, object_id, and source/dest tier. Use this to confirm quiesce stopped planner activity and reconcile resumed it.

Direct SQLite query (useful for scripting):

```bash
sqlite3 /var/lib/tierd/tierd.db \
    'SELECT written_at, to_state, source_tier, dest_tier FROM smoothfs_movement_log ORDER BY id DESC LIMIT 50;'
```

### Destroying a pool

Stops + removes the systemd mount unit. Any share pointing at a file on this pool will return `EIO` until the pool is re-created.

```bash
# UI: per-pool Destroy button.
# CLI:
tierd-cli smoothfs destroy-pool --name tank
```

The tier lower directories are untouched — `destroy-pool` only removes the smoothfs overlay. Re-creating with the same name + UUID + tiers resurrects the pool with all its data.

## 5. Kernel upgrades

`apt upgrade` pulls a new `linux-headers-*` package. DKMS's autoinstall hooks build smoothfs for the new kernel — no manual step required — as long as the new kernel is `≥ 6.18` (see `BUILD_EXCLUSIVE_KERNEL` in the support matrix).

After the upgrade completes, run the kernel-upgrade harness:

```bash
bash /usr/share/smoothfs-dkms/kernel_upgrade.sh
```

The harness confirms every installed kernel has either a signed smoothfs module at `/lib/modules/<kver>/updates/dkms/smoothfs.ko.xz` or a clean "out of BUILD_EXCLUSIVE_KERNEL" skip — no half-built or failed state. If a kernel built but didn't sign, `module_signing.sh` will catch it.

### Rollback

Per-kernel DKMS trees mean a failed build on kernel B never disturbs kernel A's working `.ko`. If the newly-installed kernel fails to boot or smoothfs fails to load, pick the previous kernel in GRUB — its module is still there. Once booted, `apt remove` the bad linux-headers package to keep DKMS from retrying the rebuild every upgrade.

## 6. Samba upgrades

Because `smoothfs-samba-vfs` pins `Depends: samba (= <exact version>)`, apt will **refuse** to upgrade Samba without a matching VFS deb. SmoothNAS also writes `/etc/apt/preferences.d/smoothnas-samba-vfs` whenever `/usr/lib/*/samba/vfs/smoothfs.so` is present, and unattended upgrades blacklist Samba packages. Rebuild the VFS module against the new Samba version, install it, then update the pin:

```bash
# On the build host (CI, not the appliance):
apt-get source samba=<new-version>
cd /path/to/smoothfs/samba-vfs
dpkg-buildpackage -us -uc -b

# On the appliance:
apt install ./smoothfs-samba-vfs_0.1.0-1_amd64.deb
tierd __host_init        # rewrites the Samba ABI pin and protocol tuning
systemctl reload smbd    # picks up the new .so for new connections
```

`/api/health` reports `smoothfs-samba-vfs`, `samba-vfs-abi-guard`, and NFS tuning checks when the relevant services or SmoothFS paths exist. Treat warning/critical protocol checks as release blockers.

## 7. Troubleshooting

### "smoothfs: active mounts present; leaving running module in place"

The `smoothfs-dkms` prerm printed this during `apt upgrade`. Expected — the package left the running module alone because you still have mounts. The new `.ko.xz` is on disk for the next reboot. To activate it immediately, destroy every smoothfs pool + `modprobe -r smoothfs && modprobe smoothfs`.

### `mount -t smoothfs ...` returns `-EOPNOTSUPP`

Lower filesystem doesn't pass the capability gate. Check `dmesg`:

```
smoothfs: tier /mnt/foo has s_magic 0xXXXX; only xfs, ext4, btrfs, zfs are supported
```

Mount the tier on a supported filesystem and try again.

### Samba VFS module fails to load with "version SAMBA_X.Y.Z_PRIVATE_SAMBA not found"

Samba was upgraded without rebuilding the VFS deb or the apt guard was removed. Run `tierd __host_init` to restore the guard if the installed Samba version still matches the module; otherwise rebuild the VFS package for the installed Samba version and reload `smbd`.

### `/var/lib/dkms/mok.*` missing under secure boot

Occurs on fresh installs if DKMS's framework.conf wasn't asked to autogenerate. Manually generate:

```bash
dkms generate_mok
bash /usr/share/smoothfs-dkms/enroll-signing-cert.sh
# reboot + shim prompt
```

### Movement wedged — quiesce doesn't return

Check `smoothfs_movement_log` for the last rows. If there's a row stuck at `cutover_in_progress` without a `switched`/`failed` successor, the kernel is likely in the SRCU drain waiting for a writer. `ss -tnlp | grep :<target-port>` to find the holding process; kill it if appropriate. As a last resort, `modprobe -r smoothfs` after every pool is unmounted forces recovery on the next mount.

### `Inspect` returns `ENOENT` / UI shows "pool not found"

Mount unit isn't active. `systemctl start mnt-smoothfs-<name>.mount` and check `journalctl -u mnt-smoothfs-<name>.mount` for the mount error.

### Disaster recovery: tierd.db corruption

The SmoothNAS sqlite db at `/var/lib/tierd/tierd.db` is a MIRROR of state that primarily lives elsewhere (systemd units on disk, kernel mounts, LIO saveconfig.json). Losing it means losing the REST view but **not** the pools themselves.

To recover:

```bash
systemctl stop tierd
mv /var/lib/tierd/tierd.db{,.broken}
systemctl start tierd
# tierd runs its goose migrations on an empty db.
# Phase 2.5 auto-discovery repopulates smoothfs_objects from the
# currently-mounted pools. iSCSI rows need to be re-imported
# manually (or re-created via the REST API) since LIO's
# saveconfig.json is the source of truth for targets.
```

No data on the tier lowers is at risk during a tierd.db recovery — only tierd's view of it is.

## 8. When to call support

Escalate anything that matches:

- Data-integrity-sounding errors in `dmesg`: `smoothfs: cutover lost lower_path`, `smoothfs: placement log corrupted`, `smoothfs: oid mismatch on ...`
- A pool that refuses to mount on a kernel that previously worked.
- A smoothfs module that loads but `modprobe smoothfs` then panics the kernel on first mount.
- `smbtorture` MUST_PASS set regressing (used to be 16/16).
- Any movement transition that reaches `failed` and won't clear on reconcile.

Gather before escalating:

```bash
tar czf /tmp/smoothfs-diag.tar.gz \
    /var/log/syslog* \
    /var/lib/tierd/tierd.db \
    /var/lib/dkms/smoothfs \
    /etc/systemd/system/mnt-smoothfs-*.mount \
    /etc/dkms/framework.conf.d/ \
    <(dmesg --time-format=iso) \
    <(uname -a) \
    <(modinfo smoothfs) \
    <(dkms status)
```
