-- +goose Up
-- OCI refs a plugin is based on. The primary ref is the runtime
-- container image; auxiliary refs let plugins track helper/base
-- containers independently of plugin manifest releases.

CREATE TABLE plugin_container_refs (
    plugin_name  TEXT NOT NULL,
    service      TEXT NOT NULL,
    ref_name     TEXT NOT NULL,
    image_ref    TEXT NOT NULL,
    digest       TEXT,
    resolved_ref TEXT,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (plugin_name, service, ref_name),
    FOREIGN KEY (plugin_name, service)
        REFERENCES plugin_services(plugin_name, service) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS plugin_container_refs;
