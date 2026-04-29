# Proposal: Unified Tiering 06 Snapshot UI Remainder

**Status:** Done
**Split from:** [`unified-tiering-06-coordinated-snapshots.md`](./unified-tiering-06-coordinated-snapshots.md)

---

## Context

Coordinated namespace snapshot support is implemented in the backend: the managed ZFS adapter records `SnapshotMode`, exposes create/list/get/delete snapshot APIs, persists snapshot records, performs worker quiesce, and emits the `snapshot_timeout` degraded state.

The Tiering Inventory UI still only shows snapshot mode and the multi-pool informational note. It does not expose snapshot creation, listing, detail, or deletion.

## Scope

1. Add a Snapshot action for namespaces whose `snapshot_mode` is `coordinated-namespace`.
2. Add a namespace snapshot list panel populated from `GET /api/tiering/namespaces/{id}/snapshots`.
3. Add delete confirmation and wire `DELETE /api/tiering/namespaces/{id}/snapshots/{snapshot_id}`.
4. Show inline API errors for timeout, topology mismatch, and concurrent snapshot conflicts.
5. Keep the multi-pool informational note for managed ZFS namespaces with `snapshot_mode = none`.

## Acceptance Criteria

- [x] The Snapshot button appears only for managed ZFS namespaces with `snapshot_mode = coordinated-namespace`.
- [x] Created snapshots appear in newest-first order with consistency status.
- [x] Delete removes the snapshot row after successful API completion.
- [x] UI errors are visible for 409, 422, and timeout degraded-state cases.
