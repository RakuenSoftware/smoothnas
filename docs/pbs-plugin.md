# Proxmox Backup Server plugin

The in-tree **pbs** plugin runs Proxmox Backup Server on SmoothNAS so Proxmox
VE can send VM and container backups to the NAS. It is experimental until the
appliance checks below have passed on the storage configuration in use.
Sending SmoothNAS files to an external PBS is a separate integration.

## Delivery status

This is a development branch, not a released appliance feature. The live
results below cover the explicitly listed test baseline. Before shipping:

- Merge and release the smoothfs directory-sync and inode-reuse fixes from
  `fix/pbs-storage-durability`. This branch ports the validated fixes onto
  smoothfs 0.3.4; its head is `c1392ccd1bde24dc89d5166d5d9b403497ca3dda`.
- Publish the corresponding `smoothfs-dkms` and `smoothfs-samba-vfs` packages.
  `scripts/ci/smoothfs-ref` requires that fixed ref; the existing minimum-version
  check deliberately refuses an appliance build until a release contains it.
- Build the pinned runtime with the retained downstream bridge patch. The pin
  includes the credential-logging fixes from `fix/container-request-secrets`.
- Validate the resulting appliance package on a fresh guest, including loading
  its packaged module and repeating backup, restore, movement and reboot checks.

The Go module pin alone does not update a running appliance's kernel module.
The plugin refuses a datastore whose module lacks directory-sync support.
Keep the plugin experimental pending sustained-load and retention testing.

## Install and connect

1. Open **Plugins → Install**, select **pbs**, and choose the storage tiers for
   configuration, state, logs and datastore. Supply an initial administrator
   password of at least 12 characters without line breaks, materialise, then start.
2. Click **Open**. PBS opens directly at `https://<nas-hostname>:8007/` in a new
   tab. Accept its self-signed certificate after checking its fingerprint, or
   configure a trusted certificate in PBS. The appliance address must be
   reachable directly from both your browser and the Proxmox VE nodes.
3. Log in as `admin@pbs` (user `admin`, realm **Proxmox Backup authentication
   server**). Add a directory datastore, for example `backups`, with backing
   path **`/datastore`**. No host disks need to be exposed to PBS.
4. Configure PBS prune, verification and garbage-collection schedules. PBS owns
   these jobs; SmoothNAS owns the container lifecycle and storage placement.
5. Create a dedicated PBS user and API token for Proxmox VE. Give both the user
   and its privilege-separated token `DatastoreBackup` on
   `/datastore/backups` (or the chosen namespace). Use a separate identity with
   the necessary read privileges for restore operations.
6. In Proxmox VE, open **Datacenter → Storage → Add → Proxmox Backup Server**.
   Enter the NAS hostname, datastore `backups`, token identity
   `user@pbs!token-name`, token secret, and PBS certificate fingerprint. Run a
   small VM/container backup and restore it to a new guest before scheduling
   production backups.

