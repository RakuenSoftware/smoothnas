# Proposal: Unified Tiering — 06: Coordinated Namespace Snapshots for Managed ZFS

**Status:** Pending
**Date:** 2026-04-12
**Depends on:** unified-tiering-04b-zfs-managed-adapter, unified-tiering-05-mixed-backend-ui
**Part of:** unified-tiering-control-plane
**Preceded by:** unified-tiering-05-mixed-backend-ui

---

## Problem

Proposal 05 (mixed-backend UI) deferred coordinated namespace snapshots because a critical design constraint was unresolved at the time of writing. Proposal 05 contained the following placeholder:

> The managed ZFS adapter must take a `zfs snapshot` of all backing datasets atomically (within a single `zfs snapshot -r` or equivalent).

This is wrong in the general case. `zfs snapshot -r` recurses within one pool hierarchy — it does not cross pool boundaries. A managed ZFS namespace whose tier backing datasets span multiple zpools (e.g. `pool_fast`, `pool_warm`, `pool_cold`) has no OpenZFS primitive that snapshots all three atomically. Two separate `zfs snapshot` invocations cannot be made atomic; they will always reflect different TXG commits.

This proposal resolves the constraint, defines the only valid implementation, and specifies the full design that proposal 05 deferred.

---

## Goals

1. Define the atomicity constraint for coordinated namespace snapshots and its consequence for namespace configuration.
2. Implement coordinated namespace snapshots for single-pool managed ZFS namespaces using a single `zfs snapshot` invocation that covers all backing datasets and the metadata dataset in one TXG.
3. Implement quiesce semantics that ensure the snapshot reflects a consistent namespace state: no mid-copy movement workers, no new file-creation races.
4. Record each coordinated snapshot in durable adapter-owned metadata.
5. Expose snapshot list, detail, and delete API endpoints.
6. Add UI elements for snapshot creation, listing, deletion, and the multi-pool informational note.
7. Update the adapter to set `SnapshotMode = coordinated-namespace` when pool membership allows it, and `SnapshotMode = none` when it does not.

---

## Non-goals

- Cross-pool coordinated snapshots. There is no atomic OpenZFS primitive for this; it will not be implemented.
- Snapshot restore or rollback. Snapshots created by this mechanism can be destroyed; restoring them is out of scope.
- mdadm namespace snapshots. Not addressed by this proposal.
- Changes to the raw ZFS backend (proposal 03).
- Changes to the mdadm adapter (proposal 02).
- Any change to movement policy or the FUSE daemon data path beyond the quiesce signal.

---

## Decision: Single-Pool Constraint

The only valid atomic multi-dataset snapshot in OpenZFS is a single `zfs snapshot` call listing all target datasets in one command:

```
zfs snapshot pool/fast@snap pool/warm@snap pool/cold@snap pool/meta@snap
```

When all named datasets share the same pool, ZFS commits all snapshot operations in a single Transaction Group (TXG). This is atomic: the resulting snapshots reflect the same on-disk state and no write can appear in some snapshots but not others.

**This only holds when all datasets are in the same pool.** If `pool/fast` and `pool_warm/warm` are in different pools, their TXGs are independent; there is no way to align them.

**Consequence for namespace configuration:**

- When a managed ZFS namespace is created and all tier backing datasets and the metadata dataset reside within the same zpool, the adapter records the pool name and sets `SnapshotMode = coordinated-namespace`.
- When backing datasets span more than one zpool, `SnapshotMode = none`. No coordinated snapshot endpoint is active for that namespace. An informational note is surfaced in the UI.

The adapter performs this check at namespace creation time and records the result durably. Re-checking on every snapshot call is not sufficient; the pool topology must not be silently re-evaluated mid-lifecycle.

---

## Pool Membership Detection

At namespace creation, the adapter resolves the zpool name for each backing dataset and the metadata dataset. The resolution uses `zfs get -H -o value name <dataset>` to obtain the canonical dataset name, then splits on `/` to extract the pool component.

If all resolved pool names are identical, the namespace is single-pool. The pool name is stored in the namespace record (see Snapshot Record Format below). `SnapshotMode` is set to `coordinated-namespace`.

