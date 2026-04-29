# Proposal: Backup throughput on 2.5 Gbps LAN — investigation and follow-up

**Status:** Done — `tcp_slot_table_entries=128` landed in SmoothNAS,
the smoothkernel compile-time default patch landed in `smoothkernel`,
and smoothfs tier-spill on ENOSPC landed in the standalone `smoothfs`
repo.

**Related PR:** #266 (this branch).

---

## 1. Target

A single `rsync`-over-NFS backup from the real source NAS
(`192.168.1.254:/mnt/data/storage`) onto a smoothnas tier should saturate
the 2.5 Gbps link to within a few percent of line rate. Operator has
verified the source and the network can sustain 2.5 Gbps; the task is to
find whichever bottleneck on the smoothnas client side keeps us short.

Hard constraints established during the investigation:

- **`nconnect>1` is vetoed** on backup NFS mounts. Operator has tested
  it against the real source in the past and observed reduced
  throughput. Persisted as a feedback memory.
- **Jumbo frames** (MTU 9000) are blocked at the bridge on this test
  environment (`ping -M do -s 8972 192.168.1.254` drops), so end-to-end
  validation would need an infra-side change not in scope for this
  proposal.

All throughput numbers below are from the same workload: pull of
`/mnt/data/storage/public` (14.23 GB, 909 files) onto `/mnt/media/storage`
on the test VM (`192.168.0.207`, 4 vCPU, 8 GB RAM, single virtio-net
queue, tier0 = 64 GB XFS on LVM-on-mdadm, tier1 = ZFS).

## 2. Experiment matrix

| # | Variant                                     | Duration | Avg rate       | % of 2.5 Gbps |
| - | ------------------------------------------- | -------: | -------------: | ------------: |
| 9  | Baseline, `tcp_slot_table_entries=2` (kernel default), BBR | 58.0 s | 2 057 Mbps | 82 %          |
| 10 | `tcp_slot_table_entries=128`, BBR          | 52.0 s   | 2 295 Mbps    | 92 %          |
| 11 | (same, via `tuning.ApplyNetworkTuning`)    | 52.0 s   | 2 295 Mbps    | 92 %          |
| 12 | + RPS on all 4 cores, RFS flow entries=32k | 52.0 s   | 2 295 Mbps    | 92 %          |
| 13 | + CUBIC (instead of BBR)                   | 53.0 s   | 2 252 Mbps    | 90 %          |
| 14 | + IRQ 29/30 pinned to cpu3, tierd on cpu0-2 | 53.0 s  | 2 252 Mbps    | 90 %          |
| P  | 4× parallel `rsync`, shared NFS mount      | 49.2 s   | 2 313 Mbps    | 92.5 %        |

The ~10 pp jump between row 9 and row 10 is the single biggest change.
Everything else is statistical noise or mild regression.

### 2.1 Why RPS didn't help

With a single NFSv4.2 mount there is exactly one TCP flow. RPS steers
RX packets to CPUs by the 5-tuple hash, so all packets from that flow
hash to the same CPU regardless of the `rps_cpus` mask. Verified by
`/proc/stat` softirq counts after the run: `cpu2` kept ~90 % of the
softirq deltas even with `rps_cpus=f`. RPS is the right lever for the
NAS *serving* path (many concurrent SMB/NFS clients). It is not useful
for the backup *client* path.

### 2.2 Why parallel rsync didn't help

`ss -tnpe` during the parallel run showed exactly one `ESTAB` to
`192.168.1.254:2049`. The Linux NFSv4 client multiplexes every consumer
onto a single transport per `(server, port, auth)` tuple. Four rsync
workers share the same 128-deep RPC slot table we already unlocked in
row 10 — application-layer parallelism and in-kernel RPC parallelism
are the same knob expressed two different ways.

### 2.3 Why IRQ pinning didn't help

The hypothesis was that softirq on `cpu2` was CPU-bound. It wasn't:
`cpu2` still finished runs below 100 % utilisation (other cores took a
few hundred softirq ticks too but nothing was saturated). Pinning
moved the softirq off the rsync consumer core and broke rsync's
cache locality with the NFS page cache, netting a small regression.

### 2.4 Why CUBIC lost to BBR here

Against a low-latency LAN source, CUBIC's AIMD window-growth curve
takes longer to reach the same cwnd than BBR's bandwidth-probing
model. On a steady-state single flow they converge, but the many
short idle windows between rsync files (close → open next) restart
the ramp each time. BBR recovers faster. Stay on BBR.

## 3. Conclusions

**The single-flow NFSv4 ceiling on a 1500-MTU 2.5 Gbps link is ~92 % of
line (2.3 Gbps).** The remaining ~8 % is structural:

- ~3 % TCP + IP header overhead per 1500-byte frame
- ~2 % RPC/NFS framing + round-trip per RPC boundary
- ~2 % page-cache writeback dips (visible as 1.9 Gbps valleys every
  few seconds when dirty → 0 in a single tick)
- ~1 % single-TCP ramp + ACK-pacing overhead at file boundaries

Recovering more requires relaxing the single-flow constraint, which
means either `nconnect>1` (vetoed), jumbo frames end-to-end (infra
change), or a faster physical link.

## 4. Changes landed in this PR

- **`internal/tuning/tuning.go`**: raise
  `sunrpc.tcp_slot_table_entries` from the upstream default of 2 to
  128 at boot via `ApplyNetworkTuning()`. This is the one change with
  measured impact on the workload (+10 pp of line, 82 → 92 %).

