-- +goose Up
-- Pinned tier placement for a compose plugin's x-smoothnas volume (plugins-11
-- Phase 2). Records volume -> {pool, host_path} resolved at first materialise so
-- a later re-materialise reuses the SAME host path even if the operator edits the
-- compose's x-smoothnas.tier -- preventing SILENT DATA RELOCATION. An explicit
-- retier (REPIN) is a separate operator action, not a compose edit.
CREATE TABLE plugin_compose_volumes (
    plugin_name TEXT NOT NULL,
    volume_name TEXT NOT NULL,
    pool        TEXT NOT NULL,
    host_path   TEXT NOT NULL,
    min_size    TEXT,
    pinned_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (plugin_name, volume_name),
    FOREIGN KEY (plugin_name) REFERENCES plugins(name) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE plugin_compose_volumes;
