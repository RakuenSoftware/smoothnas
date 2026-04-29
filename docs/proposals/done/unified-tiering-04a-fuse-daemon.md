# Proposal: Unified Tiering — 04A: FUSE Daemon Infrastructure

**Status:** Done
**Depends on:** unified-tiering-03-zfs-raw-backend
**Part of:** unified-tiering-control-plane
**Preceded by:** unified-tiering-03-zfs-raw-backend
**Followed by:** unified-tiering-04b-zfs-managed-adapter

---

## Context

This proposal is P04A, one half of a split from the original P04 (Managed ZFS Adapter). The original proposal was rated XL and has been divided into two bounded pieces:

- **P04A (this file)**: the C FUSE daemon, Unix socket protocol, kernel passthrough and fallback, fd validation, directory protocol, bypass detection, and daemon lifecycle supervision.
- **P04B**: the Go `TieringAdapter` implementation, metadata dataset schema, movement workers, synchronous recall, and crash recovery.

P04B depends on P04A: the adapter cannot move files or serve placements until the daemon IPC contract is stable.

---

## Problem

Raw ZFS datasets (proposal 03) expose backing storage directly. To provide a controlled namespace where placement decisions are enforced at `open()` time, tierd needs a FUSE daemon that intercepts all file access and routes backing-fd selection through the placement engine.

go-fuse cannot be used: it lacks libfuse3's passthrough fd API, imposing a 10–30% throughput penalty on every read and write — unacceptable for a NAS appliance. Wrapping libfuse3 in CGo inside tierd is also rejected: libfuse3 uses pthreads internally, and CGo forces goroutines onto OS threads for every C call, conflicting with Go's scheduler under libfuse3's threading model.

The solution is a separate, single-purpose C daemon (~500–1,000 lines). The daemon is a thin relay. It contains no placement logic. All placement decisions live in tierd (Go).

---

## Goals

1. Specify and implement the C FUSE daemon using libfuse3.
2. Define the full Unix socket protocol between the daemon and tierd, including file and directory operations.
3. Implement kernel passthrough fd support for Linux 6.x and a correct fallback for older kernels.
4. Implement fd validation via `SCM_RIGHTS` and `fstat`.
5. Implement daemon lifecycle supervision (start, monitor, restart with backoff) inside tierd.
6. Implement bypass prevention and detection via dataset permissions and `fanotify`.

---

## Non-goals

- The `TieringAdapter` Go interface implementation (P04B).
- Metadata dataset schema (P04B).
- File movement workers (P04B).
- Synchronous recall-on-access (P04B).
- The mixed-backend GUI (proposal 05).
- Coordinated namespace snapshots (proposal 05).

---

## Architecture

```text
application / NFS / SMB
  → kernel FUSE driver
  → C FUSE daemon (libfuse3)
      on open():        Unix socket query → tierd → backing fd returned to kernel
      on read()/write(): kernel passthrough directly to backing fd (no userspace hop)
      on opendir()/readdir(): daemon serves from local in-memory directory cache
      on metadata stat(): daemon serves from local directory cache
      on release():     notify tierd fd is closed
  → tierd (Go)
      Unix socket listener per namespace
      authoritative placement metadata
      directory tree snapshot pushed to daemon on each CollectActivity() cycle
      daemon supervisor (start, monitor, restart)
      fanotify watch on backing mountpoints
```

Placement decisions happen at `open()` time only. After the C daemon registers a passthrough fd with the kernel, subsequent reads and writes on that fd bypass userspace entirely. Directory and metadata operations are served from the daemon's local cache without a socket round-trip per call.

---

## Unix Socket Protocol

The C FUSE daemon and tierd communicate over a Unix domain socket. tierd creates the socket and begins listening before starting the daemon. One socket exists per managed namespace, at a fixed path:

```
/run/tierd/fuse-<namespace-id>.sock
```

The daemon connects on startup. If the connection is refused or fails, the daemon exits immediately (it cannot serve any opens without tierd). tierd then applies the restart-with-backoff policy described in the Daemon Lifecycle section.

### Message Framing

All messages use a fixed 8-byte header:

| Bytes | Field |
|-------|-------|
| 0–3   | Message type (uint32 little-endian) |
| 4–7   | Payload length in bytes (uint32 little-endian) |