## 5. Follow-up recommendations (separate PRs)

### 5.1 `smoothkernel` — compile-time RPC slot default

Patch `include/linux/sunrpc/xprt.h` so `RPC_DEF_SLOT_TABLE` is 128
rather than the upstream default. Tierd's sysctl runs late in boot;
early NFS mounts (initramfs rescue paths, systemd early `mnt-*`
units ordered before `tierd.service`) miss it. Baking the default
into the appliance-shipped kernel is strictly better and carries no
downside — max-slot ceiling stays at 65536 and `tcp_slot_table_entries`
can still be overridden at runtime.

A ready-to-apply patch ships alongside this proposal:
[`smoothkernel-rpc-slot-default.patch`](./smoothkernel-rpc-slot-default.patch).
One-line patch; build cost zero.

### 5.2 `smoothfs` — tier-spill on near-ENOSPC *(highest impact remaining)*

`smoothfs_create` in `src/smoothfs/inode.c` unconditionally creates
the lower file on the `fastest_tier`. If tier0 is at or near full,
`->create` on the lower returns `ENOSPC` and rsync aborts — which is
how every run of the full /backup subtree on this test VM died in the
first place.

Design sketch (the actual change warrants its own PR):

1. In `smoothfs_create` and `smoothfs_mkdir`, start with the tier
   the parent dentry is on (usually `fastest_tier`, but `0..ntiers-1`
   once subdirectories land on non-fastest tiers).
2. Iterate `tier = parent_tier ..< ntiers`. For each tier:
   a. If the tier differs from the parent's, resolve the parent
      path on this tier via `smoothfs_resolve_rel_path`, creating
      any missing intermediate directories (`vfs_mkdir`-per-component).
      Record each intermediate as PLACED on this tier in the placement
      log so subsequent `smoothfs_lookup` finds it.
   b. Call `smoothfs_compat_create` / `_mkdir` on that tier's
      `lower_parent`.
   c. On success → break; record PLACED for the new object; store
      its lower_path in the smoothfs inode.
   d. On `-ENOSPC` → continue to next tier.
3. If all tiers exhausted → return `-ENOSPC` as today.
4. `smoothfs_lookup` already falls back to `smoothfs_lookup_rel_path`
   via the placement log on negative parent-tier lookups, so spilled
   files and their parent chains are findable on subsequent opens.
5. On `smoothfs_unlink` / `_rmdir`, the existing code already walks
   the placement log to find which tier the object lives on; no
   change needed.

This is correctness before it is throughput — any backup larger than
tier0 is currently impossible on the fast-tier-smaller-than-data
configuration. But it also feeds throughput: once tier0's write-cache
saturates on real NVMe under sustained 2.3 Gbps in, spilling sequential
bulk writes to tier1 (ZFS) keeps the pipe full instead of collapsing.

Not included in this PR because it touches `smoothfs_create`,
`smoothfs_mkdir`, `smoothfs_lookup` placement-log interactions, and
needs proper test coverage in `src/smoothfs/test/`. Warrants a
dedicated PR against the smoothfs module.

### 5.3 Already in smoothfs (no further work needed)

Two items from earlier drafts are already landed and do not need
new work:

- **splice fast path**: `smoothfs_file_ops` already defines
  `.splice_read = smoothfs_splice_read` and
  `.splice_write = smoothfs_splice_write`.
- **dedicated BDI** was proposed on a faulty premise: smoothfs is
  pure passthrough in Phase 1 (`smoothfs_aops` is empty; pages are
  owned by the lower filesystem's address_space). Dirty pages land
  on the lower's BDI, not smoothfs's, so registering our own BDI
  changes nothing about writeback scheduling. The visible
  dirty-cycle dips in the throughput trace come from tier0's XFS
  BDI hitting its thresholds and are controllable via the existing
  system-level `vm.dirty_*` sysctls (already raised in
  `tuning.ApplyNetworkTuning`). No smoothfs change required.

`rsync --copy-file-range` was also considered and dropped: the
`copy_file_range(2)` syscall falls back to read/write for
cross-filesystem copies, and backup source (NFS) is always a
different filesystem from destination (smoothfs). So a
`.copy_file_range` hook on smoothfs wouldn't fire for this workload.

### 5.4 Infra (not kernel)

Jumbo frames (MTU 9000) on the backup VLAN would close most of the
remaining 8 % gap. Test VM cannot demonstrate this because the bridge
is at 1500. For production: raise `br0` MTU on PVE, raise NIC MTU on
the source NAS, raise the smoothnas host NIC MTU, verify with
`ping -M do -s 8972`. Then the backup should land at 96–97 % of line
on the same workload. This is a smoothnas *operator-runbook* item,
not a code change.

## 6. Why not just raise `nconnect`

The operator has tested `nconnect>1` against this source NAS in the
past and observed reduced overall throughput — counter to the Linux
documentation's idealised expectation. The likely cause is that the
source server serialises NFS writes on its own internal locking (one
ZFS dataset, one write queue), so opening additional TCP connections
adds contention without adding parallel write throughput. This is a
property of the specific source's NFS server implementation, not a
generic property of `nconnect`. Do not re-add `nconnect` to
`backup.go:nfsMountOpts` without a fresh measurement against the
current real source.