If any pool name differs, the namespace is multi-pool. `SnapshotMode` is set to `none`. The pool name field in the namespace record is left empty.

Pool membership is re-verified at snapshot request time as a safety check. If the topology has changed (e.g. a backing dataset was moved to another pool outside of tierd), the snapshot is aborted and a degraded state is reported. This is an operator error condition, not a normal operating case.

---

## Quiesce Semantics

A namespace snapshot captures the FUSE namespace, not the underlying datasets in isolation. The namespace has two sources of mutation at snapshot time:

1. **New file creations** via the FUSE mount (O_CREAT opens).
2. **In-progress movement workers** (copy/verify/switch/cleanup sequences).

Both must be quiesced before the `zfs snapshot` command is issued to ensure the snapshot is consistent.

### Quiesce Protocol

1. The FUSE daemon receives a quiesce signal from tierd over the Unix socket. After this point, any `open()` call with `O_CREAT` returns `EBUSY`. Existing open file descriptors continue to operate normally; reads and writes through already-open fds are not blocked.

2. tierd sends a quiesce checkpoint request to all movement workers for the namespace. Each worker completes its current atomic unit of work (not mid-copy) and then parks. The definition of a safe checkpoint is:
   - A worker in the **copy** phase parks after finishing the current file's copy and fsync, before beginning the next file.
   - A worker in the **verify** or **switch** phase parks after completing the full verify-switch-cleanup sequence for the current file, before starting the next file.
   - A worker in the **cleanup** phase parks after the current cleanup completes.
   Workers in a parked state hold no locks on backing datasets.

3. Once all movement workers have reported parked and the FUSE daemon has confirmed O_CREAT is blocked, tierd issues the single `zfs snapshot` command.

4. After the `zfs snapshot` command returns, tierd sends a release signal to the FUSE daemon and to all movement workers. O_CREAT is accepted again; workers resume from their parked positions.

### Quiesce Timeout

If any movement worker has not reported parked within `snapshot_quiesce_timeout_seconds` (configurable, default 30 seconds) from the moment the quiesce signal is sent, the snapshot is aborted:

1. tierd sends a release signal to the FUSE daemon and to any already-parked workers immediately.
2. No `zfs snapshot` command is issued.
3. A `snapshot_timeout` degraded state is recorded for the namespace with severity `warning`.
4. The API call that triggered the snapshot returns an error indicating timeout.
5. The namespace continues operating normally.

The `snapshot_quiesce_timeout_seconds` value is stored in the namespace configuration and is validated at namespace creation time. Minimum value: 5 seconds. Maximum value: 300 seconds. Default: 30 seconds.

---

## Snapshot Record Format

Each coordinated snapshot record is stored in the adapter metadata dataset. Records are stored as JSON files under `<metadata_dataset>/snapshots/<snapshot_id>.json`.

Fields:

| Field | Type | Description |
| --- | --- | --- |
| `snapshot_id` | UUID (string) | Unique identifier for this snapshot record. |
| `namespace_id` | UUID (string) | The namespace this snapshot belongs to. |
| `pool_name` | string | The single zpool containing all backing and metadata datasets. |
| `zfs_snapshot_name` | string | The ZFS snapshot name suffix applied to all datasets (e.g. `tiering-snap-<snapshot_id>`). |
| `backing_snapshots` | array of objects | One entry per backing dataset: `{dataset_path, snapshot_name}`. |
| `metadata_snapshot` | object | `{dataset_path, snapshot_name}` for the metadata dataset. |
| `created_at` | RFC 3339 timestamp | Wall clock time when the snapshot completed. |
| `consistency` | string enum | `atomic` when the snapshot was committed in a single TXG. `none` is reserved for error or fallback paths; a snapshot record with `consistency: none` should never be created by a correct implementation and signals an internal error if encountered. |

The `zfs_snapshot_name` suffix is `tiering-snap-<snapshot_id>` where `<snapshot_id>` is the UUID without hyphens. The full ZFS snapshot name for a dataset is `<dataset_path>@tiering-snap-<snapshot_id>`.

