-- +goose Up
ALTER TABLE backup_configs ADD COLUMN schedule_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backup_configs ADD COLUMN schedule_time TEXT NOT NULL DEFAULT '';
ALTER TABLE backup_configs ADD COLUMN schedule_days INTEGER NOT NULL DEFAULT 127;
ALTER TABLE backup_configs ADD COLUMN last_scheduled_at TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 1;
