-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
-- Phase 10 of the plugin system (smoothnas-plugins, plugins-10-compose-services):
-- a plugin owns a *set* of containers (compose-style) rather than a single
-- image. This introduces a per-service anchor table (plugin_services) and folds
-- a `service` dimension into every per-image child table. The run unit becomes
-- (plugin_name, service, instance).
--
-- Existing single-image plugins migrate to exactly one service whose name is the
-- plugin name (matching the convention that a single-container plugin is one
-- service named after the plugin). artifact_type/image_ref/distro_summary move
-- to plugin_services; the columns on `plugins` are left in place as legacy
-- mirrors and are no longer authoritative.
--
-- SQLite can't widen a PRIMARY KEY in place, so each child table is rebuilt via
-- the standard create-new / copy / drop / rename dance with foreign keys off.

PRAGMA foreign_keys=off;

CREATE TABLE plugin_services (
    plugin_name    TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    service        TEXT    NOT NULL,
    artifact_type  TEXT    NOT NULL,
    image_ref      TEXT,
    distro_summary TEXT,
    depends_on     TEXT,                       -- JSON: {"postgres":"service_healthy",...}, nullable
    health         TEXT,                       -- JSON: serialized Healthcheck, nullable
    ordinal        INTEGER NOT NULL DEFAULT 0, -- precomputed topological start order
    PRIMARY KEY (plugin_name, service)
);

INSERT INTO plugin_services
    (plugin_name, service, artifact_type, image_ref, distro_summary, ordinal)
SELECT name, name, artifact_type, image_ref, distro_summary, 0
FROM plugins;

-- plugin_instances: (plugin_name, service, instance)
CREATE TABLE plugin_instances_new (
    plugin_name   TEXT    NOT NULL,
    service       TEXT    NOT NULL,
    instance      INTEGER NOT NULL,
    container_id  TEXT,
    state         TEXT    NOT NULL,
    bridge_ip     TEXT,
    last_change   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_error    TEXT,
    PRIMARY KEY (plugin_name, service, instance),
    FOREIGN KEY (plugin_name, service)
        REFERENCES plugin_services(plugin_name, service) ON DELETE CASCADE
);
INSERT INTO plugin_instances_new
    (plugin_name, service, instance, container_id, state, bridge_ip, last_change, last_error)
SELECT plugin_name, plugin_name, instance, container_id, state, bridge_ip, last_change, last_error
FROM plugin_instances;
DROP TABLE plugin_instances;
ALTER TABLE plugin_instances_new RENAME TO plugin_instances;

-- plugin_volumes: (plugin_name, service, volume_name)
CREATE TABLE plugin_volumes_new (
    plugin_name  TEXT    NOT NULL,
    service      TEXT    NOT NULL,
    volume_name  TEXT    NOT NULL,
    mode         TEXT    NOT NULL,
    slot         TEXT,
    tier_pool    TEXT,
    per_instance INTEGER NOT NULL DEFAULT 0,
    bind_path    TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, service, volume_name),
    FOREIGN KEY (plugin_name, service)
        REFERENCES plugin_services(plugin_name, service) ON DELETE CASCADE
);
INSERT INTO plugin_volumes_new
    (plugin_name, service, volume_name, mode, slot, tier_pool, per_instance, bind_path)
SELECT plugin_name, plugin_name, volume_name, mode, slot, tier_pool, per_instance, bind_path
FROM plugin_volumes;
DROP TABLE plugin_volumes;
ALTER TABLE plugin_volumes_new RENAME TO plugin_volumes;

-- plugin_volume_paths: (plugin_name, service, volume_name, instance)
CREATE TABLE plugin_volume_paths_new (
    plugin_name  TEXT    NOT NULL,
    service      TEXT    NOT NULL,
    volume_name  TEXT    NOT NULL,
    instance     INTEGER NOT NULL,
    host_path    TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, service, volume_name, instance),
    FOREIGN KEY (plugin_name, service, volume_name)
        REFERENCES plugin_volumes(plugin_name, service, volume_name) ON DELETE CASCADE
);
INSERT INTO plugin_volume_paths_new
    (plugin_name, service, volume_name, instance, host_path)
SELECT plugin_name, plugin_name, volume_name, instance, host_path
FROM plugin_volume_paths;
DROP TABLE plugin_volume_paths;
ALTER TABLE plugin_volume_paths_new RENAME TO plugin_volume_paths;

-- plugin_ports: (plugin_name, service, port_name)
CREATE TABLE plugin_ports_new (
    plugin_name    TEXT    NOT NULL,
    service        TEXT    NOT NULL,
    port_name      TEXT    NOT NULL,
    container_port INTEGER NOT NULL,
    protocol       TEXT    NOT NULL,
    expose         INTEGER NOT NULL,
    host_expose    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plugin_name, service, port_name),
    FOREIGN KEY (plugin_name, service)
        REFERENCES plugin_services(plugin_name, service) ON DELETE CASCADE
);
INSERT INTO plugin_ports_new
    (plugin_name, service, port_name, container_port, protocol, expose, host_expose)