Example record:

```json
{
  "snapshot_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "namespace_id": "f0e1d2c3-b4a5-6789-fedc-ba0987654321",
  "pool_name": "pool_nas",
  "zfs_snapshot_name": "tiering-snap-a1b2c3d4e5f67890abcdef1234567890",
  "backing_snapshots": [
    {
      "dataset_path": "pool_nas/tiering/fast",
      "snapshot_name": "pool_nas/tiering/fast@tiering-snap-a1b2c3d4e5f67890abcdef1234567890"
    },
    {
      "dataset_path": "pool_nas/tiering/warm",
      "snapshot_name": "pool_nas/tiering/warm@tiering-snap-a1b2c3d4e5f67890abcdef1234567890"
    },
    {
      "dataset_path": "pool_nas/tiering/cold",
      "snapshot_name": "pool_nas/tiering/cold@tiering-snap-a1b2c3d4e5f67890abcdef1234567890"
    }
  ],
  "metadata_snapshot": {
    "dataset_path": "pool_nas/tiering/meta",
    "snapshot_name": "pool_nas/tiering/meta@tiering-snap-a1b2c3d4e5f67890abcdef1234567890"
  },
  "created_at": "2026-04-12T14:23:01Z",
  "consistency": "atomic"
}
```

---

## API

### Existing endpoint (activated by this proposal)

`POST /api/tiering/namespaces/{id}/snapshot` — defined in proposal 01. Returns `202 Accepted` while quiesce is in progress, then `200 OK` with the snapshot record on success, or an error body on timeout or pool topology mismatch.

### New endpoints

| Method | Endpoint | Description |
| --- | --- | --- |
| GET | `/api/tiering/namespaces/{id}/snapshots` | List all coordinated snapshot records for the namespace, ordered by `created_at` descending. |
| GET | `/api/tiering/namespaces/{id}/snapshots/{snapshot_id}` | Return the full snapshot record including all backing snapshot names and consistency indicator. |
| DELETE | `/api/tiering/namespaces/{id}/snapshots/{snapshot_id}` | Destroy the coordinated snapshot. Destroys all backing ZFS snapshots and the metadata snapshot atomically (single `zfs destroy` call listing all snapshot names). Removes the metadata record from the adapter metadata dataset. |

**GET list response** — array of objects, each containing: `snapshot_id`, `namespace_id`, `pool_name`, `created_at`, `consistency`. Backing snapshot details are omitted from the list to keep response size manageable.

**GET detail response** — the full snapshot record as described in Snapshot Record Format.

**DELETE behavior** — The destroy command is:

```
zfs destroy pool_nas/tiering/fast@snap pool_nas/tiering/warm@snap pool_nas/tiering/cold@snap pool_nas/tiering/meta@snap
```

All names are passed in a single `zfs destroy` invocation so the destroy is processed in a single TXG. If any named snapshot does not exist (e.g. was destroyed out-of-band), the destroy call still proceeds for the remaining snapshots and the metadata record is removed. A warning is logged but the DELETE API call succeeds. If `zfs destroy` fails for any reason other than a missing snapshot, the API returns `500 Internal Server Error` and the metadata record is left in place.

**Error responses common to all new endpoints:**
- `404 Not Found` — namespace does not exist, or snapshot record does not exist.
- `409 Conflict` — a snapshot operation is already in progress for this namespace (returned by POST).
- `422 Unprocessable Entity` — `SnapshotMode` is not `coordinated-namespace` (returned by POST).

---

## UI Changes

This proposal adds to the namespace detail panel introduced in proposal 05.

### Snapshot button

The Snapshot button on the namespace detail panel is shown only when `SnapshotMode = coordinated-namespace`. Its behavior is unchanged from the proposal 05 description: clicking it calls `POST /api/tiering/namespaces/{id}/snapshot` and shows a spinner while in progress.

### Snapshot list panel

Below the Snapshot button, a **Snapshots** panel lists existing coordinated snapshots. The panel is visible only when `SnapshotMode = coordinated-namespace`. It is populated from `GET /api/tiering/namespaces/{id}/snapshots`.

