-- +goose Up
ALTER TABLE smoothfs_pools ADD COLUMN mode TEXT NOT NULL DEFAULT 'tier' CHECK (mode IN ('tier', 'cache'));
ALTER TABLE tier_pools ADD COLUMN storage_mode TEXT NOT NULL DEFAULT 'tier' CHECK (storage_mode IN ('tier', 'cache'));
-- +goose Down
ALTER TABLE tier_pools DROP COLUMN storage_mode;
ALTER TABLE smoothfs_pools DROP COLUMN mode;
