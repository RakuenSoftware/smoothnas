-- +goose Up
-- New exports are async by default. Rebuild the table so existing installs
-- get the default without changing any already-created export rows.
CREATE TABLE nfs_exports_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT    NOT NULL,
    networks    TEXT    NOT NULL DEFAULT '',
    sync_mode   INTEGER NOT NULL DEFAULT 0,
    root_squash INTEGER NOT NULL DEFAULT 1,
    read_only   INTEGER NOT NULL DEFAULT 0,
    nfsv3       INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO nfs_exports_new (id, path, networks, sync_mode, root_squash, read_only, nfsv3, created_at)
SELECT id, path, networks, sync_mode, root_squash, read_only, nfsv3, created_at
FROM nfs_exports;

DROP TABLE nfs_exports;
ALTER TABLE nfs_exports_new RENAME TO nfs_exports;

-- +goose Down
SELECT 1;