Each row shows:
- `created_at` formatted as a local timestamp.
- A consistency indicator: `atomic` shown as a green badge labelled "Atomic". A row with `consistency: none` is shown with a red badge labelled "Inconsistent" and a warning tooltip (this should never occur in normal operation).
- A **Delete** button.

Clicking **Delete** opens a confirmation dialog: "Delete snapshot `<snapshot_id>`? This will permanently destroy the ZFS snapshot data for all backing datasets. This cannot be undone." Confirming sends `DELETE /api/tiering/namespaces/{id}/snapshots/{snapshot_id}`. On success, the row is removed. On failure, an inline error is shown.

The panel shows a maximum of 50 snapshots. If more exist, a note reads "Showing 50 most recent snapshots."

### Multi-pool informational note

When `SnapshotMode = none` on a managed ZFS namespace (as opposed to an mdadm namespace), the namespace detail panel shows an informational note in place of the Snapshot button and Snapshots panel:

> Coordinated snapshots require all tier datasets to be in the same ZFS pool. This namespace uses backing datasets across multiple pools and cannot be snapshotted consistently.

This note is not shown for mdadm namespaces; it is specific to managed ZFS namespaces where the user might otherwise wonder why the snapshot feature is absent.

### `snapshot_timeout` degraded state message

When the `snapshot_timeout` degraded state is active for a namespace, the degraded-state panel (defined in proposal 05) shows:

> Snapshot aborted: quiesce timed out. The namespace is operating normally.

---

## Effort

**M** — Pool membership detection and enforcement is S. The quiesce protocol across FUSE daemon and movement workers is M, particularly the safe-checkpoint logic for workers mid-copy. The atomic `zfs snapshot` / `zfs destroy` invocation construction and the snapshot record persistence are S. The three new API endpoints are S. The UI additions (snapshot list panel, delete confirm dialog, informational note) are S. The coordination between all of these raises the total to M.

---

## Acceptance Criteria

- [ ] A managed ZFS namespace whose backing datasets and metadata dataset all reside in the same zpool reports `SnapshotMode = coordinated-namespace`.
- [ ] A managed ZFS namespace whose backing datasets span more than one zpool reports `SnapshotMode = none`.
- [ ] Pool membership is detected at namespace creation time and stored durably in the namespace record.
- [ ] `POST /api/tiering/namespaces/{id}/snapshot` issues a single `zfs snapshot` command listing all backing datasets and the metadata dataset and succeeds atomically.
- [ ] The resulting snapshot record has `consistency: atomic`.
- [ ] No movement worker is mid-copy when the `zfs snapshot` command is issued.
- [ ] No new file-creation (O_CREAT) can succeed in the FUSE namespace between quiesce start and quiesce release.
- [ ] Existing open file descriptors are not interrupted during quiesce.
- [ ] If quiesce is not complete within `snapshot_quiesce_timeout_seconds`, the snapshot is aborted, no `zfs snapshot` command is issued, and a `snapshot_timeout` degraded state is recorded.
- [ ] After a timeout abort, the namespace is released immediately and resumes normal operation.
- [ ] `GET /api/tiering/namespaces/{id}/snapshots` returns the list of snapshot records ordered by `created_at` descending.
- [ ] `GET /api/tiering/namespaces/{id}/snapshots/{snapshot_id}` returns the full snapshot record including all backing snapshot names.
- [ ] `DELETE /api/tiering/namespaces/{id}/snapshots/{snapshot_id}` destroys all backing ZFS snapshots in a single `zfs destroy` invocation and removes the metadata record.
- [ ] A `zfs destroy` where one backing snapshot is missing out-of-band succeeds for the remaining snapshots and removes the metadata record, logging a warning.
- [ ] `POST /api/tiering/namespaces/{id}/snapshot` returns `422` when `SnapshotMode = none`.
- [ ] The Snapshot button and Snapshots panel appear in the UI only when `SnapshotMode = coordinated-namespace`.
- [ ] The multi-pool informational note appears in the namespace detail panel for managed ZFS namespaces with `SnapshotMode = none`.
- [ ] The `snapshot_timeout` degraded state message reads: "Snapshot aborted: quiesce timed out. The namespace is operating normally."
- [ ] The delete confirm dialog warns that destruction is permanent before sending the DELETE request.

