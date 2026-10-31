# Proxmox Backup Server plugin

The in-tree **pbs** plugin runs Proxmox Backup Server on SmoothNAS so Proxmox
VE can send VM and container backups to the NAS. It is experimental until the
appliance checks below have passed on the storage configuration in use.
Sending SmoothNAS files to an external PBS is a separate integration.

## Delivery status

**The current PBS image is blocked for production use.** A later power-loss
repeat left all nine tested snapshot manifests empty. Their archive indexes
and referenced chunks remained intact, but normal verification failed. The
locally rebuilt manifest-sync candidate passed the additional tests below, but
the default plugin image remains the failing baseline.

PBS on smoothfs requires every fix through `e85ea3a25126`, including interrupted
movement recovery and a real lower-filesystem `syncfs` barrier. Older development
builds can also omit live snapshots during GC and lose chunks. The startup
directory-sync probe does not detect these other defects; load the required
updated module as well as the backend.

This is a development branch, not a released appliance feature. The live
results below cover the explicitly listed test baseline. Before shipping:

- Merge and release the smoothfs directory-sync and inode-reuse fixes from
  `fix/pbs-storage-durability`. This branch ports the validated fixes onto
  smoothfs 0.3.4; its head is `e85ea3a251262e4bea95efa8862d91258e76f880`. It also adds a
  durable cleanup acknowledgement so an object can move repeatedly,
  corrects mkdir failure cleanup across supported kernel versions, and fixes
  non-root enumeration of snapshots spread over several backing tiers. It now
  reconciles interrupted moves with live kernel placement and flushes each
  backing filesystem when `syncfs` completes.
- Publish a PBS image with durable manifest replacement and pin its immutable
  digest. [The validated candidate patch](patches/pbs-manifest-fsync.patch) targets
  PBS 4.2.0. Its locally built image passed the checks below; the plugin still
  references the failing upstream baseline, rather than an unavailable test
  registry. Publishing and pinning a suitable image remains a release blocker.
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

An abrupt power-off can leave an incomplete snapshot. In live testing PBS
listed the interrupted entry with no archive files or completed manifest;
restore failed, and verification reported that entry as a failure while all
completed snapshots verified successfully. Remove an entry only after
confirming it is the known interrupted backup without a completed manifest,
then rerun verification and restore checks.

Use the required fixed kernel module for full-store recovery. The extended
test found a smoothfs mkdir error-path lock bug: after `ENOSPC`, some subsequent
lookups blocked, PBS retained a backup-group lock, and an application restart
could not finish. Existing snapshots remained readable, but freeing space did
not repair the damaged lock. The isolated test VM needed a forced reset to
load the corrected module. This was a filesystem defect, rather than evidence
that restarting PBS is sufficient recovery on the affected module.

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
`v0.3.5-0.20261031200503-e85ea3a25126` in both the Go module and protocol gate.
The older successful matrix used the earlier refs recorded in each section;
the continuation below reports the newer failures and fixes.
The extended run below tests the forward-ported code, movement completion,
full-filesystem mkdir error handling, and directory enumeration. The final
packaged appliance still requires validation after its dependency packages are
published.

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

The plugin remains experimental. This initial run did not cover full-datastore
failure, backing-disk loss, abrupt power failure, sustained load, or every lower
filesystem; the extended run below adds several of these checks. The immediate GC test exercises the age cutoff with disposable
chunk timestamps; it does not replace a multi-day retention soak.

## Extended live validation — 31 October 2026

A second fresh guest on `.252` ran the delivery branch with smoothfs based on
0.3.4, Linux `7.2.11+deb13-cloud-amd64`, two 8 GiB XFS backing disks, and the
pinned PBS image/runtime plus the retained runtime bridge patch. All three
virtual disks used ZFS `sync=standard` for the tests. Root-disk sync was disabled
only during initial package provisioning and restored before loading the pool.

