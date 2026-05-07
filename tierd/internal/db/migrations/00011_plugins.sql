-- +goose Up
-- Phase 1 of the plugin system (smoothnas-plugins, plugins-01-foundation):
-- declarative plugins with both OCI-image and lxc-distro artifacts, real
-- isolation under LXC system containers, and full integration with the
-- named-tier model. This migration lands the schema only; no runtime
-- columns reference LXC2Docker yet (those are populated in phase 02).
--
-- Multi-instance is first-class from the start: every plugin has at
-- least one row in plugin_instances (instance = 1) and every volume
-- has at least one row in plugin_volume_paths. Single-instance plugins
-- are simply N=1; phase 09 (gh-runner) is the first plugin that sets
-- N>1 in practice. This avoids a schema migration when multi-instance
-- becomes user-visible.

CREATE TABLE plugins (
    name                  TEXT    PRIMARY KEY,
    version               TEXT    NOT NULL,
    state                 TEXT    NOT NULL,
    manifest              TEXT    NOT NULL,
    artifact_type         TEXT    NOT NULL,
    image_ref             TEXT,
    distro_summary        TEXT,
    instance_count        INTEGER NOT NULL DEFAULT 1,
    instance_configurable INTEGER NOT NULL DEFAULT 0,
    installed_at          TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at            TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE plugin_instances (
    plugin_name   TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    instance      INTEGER NOT NULL,
    container_id  TEXT,
    state         TEXT    NOT NULL,
    bridge_ip     TEXT,
    last_change   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_error    TEXT,
    PRIMARY KEY (plugin_name, instance)
);

CREATE TABLE plugin_volumes (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    volume_name  TEXT    NOT NULL,
    mode         TEXT    NOT NULL,
    slot         TEXT,
    tier_pool    TEXT,
    per_instance INTEGER NOT NULL DEFAULT 0,
    bind_path    TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, volume_name)
);

CREATE TABLE plugin_volume_paths (
    plugin_name  TEXT    NOT NULL,
    volume_name  TEXT    NOT NULL,
    instance     INTEGER NOT NULL,
    host_path    TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, volume_name, instance),
    FOREIGN KEY (plugin_name, volume_name)
        REFERENCES plugin_volumes(plugin_name, volume_name) ON DELETE CASCADE
);

CREATE TABLE plugin_ports (
    plugin_name    TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    port_name      TEXT    NOT NULL,
    container_port INTEGER NOT NULL,
    protocol       TEXT    NOT NULL,
    expose         INTEGER NOT NULL,
    host_expose    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plugin_name, port_name)
);

CREATE TABLE plugin_config (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    key          TEXT    NOT NULL,
    value        TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, key)
);

-- +goose Down
DROP TABLE IF EXISTS plugin_config;
DROP TABLE IF EXISTS plugin_ports;
DROP TABLE IF EXISTS plugin_volume_paths;
DROP TABLE IF EXISTS plugin_volumes;
DROP TABLE IF EXISTS plugin_instances;
DROP TABLE IF EXISTS plugins;