Followed by exactly `payload_length` bytes of payload. Maximum payload size is 65,536 bytes. Any message whose payload length field exceeds this limit is rejected: the receiver sends an `ERROR` message and closes the connection.

Messages are framed on the socket as complete header+payload units. The receiver must read the full header before reading any payload bytes. Partial reads must be retried; a short read that returns EOF before the header is complete is treated as a connection error.

### Message Types

| Type | Value | Direction | Description |
|------|-------|-----------|-------------|
| `OPEN_REQUEST`    | 1 | daemon → tierd | Request the backing fd for a namespace object |
| `OPEN_RESPONSE`   | 2 | tierd → daemon | Backing fd (via `SCM_RIGHTS`) plus placement metadata |
| `RELEASE_NOTIFY`  | 3 | daemon → tierd | Notify tierd that an fd obtained via `OPEN_RESPONSE` has been closed |
| `HEALTH_PING`     | 4 | tierd → daemon | Liveness check sent by tierd on a fixed interval |
| `HEALTH_PONG`     | 5 | daemon → tierd | Liveness response to `HEALTH_PING` |
| `ERROR`           | 6 | either direction | Protocol error; sender closes the connection after sending |
| `DIR_UPDATE`      | 7 | tierd → daemon | Push a full or incremental directory tree snapshot to the daemon |

### OPEN_REQUEST Payload

A null-terminated namespace object key encoded as UTF-8. Maximum key length is 4,096 bytes including the null terminator. Keys longer than this limit cause the daemon to send an `ERROR` message.

### OPEN_RESPONSE Payload and fd Passing

| Bytes | Field |
|-------|-------|
| 0     | Result code (1 byte; 0 = success, non-zero = error) |
| 1–8   | Inode number of the backing file (uint64 little-endian); present only on success |

The backing fd is passed out-of-band via `SCM_RIGHTS` ancillary data attached to the same `sendmsg` call that carries the `OPEN_RESPONSE` message. On success (result code 0), exactly one fd is attached. On error (non-zero result code), no fd is attached and the inode field is absent; the payload is exactly 1 byte.

**Result codes:**

| Code | Meaning |
|------|---------|
| 0    | Success — fd attached and valid |
| 1    | Object not found in namespace |
| 2    | Backend degraded — backing dataset unavailable |
| 3    | Placement error (temporary) — placement engine could not resolve a backing target; client may retry |
| 4    | Reserved — must not be returned; recall blocks the response until complete, so the daemon never sees a "recall in progress" code |
| 5    | Permission denied — caller does not have access to this object |

The daemon must map non-zero result codes to appropriate POSIX errors: code 1 → `ENOENT`; code 2 → `EIO`; code 3 → `EAGAIN`; code 5 → `EACCES`. Unknown codes → `EIO`.

### RELEASE_NOTIFY Payload

The inode number of the backing file being released (uint64 little-endian, 8 bytes). tierd uses this to update open-fd accounting and determine when a backing file is safe to move or promote.

### HEALTH_PING / HEALTH_PONG

Both messages have zero-length payloads. The sequence is:

1. tierd sends `HEALTH_PING` every 10 seconds.
2. The daemon must respond with `HEALTH_PONG` within 5 seconds of receiving the ping.
3. If tierd does not receive `HEALTH_PONG` within 5 seconds, it logs a warning and marks one missed ping.
4. If three consecutive pings go unanswered, tierd treats the daemon as dead: it reports `namespace_unavailable` degraded state and begins the restart-and-remount sequence (see Daemon Lifecycle).

The daemon must not perform blocking work in the `HEALTH_PING` handler; it replies immediately from the same thread or a dedicated IPC thread.

### DIR_UPDATE Payload

tierd pushes directory tree snapshots to the daemon so that `opendir`, `readdir`, and `stat` calls can be served from the daemon's local in-memory cache without a socket round-trip per call.

The payload encodes the full logical directory tree for the namespace as a sequence of directory entry records. Each record is:

| Bytes | Field |
|-------|-------|
| 0–7   | Inode number (uint64 little-endian) |
| 8     | Entry type: 0 = regular file, 1 = directory |
| 9–10  | Path length in bytes (uint16 little-endian), not including null |
| 11 .. 11+path_length | UTF-8 path relative to namespace root, null-terminated |
| Following | stat-compatible metadata: mode (uint32), uid (uint32), gid (uint32), size (uint64), mtime_sec (int64), mtime_nsec (uint32) |

