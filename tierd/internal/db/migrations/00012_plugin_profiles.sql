-- +goose Up
-- Phase 5 of the plugin system (smoothnas-plugins, plugins-05-profiles):
-- record the resolved profile list (after default-limits injection
-- and operator-side overrides) per plugin, so a future tierd does
-- not have to re-resolve against a possibly-changed catalog to know
-- what was applied at install time.
--
-- The manifest in `plugins.manifest` is the source of truth for
-- *requested* profiles; this column is the *applied* set.

ALTER TABLE plugins ADD COLUMN profiles_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; tierd targets
-- newer SQLite (the migration framework already relies on WAL
-- mode and FK pragmas), so the explicit DROP is safe here.
ALTER TABLE plugins DROP COLUMN profiles_json;
