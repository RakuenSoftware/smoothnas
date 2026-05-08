-- +goose Up
-- Phase 7 of the plugin system (smoothnas-plugins, plugins-07-iframe-embed):
-- per-plugin bearer tokens for the nginx auth-injection flow. The
-- plugin's nginx route (phase 04) sets
--   proxy_set_header Authorization "Bearer <token>";
-- for plugins whose manifest declares ui.embed.auth=bearer-injected,
-- so the embedded UI sees an authenticated request without doing
-- its own login.
--
-- Tokens are 64 hex chars (256 random bits). Rotated by
-- POST /api/plugins/<name>/rotate-token; on rotate the new value
-- replaces the row and the nginx route is re-applied + reloaded
-- in one atomic step.
--
-- ON DELETE CASCADE so the row goes away with the plugin row at
-- uninstall time.

CREATE TABLE plugin_secrets (
    plugin_name  TEXT    PRIMARY KEY REFERENCES plugins(name) ON DELETE CASCADE,
    bearer_token TEXT    NOT NULL,
    issued_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- +goose Down
DROP TABLE IF EXISTS plugin_secrets;
