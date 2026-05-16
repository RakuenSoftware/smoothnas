-- +goose Up
-- +goose StatementBegin

CREATE TABLE filesystem_arrays (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT    NOT NULL UNIQUE,
    kind             TEXT    NOT NULL CHECK (kind IN ('btrfs','bcachefs')),
    label            TEXT    NOT NULL UNIQUE,
    mount_path       TEXT    NOT NULL UNIQUE,
    data_profile     TEXT    NOT NULL DEFAULT '',
    metadata_profile TEXT    NOT NULL DEFAULT '',
    replicas         INTEGER NOT NULL DEFAULT 1,
    state            TEXT    NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','error','destroying')),
    size_bytes       INTEGER NOT NULL DEFAULT 0,
    error_reason     TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE filesystem_array_devices (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    array_id    INTEGER NOT NULL REFERENCES filesystem_arrays(id) ON DELETE CASCADE,
    device_path TEXT    NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    state       TEXT    NOT NULL DEFAULT 'active',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (array_id, device_path),
    UNIQUE (device_path)
);

CREATE INDEX idx_filesystem_array_devices_array
    ON filesystem_array_devices(array_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_filesystem_array_devices_array;
DROP TABLE IF EXISTS filesystem_array_devices;
DROP TABLE IF EXISTS filesystem_arrays;

-- +goose StatementEnd