Records are packed end-to-end with no alignment padding. The end of the payload marks the end of the record list.

On receiving `DIR_UPDATE`, the daemon atomically replaces its entire in-memory directory snapshot. In-flight `readdir` calls that started before the update completes continue against the old snapshot; new calls after the swap use the new snapshot. The daemon must not block incoming FUSE callbacks while applying a `DIR_UPDATE`.

tierd sends a `DIR_UPDATE` at mount time (to populate the initial cache) and once per `CollectActivity()` cycle thereafter. It does not send incremental diffs; each update is a full snapshot. If the payload would exceed 65,536 bytes, tierd splits the namespace into multiple sequential `DIR_UPDATE` messages; the daemon applies them in order and treats arrival of a new batch start (distinguished by a sequence number in reserved bytes 0–3 of the payload, not described in the entry format above) as the beginning of a new atomic replacement. Implementations may defer full-snapshot splitting to a later iteration and document large-namespace support as a known limitation until that point.

---

## C FUSE Daemon

### fd Passing and Validation

tierd opens backing files and passes the open fds to the C daemon over the Unix socket using `SCM_RIGHTS` ancillary data. The C daemon never opens backing files directly and cannot bypass placement logic by choosing its own path. tierd is the sole arbiter of which backing fd maps to which namespace object.

Before registering any received fd as a passthrough target or forwarding I/O through it, the daemon must validate the fd:

1. Call `fstat` on the received fd.
2. Confirm that the reported inode number matches the inode number in the `OPEN_RESPONSE` payload.
3. Confirm that the device number corresponds to the expected backing ZFS dataset (daemon has a table of expected `st_dev` values populated at mount time from a `MOUNT_INFO` payload sent by tierd before the daemon begins serving opens; this message type is internal and not part of the public protocol).

If any validation step fails, the daemon:
- closes the received fd
- returns `EIO` for the pending `open()` FUSE call
- sends a structured `ERROR` message to tierd indicating `fd_pass_failed`

tierd, on receiving this `ERROR`, transitions the namespace to `fd_pass_failed` degraded state and logs the backing path and inode that were expected.

### Kernel Version Detection and Passthrough

`FUSE_PASSTHROUGH` is a kernel capability introduced in Linux 6.x. The daemon must detect support at mount time:

- If the kernel reports `FUSE_PASSTHROUGH` in its capability flags during the FUSE handshake: the daemon enables passthrough mode. On each successful `open()` (after fd validation), the daemon calls `fuse_passthrough_enable()` (or the equivalent low-level API) to register the backing fd with the kernel. Subsequent `read()` and `write()` calls on that FUSE file descriptor go directly from the kernel to the backing file without passing through userspace.
- If `FUSE_PASSTHROUGH` is not present: the daemon falls back to traditional read/write handlers. On `read()`, the daemon reads from the backing fd and copies the data to the FUSE reply buffer. On `write()`, the daemon reads from the FUSE request buffer and writes to the backing fd. This path is safe and correct; it is slower.

The fallback path must be exercised in the test suite and must not be removed. The API (served by tierd) reports the active FUSE mode as `passthrough` or `fallback` in the managed namespace capability field.

### Directory Operations

Directory listing and file metadata (`stat`) operations are not routed through the socket protocol. Routing every `readdir` and `stat` through a synchronous socket call to tierd would serialize all metadata operations through a single IPC channel, producing unacceptable latency for NFS and SMB clients that issue many concurrent stat calls during directory traversals.

Instead, the daemon maintains a local in-memory directory cache populated at mount time and refreshed on each `CollectActivity()` cycle via `DIR_UPDATE` messages (see Unix Socket Protocol). The daemon serves `opendir`, `readdir`, and `getattr` (for existing entries) directly from this cache.

Cache staleness: the cache is at most one `CollectActivity()` cycle stale. For the NAS workloads targeted here, this is acceptable. Clients doing rapid `create`/`stat` round-trips may observe brief inconsistency. This is a documented limitation.

