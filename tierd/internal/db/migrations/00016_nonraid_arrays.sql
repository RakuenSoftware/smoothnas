-- +goose Up
-- +goose StatementBegin

CREATE TABLE nonraid_arrays (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL UNIQUE,
    state             TEXT    NOT NULL DEFAULT 'configured'
        CHECK (state IN ('configured','active','degraded','stopped','error','destroying')),
    uuid              TEXT    NOT NULL DEFAULT '',
    filesystem        TEXT    NOT NULL DEFAULT 'xfs'
        CHECK (filesystem IN ('xfs')),
    mount_path        TEXT    NOT NULL,
    parity_count      INTEGER NOT NULL CHECK (parity_count BETWEEN 1 AND 3),
    min_parity_bytes  INTEGER NOT NULL DEFAULT 0,
    capacity_bytes    INTEGER NOT NULL DEFAULT 0,
    error_reason      TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE nonraid_array_devices (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    array_id     INTEGER NOT NULL REFERENCES nonraid_arrays(id) ON DELETE CASCADE,
    role         TEXT    NOT NULL CHECK (role IN ('data','parity')),
    slot         INTEGER NOT NULL CHECK (slot > 0),
    device_path  TEXT    NOT NULL UNIQUE,
    virtual_device_path TEXT NOT NULL DEFAULT '',
    serial       TEXT    NOT NULL DEFAULT '',
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    usable_bytes INTEGER NOT NULL DEFAULT 0,
    mount_path   TEXT    NOT NULL DEFAULT '',
    state        TEXT    NOT NULL DEFAULT 'configured'
        CHECK (state IN ('configured','active','missing','disabled','rebuilding','error')),
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (array_id, role, slot)
);

CREATE INDEX idx_nonraid_array_devices_array_role
    ON nonraid_array_devices(array_id, role, slot);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_nonraid_array_devices_array_role;
DROP TABLE IF EXISTS nonraid_array_devices;
DROP TABLE IF EXISTS nonraid_arrays;

-- +goose StatementEnd