SELECT plugin_name, plugin_name, port_name, container_port, protocol, expose, host_expose
FROM plugin_ports;
DROP TABLE plugin_ports;
ALTER TABLE plugin_ports_new RENAME TO plugin_ports;

-- plugin_config: (plugin_name, service, key)
CREATE TABLE plugin_config_new (
    plugin_name  TEXT    NOT NULL,
    service      TEXT    NOT NULL,
    key          TEXT    NOT NULL,
    value        TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, service, key),
    FOREIGN KEY (plugin_name, service)
        REFERENCES plugin_services(plugin_name, service) ON DELETE CASCADE
);
INSERT INTO plugin_config_new
    (plugin_name, service, key, value)
SELECT plugin_name, plugin_name, key, value
FROM plugin_config;
DROP TABLE plugin_config;
ALTER TABLE plugin_config_new RENAME TO plugin_config;

PRAGMA foreign_keys=on;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Best-effort rollback: collapse the service dimension back to one row per
-- plugin. Lossy for genuine multi-service plugins (rows from services other
-- than the plugin-named one are dropped); intended for dev rollback only.

PRAGMA foreign_keys=off;

CREATE TABLE plugin_instances_old (
    plugin_name   TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    instance      INTEGER NOT NULL,
    container_id  TEXT,
    state         TEXT    NOT NULL,
    bridge_ip     TEXT,
    last_change   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_error    TEXT,
    PRIMARY KEY (plugin_name, instance)
);
INSERT OR IGNORE INTO plugin_instances_old
    (plugin_name, instance, container_id, state, bridge_ip, last_change, last_error)
SELECT plugin_name, instance, container_id, state, bridge_ip, last_change, last_error
FROM plugin_instances WHERE service = plugin_name;
DROP TABLE plugin_instances;
ALTER TABLE plugin_instances_old RENAME TO plugin_instances;

CREATE TABLE plugin_volumes_old (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    volume_name  TEXT    NOT NULL,
    mode         TEXT    NOT NULL,
    slot         TEXT,
    tier_pool    TEXT,
    per_instance INTEGER NOT NULL DEFAULT 0,
    bind_path    TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, volume_name)
);
INSERT OR IGNORE INTO plugin_volumes_old
    (plugin_name, volume_name, mode, slot, tier_pool, per_instance, bind_path)
SELECT plugin_name, volume_name, mode, slot, tier_pool, per_instance, bind_path
FROM plugin_volumes WHERE service = plugin_name;
DROP TABLE plugin_volumes;
ALTER TABLE plugin_volumes_old RENAME TO plugin_volumes;

CREATE TABLE plugin_volume_paths_old (
    plugin_name  TEXT    NOT NULL,
    volume_name  TEXT    NOT NULL,
    instance     INTEGER NOT NULL,
    host_path    TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, volume_name, instance),
    FOREIGN KEY (plugin_name, volume_name)
        REFERENCES plugin_volumes(plugin_name, volume_name) ON DELETE CASCADE
);
INSERT OR IGNORE INTO plugin_volume_paths_old
    (plugin_name, volume_name, instance, host_path)
SELECT plugin_name, volume_name, instance, host_path
FROM plugin_volume_paths WHERE service = plugin_name;
DROP TABLE plugin_volume_paths;
ALTER TABLE plugin_volume_paths_old RENAME TO plugin_volume_paths;

CREATE TABLE plugin_ports_old (
    plugin_name    TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    port_name      TEXT    NOT NULL,
    container_port INTEGER NOT NULL,
    protocol       TEXT    NOT NULL,
    expose         INTEGER NOT NULL,
    host_expose    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plugin_name, port_name)
);
INSERT OR IGNORE INTO plugin_ports_old
    (plugin_name, port_name, container_port, protocol, expose, host_expose)
SELECT plugin_name, port_name, container_port, protocol, expose, host_expose
FROM plugin_ports WHERE service = plugin_name;
DROP TABLE plugin_ports;
ALTER TABLE plugin_ports_old RENAME TO plugin_ports;

CREATE TABLE plugin_config_old (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    key          TEXT    NOT NULL,
    value        TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, key)
);
INSERT OR IGNORE INTO plugin_config_old
    (plugin_name, key, value)
SELECT plugin_name, key, value
FROM plugin_config WHERE service = plugin_name;
DROP TABLE plugin_config;
ALTER TABLE plugin_config_old RENAME TO plugin_config;

DROP TABLE plugin_services;

PRAGMA foreign_keys=on;

-- +goose StatementEnd