The daemon does not synthesize missing entries. If an object key appears in an `OPEN_REQUEST` that is not present in the current directory cache, the daemon treats the state as stale and forwards the open to tierd anyway; tierd is authoritative.

### Daemon Lifecycle Supervision

tierd is responsible for starting, supervising, and restarting the C FUSE daemon. The daemon is an external process managed by tierd as a subprocess.

**Startup sequence:**

1. tierd creates the Unix socket and begins listening.
2. tierd starts the daemon subprocess, passing the namespace id and socket path as command-line arguments.
3. tierd sends a `DIR_UPDATE` containing the initial directory snapshot.
4. tierd waits for the daemon to signal readiness (FUSE mount confirmed) via a startup handshake (an out-of-band mechanism such as a startup-complete pipe or a polling check against the mountpoint).
5. Only after the daemon is confirmed live and the FUSE mount is healthy does tierd begin serving API traffic for the managed namespace.

**On unexpected daemon exit:**

1. In-flight I/O from applications receives errors (the FUSE mount becomes inaccessible).
2. tierd detects the exit via `waitpid` or an equivalent process-monitoring mechanism.
3. tierd reports `namespace_unavailable` degraded state.
4. tierd attempts to restart the daemon with exponential backoff, starting at 1 second and doubling up to a cap of 60 seconds.
5. tierd caps the number of restart attempts within any rolling 5-minute window. If the cap is exceeded, tierd stops restarting and holds `namespace_unavailable` until an operator intervenes or the adapter is reconciled.
6. On successful restart: tierd re-sends `DIR_UPDATE` with the current snapshot; the daemon proceeds normally. Existing open fds held by applications before the crash become invalid; applications must reopen.

**HEALTH_PING-triggered restart:**

If three consecutive `HEALTH_PING` messages go unanswered within their respective 5-second windows, tierd treats the daemon as dead. It transitions the namespace to `namespace_unavailable` and applies the same restart procedure as for an unexpected exit.

---

## Bypass Prevention and Detection

### Permission Enforcement

Backing datasets must not be reachable by operators through normal ZFS paths:

- Backing datasets are owned by the `tierd` system user: `tierd:tierd`, mode `700`.
- `zfs allow` must not grant dataset-level read or write access to operator accounts for adapter-owned datasets.
- Backing dataset mountpoints are not under any path exposed to users. They are not under `/mnt` paths or any path reachable through the managed namespace.

These permissions are enforced at dataset creation time by tierd and verified on each `Reconcile()` call.

### fanotify Watch

Permissions alone are not sufficient: a sufficiently privileged operator (root) can still bypass them. tierd therefore installs a `fanotify` watch on each backing dataset mountpoint at namespace startup. The watch monitors `FAN_OPEN` and `FAN_CREATE` events.

On any event, tierd checks whether the originating PID is the C FUSE daemon for this namespace. If not, tierd:

1. Logs the event with the offending path, PID, and process name.
2. Transitions the namespace to `bypass_detected` degraded state.
3. Does not kill the offending process (that is an operator decision), but does emit a structured audit log entry.

Note: `atime`/`mtime`/`ctime` comparison is not used for bypass detection. It produces false positives under `relatime` and misses reads entirely under `noatime`. The `fanotify` approach catches access at the VFS layer before data is served.

### Fallback on Older Kernels

`fanotify` is available from Linux 2.6.37. On kernels older than this (unlikely in a modern NAS deployment but possible in a test environment), tierd falls back to periodic `fstat` comparison of backing file metadata. This fallback is significantly less reliable (it misses in-memory-only accesses and has a polling interval gap). tierd reports `bypass_detection_unavailable` degraded state when operating in this mode so operators are aware.

---

## Backing Dataset Layout (Normative for P04A)

The daemon relies on a fixed relationship between the namespace id, the socket path, and the set of backing dataset mountpoints it is permitted to validate fds against. This layout is normative for the IPC contract; P04B defines the full schema.

```text
/run/tierd/fuse-<namespace-id>.sock   — Unix socket (created by tierd)
/run/tierd/fuse-<namespace-id>.pid    — PID file written by daemon on startup
backing dataset mountpoints:          — communicated to daemon via MOUNT_INFO at startup
exposed FUSE path:                    /mnt/tiering/<namespace-id>
```

The daemon must not hardcode any backing path. All backing paths are communicated by tierd at mount time.

