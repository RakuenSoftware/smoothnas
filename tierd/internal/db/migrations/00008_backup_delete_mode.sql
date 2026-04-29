-- +goose Up
ALTER TABLE backup_configs
    ADD COLUMN delete_mode INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite has no DROP COLUMN in the compatibility mode we target here.
-- Keep the column on rollback; backup config rows remain readable.
SELECT 1;