The image is the unofficial
[ayufan PBS container](https://github.com/ayufan/pve-backup-server-dockerfiles),
pinned to `v4.2.0-1`. This is not an officially supported Proxmox deployment.
Its public default password is never installed: the plugin bootstraps its own
configuration and salted SHA-512 password hash before starting any PBS daemon.
The container's `backup` account uses UID/GID 1000 to match smoothfs ownership,
while keeping PBS's private directory permissions. Startup also checks datastore
directory `fsync` and refuses filesystems that cannot provide it.
The initial password is a SmoothNAS secret; subsequent password changes belong
in PBS. Restart and reinstall do not reset existing credentials. Configuration
from unrelated PBS installations is refused rather than overwritten.

The initial memory limit is 4 GiB. Increase it for larger stores; upstream
recommends an additional GiB per TiB. Fast local storage is preferred for PBS's
chunk/index workload. The plugin requests no privileged mode, host network,
raw disk devices, or SMART capabilities. SmoothNAS manages the disks. PBS tape,
in-container disk formatting, ZFS administration, and PAM/shell access are
outside this plugin's scope.

## Data retention and recovery

All four volumes are tier-backed binds. **Uninstall preserves them.** Compose
teardown removes containers and project files; these binds are not native
manifest volume rows subject to the installer's recursive deletion.

Paths are `<tier-mount>/.plugins/pbs/compose/{config,state,logs,datastore}`.
Record each volume's tier before uninstall. Reinstall **pbs** with the same
assignments to reuse those paths; its administrator credentials, certificates,
datastore configuration and backup contents remain intact. A different tier
selects a different directory and does not migrate the old data. Reinstalling
also requires reentering any SmoothNAS configuration settings because its
database records and initial secret were removed.

Keep configuration and datastore together in a recovery plan. A datastore
recovered without its configuration can be attached to a fresh PBS installation
using PBS's **Reuse existing datastore** option. Keep client-side encryption
keys independently; the server cannot recreate them. Avoid simultaneous PBS
instances writing the same datastore. A second PBS on another machine can
provide an independent copy through PBS sync jobs.

## Appliance validation

Use a disposable datastore for these checks. Unit tests exercise bootstrap,
catalog metadata, secret storage and lifecycle routing. Live results from a
fresh VM on the `.252` Proxmox test host are recorded below; other storage and
hardware configurations still need validation.

On the appliance, first check that the plugin starts with `/run` on tmpfs and
that an empty/short initial password prevents startup. Check that the upstream
default password fails and the chosen password authenticates. Restart PBS and
confirm that certificates, credentials and datastore configuration persist.

Probe the actual datastore mount from the host (substitute its real tier path):

```sh
python3 scripts/pbs-plugin-smoke.py \
  --filesystem /mnt/media/.plugins/pbs/compose/datastore
```

The probe creates and removes its own temporary directory, checking explicit
timestamp updates, locking, rename and file/directory `fsync`. PBS updates chunk
access times explicitly; `noatime` alone does not establish incompatibility.
Both timestamp updates and timestamp preservation during tier movement still
need to pass real backup/GC/restore checks.

On a Linux client with the official `proxmox-backup-client`, use credentials
with backup, read and prune privileges in the validation datastore. Set
`PBS_REPOSITORY`, `PBS_FINGERPRINT` and `PBS_PASSWORD_FILE` without putting
the secret in command history. The script creates a unique backup group,
uploads two versions, restores and compares hashes/permissions/symlinks,
prunes only that group, then restores again:

```sh
export PBS_REPOSITORY='validation@pbs!test@nas.example:validation'
export PBS_FINGERPRINT='<certificate SHA-256 fingerprint>'
export PBS_PASSWORD_FILE='/secure/path/pbs-token'
python3 scripts/pbs-plugin-smoke.py --workdir /tmp/pbs-validation-01
```

Run **Verify** with previously verified snapshots included, then **Start GC**
in PBS and wait for both tasks to succeed. Rerun:

```sh
python3 scripts/pbs-plugin-smoke.py --workdir /tmp/pbs-validation-01 --restore-only
```

Repeat after smoothfs tier movement and after uninstall/reinstall with the
same tiers. Also test daemon/host restart, concurrent backups, a full datastore,
and loss of a backing tier. GC has a roughly 24-hour grace period: an immediate
GC/restore pass does not prove eventual reclamation. Run it again after that
period and confirm referenced chunks remain readable and unreferenced chunks
are reclaimed. Do not change the appliance clock to force this test.

References: [PBS installation](https://pbs.proxmox.com/docs/installation.html),
[datastore requirements](https://pbs.proxmox.com/docs/storage.html),
[maintenance and GC](https://pbs.proxmox.com/docs/maintenance.html),
[Proxmox VE integration](https://pbs.proxmox.com/docs/pve-integration.html).


## Live validation — 31 October 2026

The run used a fresh Debian 13 VM on `pvetest` (`192.168.1.252`, PVE 9.2.6),
Linux `7.2.11+deb13-cloud-amd64`, LXC2Docker, two separate XFS-backed smoothfs
mounts, and the pinned PBS `v4.2.0-1` image. Native PVE backup/restore used the
host's Proxmox Backup Client 4.2.3. Both backing disks share the host's physical
storage; this run tests behavior, not independent disk-failure protection.

The initial run found two smoothfs blockers: directory `fsync` returned
`EINVAL`, and reuse of an old backing inode after tier movement redirected
unrelated writes into a moved chunk. The latter was reproduced in a small
regression test before fixing cache invalidation/validation and persisting the
destination object identity. The original failing fixture was discarded, and
the backup/restore tests were repeated with fresh data on the fixed module.

The successful live run used these dependency refs:

| Component | Tested ref | Required fixes |
|---|---|---|
| smoothfs | `v0.2.28-0.20261031160744-d0edccd6ea2c` | Directory sync, safe inode reuse after movement, persistent destination identity. |
| LXC2Docker | `ff6d6d8f9f6f62ea4b2fe7592e88a10ed20569bb` | Stop logging full create requests and failure configurations containing credentials. |

The delivery branch now pins the forward-ported smoothfs fixes at
`v0.3.5-0.20261031173954-c1392ccd1bde` in both the Go module and protocol gate.
That rebased ref has not repeated the full live matrix recorded here.

Deploy the updated runtime **and rebuild/load the updated smoothfs module**
with the plugin. Reboot an existing appliance to load its rebuilt module;
installing a new backend binary alone does not replace a loaded kernel module.
The startup sync check rejects the older module without directory-sync support.

Completed checks on the fixed module:

| Check | Result |
|---|---|
| Authentication | Chosen password accepted; public image default rejected. Current administrator and token secrets absent from runtime journal. |
| File backups | Two versions backed up; restored hashes, modes and symlinks matched before and after pruning. |
| Tier movement | Every referenced chunk of the retained file archive moved to the other tier using the production movement worker. Restore and PBS verification passed after 2,048 unrelated create/write/unlink cycles. |
| Garbage collection | Only disposable test chunks were artificially aged, without changing the system clock. GC removed two unreferenced chunks (2.001 MiB), retained 142 live chunks, and restore/verification passed. |
| Proxmox containers | `vzdump` to PBS and `pct restore` of a fresh 515.6 MiB unprivileged CT passed; sentinel bytes, mode and mapped ownership matched. |
| Proxmox VMs | `vzdump` to PBS and `qmrestore` of a fresh stopped VM with a 1 GiB disk passed; a random disk payload matched byte-for-byte. |
| Uninstall/reinstall | Checked configuration and live chunks retained byte-for-byte; the original password, certificate, token and backup remained usable. A different initial password supplied on reinstall did not reset the existing password. |
| Application restart | Restart with the image's `SIGHUP` stop signal passed. |
| VM reboot | The rebuilt module and two-tier pool mounted, PBS restarted automatically, the same certificate and credentials worked, and restore plus verification of all three backup groups passed. |
| Kernel regressions | Directory sync, actual source-inode reuse, open-file cutover, concurrent getattr/cutover and nested movement passed. |

Cleanup was verified: all five temporary guests, their disks, temporary PBS
storage entries, credentials, leases, snippets and host scratch files were
removed. The host's existing guests were left running.

The plugin remains experimental. This run does not cover full-datastore or
backing-disk loss, abrupt power failure, sustained load, or every lower
filesystem. The immediate GC test exercises the age cutoff with disposable
chunk timestamps; it does not replace a multi-day retention soak.