---

## Performance Characteristics

The numbers below reflect measured overhead relative to direct ZFS dataset access and are used to set operator expectations. They are not pass/fail acceptance criteria except where noted.

| Workload | Traditional FUSE (fallback) | libfuse3 + passthrough (Linux 6.x) |
|----------|-----------------------------|-------------------------------------|
| Sequential read/write throughput | −10–30% vs bare ZFS | −2–5% vs bare ZFS |
| Per read/write latency | +5–20 μs | ~0 (kernel direct path) |
| Random small I/O (IOPS) | −20–50% vs bare ZFS | −5–15% vs bare ZFS |
| Per `open()` latency | +5–20 μs | +6–25 μs (FUSE round-trip + IPC to tierd) |

The residual `open()` overhead with passthrough is the cost of the socket round-trip to tierd. For NAS workloads (large sequential I/O, infrequent file opens) this is acceptable. For workloads that open thousands of small files per second, the per-open IPC cost is noticeable and operators should use raw ZFS datasets (proposal 03) instead.

NFS and SMB exports through the FUSE namespace benefit from passthrough: the data path is NFS/SMB kernel handler → FUSE passthrough → backing dataset, with no userspace hop on read/write.

The fallback path retains the traditional FUSE overhead and is documented as a known performance limitation for operators on kernels below Linux 6.x.

---

## Degraded States (P04A scope)

| Code | Condition |
|------|-----------|
| `namespace_unavailable` | C FUSE daemon has exited or failed three consecutive HEALTH_PINGs; mount is inaccessible |
| `fd_pass_failed` | Received fd failed inode or device validation; open returned EIO |
| `bypass_detected` | Backing dataset was accessed outside the FUSE namespace (detected via fanotify) |
| `bypass_detection_unavailable` | fanotify not available on this kernel; fallback bypass detection active |

The states `no_drain_target`, `movement_failed`, `reconciliation_required`, `placement_intent_stale`, and `recall_timeout` are defined in P04B.

---

## Effort

**L** — the FUSE daemon and IPC protocol are a significant but well-scoped C+Go subsystem. The daemon is approximately 500–1,000 lines of C (libfuse3 + socket client + fd validation + passthrough registration + directory cache). The tierd side adds a socket server, message dispatcher, process supervisor, and fanotify watcher in Go. This is a single focused sprint deliverable, distinct from the movement and recall work in P04B.

---

## Acceptance Criteria

- [ ] The namespace service is a C FUSE daemon using libfuse3, not go-fuse or a raw dataset mount.
- [ ] On Linux 6.x, the daemon registers a passthrough fd with the kernel on each `open()`; reads and writes do not pass through userspace.
- [ ] On kernels without `FUSE_PASSTHROUGH`, the daemon falls back to traditional read/write handlers and continues serving I/O correctly.
- [ ] The API reports the active FUSE mode (`passthrough` or `fallback`) as a managed namespace capability field.
- [ ] tierd passes backing fds to the daemon via `SCM_RIGHTS`; the daemon validates each fd with `fstat` against the inode in `OPEN_RESPONSE` and the expected backing device before registering it.
- [ ] On fd validation failure, the daemon returns `EIO` for the open and tierd reports `fd_pass_failed` degraded state.
- [ ] The Unix socket is at `/run/tierd/fuse-<namespace-id>.sock`; the 8-byte header and message type encoding match the specification in this document.
- [ ] `OPEN_REQUEST` carries a null-terminated UTF-8 object key; keys exceeding 4,096 bytes cause the daemon to send an `ERROR` and close the connection.
- [ ] `OPEN_RESPONSE` error codes 1–5 are implemented with the defined semantics; the daemon maps them to the correct POSIX errors.
- [ ] `RELEASE_NOTIFY` carries the inode of the released backing fd and is sent by the daemon on every FUSE `release()` callback.
- [ ] tierd sends `HEALTH_PING` every 10 seconds; the daemon replies with `HEALTH_PONG` within 5 seconds.
- [ ] Three consecutive unanswered pings cause tierd to treat the daemon as dead, report `namespace_unavailable`, and begin the restart sequence.
- [ ] tierd sends `DIR_UPDATE` at mount time and once per `CollectActivity()` cycle; the daemon applies updates atomically without blocking in-flight FUSE callbacks.
- [ ] `opendir`, `readdir`, and `getattr` calls are served from the daemon's local directory cache without a socket round-trip to tierd.
- [ ] tierd supervises the daemon and reports `namespace_unavailable` on unexpected exit; it attempts restart with exponential backoff (1 s doubling to 60 s cap) and caps restart attempts within a 5-minute window.
- [ ] tierd does not begin serving API traffic for a namespace until the daemon is confirmed live and the FUSE mount is healthy.
- [ ] Backing datasets are owned `tierd:tierd`, mode `700`; `zfs allow` grants no operator access; dataset mountpoints are not under user-visible paths.
- [ ] tierd installs a `fanotify` watch on each backing dataset mountpoint; any `FAN_OPEN` or `FAN_CREATE` event from a non-daemon PID triggers `bypass_detected` and a structured audit log entry.
- [ ] On kernels without `fanotify`, tierd reports `bypass_detection_unavailable` degraded state.
- [ ] All backing paths are communicated to the daemon by tierd at mount time; no backing paths are hardcoded in the daemon binary.

