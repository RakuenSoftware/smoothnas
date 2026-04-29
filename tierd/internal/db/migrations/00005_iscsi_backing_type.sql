-- +goose Up
-- Phase 7.5 — tierd can now create file-backed LUNs on smoothfs.
-- The existing iscsi_targets row captures block-backed targets via
-- the block_device column; add a backing_type column that
-- distinguishes them from fileio-backed ones so the REST and CLI
-- layers pick the right iscsi.* builder (iscsi.CreateTarget vs
-- iscsi.CreateFileBackedTarget from Phase 6.5).
ALTER TABLE iscsi_targets ADD COLUMN backing_type TEXT NOT NULL DEFAULT 'block';
-- +goose Down
-- SQLite ALTER TABLE can't DROP COLUMN pre-3.35; re-create the
-- table without backing_type on rollback.
CREATE TABLE iscsi_targets_prev75 (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    iqn          TEXT    NOT NULL UNIQUE,
    block_device TEXT    NOT NULL,
    chap_user    TEXT    NOT NULL DEFAULT '',
    chap_pass    TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL
);
INSERT INTO iscsi_targets_prev75 (id, iqn, block_device, chap_user, chap_pass, created_at)
    SELECT id, iqn, block_device, chap_user, chap_pass, created_at
    FROM iscsi_targets;
DROP TABLE iscsi_targets;
ALTER TABLE iscsi_targets_prev75 RENAME TO iscsi_targets;
