-- +goose Up
ALTER TABLE plugins ADD COLUMN stopped_by_user INTEGER NOT NULL DEFAULT 0;
UPDATE plugins SET stopped_by_user = 1 WHERE state = 'stopped';
-- +goose Down
ALTER TABLE plugins DROP COLUMN stopped_by_user;