---

## Test Plan

- [ ] Integration test: daemon skeleton mounts, connects to the tierd socket, receives `DIR_UPDATE`, serves opens, and unmounts cleanly.
- [ ] Integration test: `SCM_RIGHTS` fd passing delivers a valid fd; daemon validates inode and device; passthrough is registered correctly with the kernel on Linux 6.x.
- [ ] Integration test: daemon receives an fd whose inode does not match `OPEN_RESPONSE`; returns `EIO`; tierd reports `fd_pass_failed`.
- [ ] Integration test: passthrough fallback on a kernel without `FUSE_PASSTHROUGH` — reads and writes complete correctly through the traditional handler; throughput shows expected overhead range.
- [ ] Integration test: `OPEN_RESPONSE` result code 1 (not found) → daemon returns `ENOENT`; code 2 (backend degraded) → `EIO`; code 3 (placement error) → `EAGAIN`; code 5 (permission denied) → `EACCES`.
- [ ] Integration test: `OPEN_REQUEST` with a key longer than 4,096 bytes causes daemon to send `ERROR` and close the connection; tierd detects and handles the closure.
- [ ] Integration test: tierd sends `HEALTH_PING`; daemon replies with `HEALTH_PONG` within 5 seconds.
- [ ] Integration test: daemon's `HEALTH_PONG` handler is blocked (simulated); tierd logs warnings on missed pings; after three consecutive missed pings, tierd reports `namespace_unavailable` and begins restart.
- [ ] Integration test: `DIR_UPDATE` pushed at mount time populates the daemon cache; `readdir` and `stat` calls are served from cache without socket activity; a second `DIR_UPDATE` atomically replaces the cache and in-flight `readdir` calls complete against the old snapshot.
- [ ] Integration test: `opendir`/`readdir`/`getattr` calls do not produce socket messages to tierd (confirmed by inspecting the socket message log).
- [ ] Integration test: daemon exits unexpectedly; tierd detects exit, reports `namespace_unavailable`, waits the backoff interval, restarts, re-sends `DIR_UPDATE`, and the namespace becomes healthy; subsequent opens succeed.
- [ ] Integration test: daemon is restarted more times than the rolling-window cap within 5 minutes; tierd stops restarting and holds `namespace_unavailable`.
- [ ] Integration test: `RELEASE_NOTIFY` is sent by the daemon after each FUSE `release()` callback; tierd updates open-fd accounting.
- [ ] Integration test: direct `open()` on a backing dataset mountpoint from a non-daemon PID triggers `bypass_detected` via the `fanotify` watch; audit log entry is emitted.
- [ ] Integration test: tierd started on a kernel without `fanotify` support reports `bypass_detection_unavailable` degraded state.
- [ ] Integration test: full `OPEN_REQUEST` / `OPEN_RESPONSE` / `RELEASE_NOTIFY` round-trip completes with correct framing; parsed message types and payload lengths match the wire encoding.
- [ ] Throughput benchmark: sequential read and write through the managed namespace on Linux 6.x with passthrough active is within 5% of direct ZFS dataset access.
- [ ] Throughput benchmark: sequential read and write on the fallback path shows overhead in the −10–30% range documented as expected.
