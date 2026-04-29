-- +goose Up
-- Per-user preferences. Today only `language` is stored, but the
-- table is keyed by username so future preference rows can be added
-- as additional columns without a migration churn (just ALTER TABLE).
--
-- The auth subsystem stores user identity at the OS level (Linux
-- system users via getent). This table is the appliance-side
-- supplement for UI-only state that has no place in /etc/passwd.
CREATE TABLE IF NOT EXISTS user_prefs (
    username  TEXT PRIMARY KEY,
    language  TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE IF EXISTS user_prefs;