The first 412 referenced chunks moved successfully, but an immediate reverse
move failed with `EBUSY`: the kernel remained `switched` after userspace had
finished cleanup. Commit `e2d838d1fe8f` adds `MOVE_COMPLETE` after source unlink
and directory sync, then returns the object to `placed`. A failed unlink or
acknowledgement remains an incomplete cleanup. New worker tests cover these
failures; the live kernel regression covers four moves with a held descriptor,
premature/stale acknowledgements, and an idempotent completion retry.

Checks completed on this fixture:

| Check | Result |
|---|---|
| Concurrent backups | Four clients completed nine rounds: 36 successful file snapshots, including shared-content deduplication and incremental changes. Twelve of these ran after the final directory-enumeration fix. |
| Restore integrity | All 12 initial snapshots restored with matching hashes, modes and symlinks. Four more restores passed during movement; pruning retained exactly the latest two snapshots per group, and all eight retained snapshots restored. |
| Repeated movement | On the fixed module, 32 retained chunks each completed three additional moves while four restores ran. Both kernel and SQLite states returned to `placed` after every move. |
| Encrypted backups | Correct-key restore matched hashes, modes and symlinks; a wrong key was rejected. All three referenced encrypted-archive/catalog chunks moved in both directions, and encrypted restore passed afterwards. |
| Native Proxmox backups | Fresh CT `vzdump`/`pct restore` and VM `vzdump`/`qmrestore` passed, including sentinel bytes/mode/ownership and raw-disk payload checks. A VM-create attempt hit the shared host's storage lock; its serial rerun passed. |
| Cancellation | An active client was interrupted with SIGINT; retrying its 512 MiB archive and restoring it passed. |
| Forced guest power-off | The guest was stopped during a throttled upload. Boot ID changed; the pool mounted and PBS restarted automatically. All eight retained snapshots restored, the 512 MiB upload retried and restored, and the encrypted archive restored. The known incomplete snapshot required explicit removal before verification passed. |
| Normal maintenance | Verification passed during movement. GC respected its age cutoff and queued 464.281 MiB in 116 unreferenced chunks without prematurely deleting recent data. |
| Kernel/runtime checks | Directory sync, inode reuse, open-FD/getattr/nested cutover, repeated movement, and the isolated network-namespace NAT test passed. Go tests, race tests, static checks and kernel builds passed. The 4 GiB container memory limit was enforced; observed final peak was 3.24 GiB, with no OOM events. |

