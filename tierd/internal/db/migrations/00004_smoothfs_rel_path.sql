-- +goose Up
-- +goose StatementBegin

-- Phase 2.1: track each object's path relative to its pool's
-- exposed namespace root. The Phase 2 worker used the object_id hex
-- as a placeholder filename; that wrote files with random names on
-- the destination tier. rel_path lets the worker rebuild the real
-- src/dst lower paths.

ALTER TABLE smoothfs_objects ADD COLUMN rel_path TEXT NOT NULL DEFAULT '';

CREATE INDEX smoothfs_objects_rel_path
    ON smoothfs_objects(namespace_id, rel_path);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS smoothfs_objects_rel_path;

-- SQLite < 3.35 has no DROP COLUMN; rebuild the table without rel_path.
CREATE TABLE smoothfs_objects_pre_2_1 AS
SELECT object_id, namespace_id, current_tier_id, intended_tier_id,
       movement_state, transaction_seq, last_committed_cutover_gen,
       pin_state, nlink, created_at, updated_at,
       ewma_value, last_heat_sample_at, last_movement_at, failure_reason
FROM smoothfs_objects;
DROP TABLE smoothfs_objects;
ALTER TABLE smoothfs_objects_pre_2_1 RENAME TO smoothfs_objects;

-- +goose StatementEnd
