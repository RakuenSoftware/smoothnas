-- +goose Up
-- +goose StatementBegin

-- smoothfs_objects: per-object kernel-mirror placement record.
-- Per smoothfs-phase-0-contract.md Appendix B.

CREATE TABLE smoothfs_objects (
    object_id                  TEXT PRIMARY KEY CHECK(length(object_id) = 32),
    namespace_id               TEXT NOT NULL,
    current_tier_id            TEXT NOT NULL,
    intended_tier_id           TEXT,
    movement_state             TEXT NOT NULL DEFAULT 'placed'
        CHECK (movement_state IN (
            'placed',
            'plan_accepted',
            'destination_reserved',
            'copy_in_progress',
            'copy_complete',
            'copy_verified',
            'cutover_in_progress',
            'switched',
            'cleanup_in_progress',
            'cleanup_complete',
            'failed',
            'stale'
        )),
    transaction_seq            INTEGER NOT NULL DEFAULT 0,
    last_committed_cutover_gen INTEGER NOT NULL DEFAULT 0,
    pin_state                  TEXT NOT NULL DEFAULT 'none'
        CHECK (pin_state IN ('none','pin_hot','pin_cold','pin_hardlink','pin_lease','pin_lun')),
    nlink                      INTEGER NOT NULL DEFAULT 1,
    created_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (namespace_id)     REFERENCES managed_namespaces(id),
    FOREIGN KEY (current_tier_id)  REFERENCES tier_targets(id),
    FOREIGN KEY (intended_tier_id) REFERENCES tier_targets(id)
);

CREATE INDEX smoothfs_objects_namespace
    ON smoothfs_objects(namespace_id);

CREATE INDEX smoothfs_objects_movement
    ON smoothfs_objects(namespace_id, movement_state)
    WHERE movement_state NOT IN ('placed','cleanup_complete','failed');

-- Tighten placement_intents.object_id format for smoothfs-backed
-- namespaces only; legacy mdadm/zfs-managed backends may continue to
-- write NULL because their per-tier meta records are inode-keyed.
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER placement_intents_smoothfs_object_id_format
BEFORE INSERT ON placement_intents
WHEN (SELECT backend_kind FROM managed_namespaces WHERE id = NEW.namespace_id) = 'smoothfs'
  AND (NEW.object_id IS NULL OR length(NEW.object_id) != 32)
BEGIN
    SELECT RAISE(ABORT, 'smoothfs placement_intents row requires object_id of length 32');
END;
-- +goose StatementEnd

-- mdadm_movement_log uses a looser default state. For smoothfs-backed
-- mdadm pools we enforce the nine-state vocabulary defined in
-- smoothfs-phase-0-contract.md §0.3.
-- +goose StatementBegin
CREATE TRIGGER mdadm_movement_log_smoothfs_state
BEFORE INSERT ON mdadm_movement_log
WHEN (SELECT backend_kind FROM managed_namespaces WHERE id = NEW.namespace_id) = 'smoothfs'
  AND NEW.state NOT IN (
      'placed','plan_accepted','destination_reserved',
      'copy_in_progress','copy_complete','copy_verified',
      'cutover_in_progress','switched',
      'cleanup_in_progress','cleanup_complete',
      'failed','stale'
  )
BEGIN
    SELECT RAISE(ABORT, 'smoothfs mdadm_movement_log row uses unknown state');
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS mdadm_movement_log_smoothfs_state;
DROP TRIGGER IF EXISTS placement_intents_smoothfs_object_id_format;
DROP INDEX IF EXISTS smoothfs_objects_movement;
DROP INDEX IF EXISTS smoothfs_objects_namespace;
DROP TABLE IF EXISTS smoothfs_objects;
-- +goose StatementEnd