The capacity test exposed another kernel defect. Linux 6.18 consumes the child
dentry on `vfs_mkdir` failure while leaving its parent locked; 6.19+ also unlocks
the parent. The old materialization path unlocked twice on newer kernels, while
the direct mkdir path failed to unlock on 6.18. Commit `13b3b03983c7` normalizes
the error contract in the compatibility helper. A small two-tier ENOSPC probe
hung on the old module and passed on the fix, including responsive lookup and
write recovery. Builds passed against both 6.18 and 7.2 headers; live regression
execution used 7.2.11. See the primary
[6.18 implementation](https://github.com/torvalds/linux/blob/v6.18/fs/namei.c) and
[6.19 implementation](https://github.com/torvalds/linux/blob/v6.19/fs/namei.c).

With the mkdir fix, both backing filesystems were filled to 32 MiB free. PBS
correctly failed a 512 MiB upload with `ENOSPC`. After removing only the test
fillers, existing snapshots restored and the same group's new 512 MiB backup
and restore succeeded without restarting PBS. The original failing run had
kept its group locked and blocked application shutdown; a forced VM reset was
needed to discard that damaged kernel state before loading the fix.

Post-GC checksum restore then exposed a separate data-loss defect. PBS's
UID 1000 could enumerate only the canonical directory below root-owned private
backing parents, while a root probe saw the union of both tiers. A snapshot
created on the other tier after full-store recovery disappeared from PBS's
listing. Verification incorrectly returned `TASK OK` with zero snapshots in
that group, and GC deleted 134 archive chunks and one catalog chunk still
referenced by the snapshot. The failed snapshot's manifest, indexes and failure
logs were preserved as test evidence before its metadata was explicitly removed; its lost chunks
were not recovered.

Commit `65794ec7eba2` performs backing traversal with the mount credentials,
matching normal lookup, and propagates contributing-tier errors rather than
returning a partial listing. Upper DAC still applies. The old module failed a
non-root merged-directory regression; the corrected module passed for UID 1000,
a bind mount, denial of UID 1001, and propagation of a contributing-tier lookup
error. With the corrected traversal PBS saw the previously hidden snapshot and
verification correctly flagged its missing data.

Acceptance checks now compare PBS's listing with the exact known completed
snapshot set, require verification to visit every expected snapshot, and run
checksum restores after GC. A successful task status or group count alone is
insufficient.

The final repeat used every fix through `65794ec7eba2` and rebuilt the lost test
group with a new backup. Full-store failure, freeing space, retry and checksum
restore passed without a restart. Another twelve concurrent file backups and
pruning produced 14 known completed snapshots across ten groups. PBS listed
all 14; verification visited every one. GC then removed 83 unreferenced chunks
(339.585 MiB), retained 819 chunks, and strict verification plus checksum
restores of all eight retained file snapshots, all three recovery archives and
the encrypted archive passed.

Managed uninstall/reinstall retained seven configuration files and ten selected
live chunks byte-for-byte. The original password, certificate and token remained
usable; a different initial password supplied on reinstall did not reset the
installation, and both that replacement initial password and the image's public
default were rejected.

A final forced guest power-off interrupted another throttled upload on the final
module. Boot ID changed, the same module and pool loaded, and PBS restarted
automatically. After confirming and removing only the new incomplete snapshot,
the upload retried and restored successfully. PBS listed all 15 expected
completed snapshots across ten groups, and verification visited every snapshot
with zero errors. All eight retained file snapshots, four recovery archives and
the encrypted archive restored with matching data. Native `pct restore` and
`qmrestore` of the original CT/VM backups also passed again after GC, reinstall
and this power-off. Runtime journals contained neither the original nor the
replacement administrator password nor the API token.

The corrected repeat validates the tested source and fixture. Multi-day retention,
physical backing-tier loss, host power failure and the final packaged appliance
remain unvalidated. Both backing disks share one physical host storage pool.
The package gate still refuses to ship this dependency ref until a smoothfs
release and its archive packages contain all the fixes.

Cleanup of the extended fixture was verified: VM 9601, both native source/restore
pairs, all owned volumes, temporary PBS storage entries and credentials, leases,
cloud-init snippet, host scratch directory and local private credential copies
were removed. The pre-existing VM 8168 and CT 9474 remained running. Sanitized
failure and success logs were retained locally for review.


## Continued failure testing — 31 October 2026

A third fresh guest on `.252` used Linux 7.2.11 and two dedicated XFS backing
filesystems. All virtual disks used ZFS `sync=standard` during testing. The
initial root-disk package provisioning and later PBS compilation temporarily
disabled root-disk sync, restoring it before validation. Both backing disks
remained on normal sync throughout. Existing guests were left alone.

Four clients completed another six rounds of file backups: 24 successful
224 MiB snapshots. Twelve initial restores matched, then another concurrent
backup cycle ran alongside GC. Pruning retained exactly two snapshots per group,
and all eight retained snapshots restored. A separate archive roundtrip preserved
POSIX access/default ACLs, binary user xattrs, UID/GID, setuid/setgid modes, hard
links, Unicode names, dangling symlinks, a FIFO, sparse-file contents and empty
files/directories.

### Interrupted movement

Stopping the worker after source cleanup but before its completion acknowledgement
reproduced a live bug: the old startup recovery reported `placed` in SQLite while
the kernel stayed `switched`, so the next movement failed with `EBUSY`. The backup
remained readable. Commit `bbf034674eea` fixes the actual startup service path:

- Persist a full-sync movement intent and its log before the first kernel plan.
- Use kernel placement to resolve the cutover/database transition gap.
- Remove the non-authoritative copy and sync its parent before acknowledging
  completion or rollback; leave failed or uncertain recovery pending.
- Instantiate a persisted path when the post-boot OID index is cold, verify its
  identity, then inspect it through the expected pool.

The original stuck object recovered without rebooting, then moved successfully.
Four worker-exit cases passed: after planning, after copying, after cutover before
its SQLite transition, and immediately before the cleanup acknowledgement. Each
allowed subsequent movements and preserved all 18 retained archive indexes and
267 referenced chunks byte-for-byte. Live guards rejected stale rollback and
rollback after cutover; retries after a completed rollback were idempotent. The
standalone kernel probe passed four rollbacks and four moves with a held file
descriptor. Go tests, race checks, static checks, and builds against Linux 6.18
and 7.2 headers passed.

### Corruption detection and power-loss failure

A deliberate one-byte mutation in a disposable referenced chunk caused verification
to fail and PBS to quarantine that chunk as `.0.bad`. Restore rejected the affected
snapshot. The test validated the quarantined bytes against the exact injected
mutation, restored the saved original, and checked its hash. Restore and strict
verification of all nine expected snapshots then passed; all 18 archive indexes
and 267 referenced chunk hashes still matched.

The subsequent forced guest power-off exposed a separate critical failure. The
pool/runtime restarted automatically, cold-index movement recovery completed,
and the next movements preserved the referenced data. However, all nine
`index.json.blob` manifests were now zero length, on both the presented filesystem
and its raw XFS backing. Verification failed for every snapshot. The empty
manifests, original archive indexes, hashes and failure logs were preserved as
evidence. The manifests were not reconstructed, and these snapshots were not
claimed as recovered or successfully restored.

Two findings require separate treatment:

1. Smoothfs `syncfs` returned success without flushing lower file data. An isolated
   XFS test demonstrated delayed allocation remaining on both backing filesystems
   and an upper-written file after successful `syncfs`. Commit `e85ea3a25126` now
   flushes every distinct lower superblock and reports errors. The old test failed;
   the corrected module passed. In a power-off comparison, the old callback lost
   all 16 new upper replacements, retaining their older contents. With the fix,
   a single upper `syncfs` preserved all 32 new control and upper replacements.
2. PBS 4.2.0 replaces manifests without requesting file sync when updating
   verification state. The exact release's
   [manifest update](https://git.proxmox.com/?p=proxmox-backup.git;a=blob;f=pbs-datastore/src/backup_info.rs;hb=v4.2.0)
   calls `replace_file` with sync disabled; its
   [verification path](https://git.proxmox.com/?p=proxmox-backup.git;a=blob;f=src/backup/verify.rs;hb=v4.2.0)
   invokes that update. A direct-XFS control reproduced eight empty unsynced
   replacements after power loss, while all eight file-synced controls survived.
   This supports the manifest-durability diagnosis; the filesystem fix alone is
   not acceptance evidence for an image that does not request a barrier after
   verification. The candidate patch enables file sync before manifest rename.

### Backing-tier I/O failure and GC

A separate valid datastore held one 64 MiB file archive with two indexes and
19 referenced chunks. Its indexes stayed on the slow tier; one live chunk was
moved to the fast tier alongside a disposable orphan. All 20 test chunks were
aged three days while preserving mtime, so an incomplete reference scan could
have deleted the live fast-tier chunk.

A reversible device-mapper error target made the slow tier return actual `EIO`.
GC failed during phase 1 while opening the dynamic index. Both the live chunk
and the orphan on the healthy fast tier remained present. After restoring the
linear mapping, listing and checksum restore succeeded without restarting PBS.
Both indexes and all 19 chunk hashes matched. Healthy GC then removed exactly
one orphan (1.716 MiB), retained the referenced chunk, and verification plus
another actual restore passed. This separate datastore does not repair or
supersede the nine failed manifests described above.

### Built manifest-sync candidate

The candidate was built from the exact PBS `v4.2.0` source commit
`035c449897fafc228c8bbf3a5b5ba38564478ac7`, with only the single-line manifest-sync
patch. All three binaries were rebuilt: API, proxy and manager. Their dynamic
library dependencies resolved inside the clean runtime image, and deployed binary
hashes matched the build outputs. The added image layer contained exactly these
three binaries, with no live container configuration or credentials.

The base was pinned to
`sha256:01f685a010025233b649417a317cc175b27920cd64f0429abfdc46a0d8d2e88a`.
The tested registry manifest digest was
`sha256:fb5b209705b6b169b374e1b55e15cb39c38eaf5e2b335bfeff5e7cf9aee43edc`.
The image was installed through the actual plugin installer, lifecycle and
Compose backend. Existing TLS identity, token and administrator credentials
survived. A syscall trace observed a successful temporary-manifest `fsync`
before its rename, where the original image's trace had no such sync.

A fresh candidate datastore passed:

- Four concurrent file clients, three rounds of 224 MiB archives: all 12 initial
  restores matched. Pruning retained exactly two versions per group, and all
  eight retained archives restored again.
- Rich metadata and encrypted backup/restore, including wrong-key rejection.
- Native Proxmox CT backup and `pct restore` of a 515.638 MiB OS filesystem,
  with matching sentinel content, UID and mode. The backup took 322 seconds
  under heavy shared-host I/O pressure; this is correctness evidence, not a
  performance measurement.
- Native VM backup and `qmrestore` of a 1 GiB logical disk, with its random
  payload matching after restore.
- Exactly 12 retained snapshots across eight groups. The inventory includes
  dynamic and fixed indexes for file, CT and VM archives: 23 indexes and 391
  distinct referenced chunks.
- The production movement worker moved 24 referenced chunks and eight retained
  archive indexes to the slow tier while the CT backup was active. Tiered file
  and encrypted restores passed.
- GC with all 435 test chunks aged three days while preserving mtime. It removed
  44 unreferenced chunks (179.109 MiB), including the control orphan. All 23
  retained indexes and 391 referenced chunk hashes still matched.

The candidate passed three power-loss tests: an initial one-snapshot smoke test
and two full 12-snapshot repeats after GC. The second full repeat followed
uninstall/reinstall with a different initial password: the original password and
TLS identity survived, and the replacement password and public default were
rejected. Each forced power-off immediately followed
verification, without an added sync barrier. Pool, runtime and PBS restarted
automatically. Strict verification visited every expected snapshot; actual file,
encrypted, rich-metadata, native CT and native VM restores passed after the full
power cut. Each full repeat used fresh native restore guests, then destroyed
them and their temporary PBS storage entry. The 23-index/391-chunk inventory
also matched after both repeats. The nine original empty
manifests were not reconstructed and are not counted as recovered.

[The amd64 build recipe](../scripts/pbs-image/Dockerfile) records the source,
patch, base and compatible Debian Rust dependency versions. The tested build ran
these commands in a disposable Debian 13 guest and repacked the clean base with
`umoci`; the Dockerfile itself has not been executed. Cargo's lockfile, package
versions, binary hashes and an OCI image archive are retained with the test
evidence. Remaining rolling package inputs mean this is not a claim of a
bit-for-bit reproducible rebuild.

The final loaded module matched the required build. The final PBS cgroup had a
4 GiB memory limit with no OOM events or kills, and runtime journals contained
neither test administrator password nor the API token. Linux 7.2 emitted one
nonfatal heat-drain workqueue deprecation notice; the final kernel audit found
no BUG, Oops, blocked-task or OOM diagnostic.

Candidate deployment also reproduced a separate, unfixed runtime issue:
registry addresses containing `:port` are split incorrectly during image pull.
The test used a fixture-local hostname on standard HTTPS after capturing the
failure. This does not establish support for registry-port references.

Multi-day retention, physical tier loss, host power failure and the packaged
appliance remain unvalidated. Publishing/pinning the patched image and releasing
the required smoothfs packages are still required. The plugin's installation
description continues to state the observed default-image corruption risk.