---

## Test Plan

- [ ] **Single-pool atomicity**: create a namespace with three backing datasets and one metadata dataset all in the same pool. Call `POST .../snapshot`. Verify a single `zfs snapshot` command was issued with all four dataset names. Verify all four ZFS snapshots share the same creation TXG (use `zfs get createtxg`). Verify the metadata record has `consistency: atomic`.
- [ ] **Multi-pool SnapshotMode=none**: create a namespace with backing datasets on two different zpools. Verify `SnapshotMode = none` is reported by the namespace GET endpoint. Verify `POST .../snapshot` returns `422`. Verify no `zfs snapshot` command is issued.
- [ ] **Quiesce blocks new file creation**: initiate a snapshot; while quiesce is active, attempt to open a new file with O_CREAT in the FUSE namespace. Verify `EBUSY` is returned. Verify the existing open fds continue to read and write successfully during quiesce.
- [ ] **Quiesce pauses movement workers**: arrange for a movement worker to be mid-copy at snapshot request time. Verify the worker parks after completing the current file's copy and fsync before the `zfs snapshot` command is issued. Verify the worker resumes after quiesce release.
- [ ] **Quiesce timeout**: configure `snapshot_quiesce_timeout_seconds = 5`. Arrange for a movement worker to not park (simulate by holding the checkpoint indefinitely in a test harness). Verify the snapshot is aborted after 5 seconds. Verify no `zfs snapshot` command is issued. Verify `snapshot_timeout` degraded state is reported. Verify the namespace resumes accepting O_CREAT and the movement worker is released.
- [ ] **Snapshot list API**: create three snapshots for a namespace. Call `GET .../snapshots`. Verify three records are returned in descending `created_at` order. Verify each record contains `snapshot_id`, `namespace_id`, `pool_name`, `created_at`, `consistency` and does not contain `backing_snapshots` or `metadata_snapshot`.
- [ ] **Snapshot detail API**: call `GET .../snapshots/{snapshot_id}`. Verify the full record is returned including `backing_snapshots` and `metadata_snapshot` with correct ZFS snapshot names.
- [ ] **Snapshot delete API**: call `DELETE .../snapshots/{snapshot_id}`. Verify a single `zfs destroy` command is issued with all backing and metadata snapshot names. Verify the ZFS snapshots no longer exist. Verify the metadata record is removed from the metadata dataset.
- [ ] **Snapshot delete with missing backing snapshot**: destroy one backing ZFS snapshot out-of-band, then call the DELETE API. Verify the remaining snapshots are destroyed. Verify the metadata record is removed. Verify the API returns success and a warning is logged.
- [ ] **UI: snapshot button visibility**: verify the Snapshot button appears for a managed ZFS namespace with `SnapshotMode = coordinated-namespace` and is absent for a managed ZFS namespace with `SnapshotMode = none` and absent for mdadm namespaces.
- [ ] **UI: multi-pool informational note**: verify the informational note "Coordinated snapshots require all tier datasets to be in the same ZFS pool..." appears for a managed ZFS namespace with `SnapshotMode = none` and does not appear for mdadm namespaces with `SnapshotMode = none`.
- [ ] **UI: snapshot list panel**: after creating two snapshots, verify both appear in the Snapshots panel with `created_at` timestamps and green "Atomic" consistency badges. Verify the most recent appears first.
- [ ] **UI: delete confirm dialog**: click Delete on a snapshot row. Verify the confirmation dialog appears with the snapshot ID. Verify dismissing the dialog does not send the DELETE request. Verify confirming sends the request and the row is removed on success.
- [ ] **UI: snapshot_timeout degraded state**: simulate a quiesce timeout. Verify the degraded-state panel shows "Snapshot aborted: quiesce timed out. The namespace is operating normally."
