-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin

PRAGMA foreign_keys=off;

CREATE TABLE tier_pools_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    filesystem          TEXT NOT NULL DEFAULT 'xfs'
        CHECK (filesystem IN ('xfs','ext4','btrfs','bcachefs')),
    state               TEXT NOT NULL CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying')),
    error_reason        TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    last_reconciled_at  TEXT,
    region_size_mb      INTEGER NOT NULL DEFAULT 256,
    meta_on_fastest     INTEGER NOT NULL DEFAULT 0,
    CHECK (
        (state = 'error'      AND error_reason IS NOT NULL) OR
        (state = 'destroying') OR
        (state NOT IN ('error','destroying') AND error_reason IS NULL)
    )
);

INSERT INTO tier_pools_new
    (id, name, filesystem, state, error_reason, created_at, updated_at,
     last_reconciled_at, region_size_mb, meta_on_fastest)
SELECT id, name, filesystem, state, error_reason, created_at, updated_at,
       last_reconciled_at, region_size_mb, meta_on_fastest
FROM tier_pools;

DROP TABLE tier_pools;
ALTER TABLE tier_pools_new RENAME TO tier_pools;

PRAGMA foreign_keys=on;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

PRAGMA foreign_keys=off;

CREATE TABLE tier_pools_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    filesystem          TEXT NOT NULL DEFAULT 'xfs' CHECK (filesystem IN ('xfs')),
    state               TEXT NOT NULL CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying')),
    error_reason        TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    last_reconciled_at  TEXT,
    region_size_mb      INTEGER NOT NULL DEFAULT 256,
    meta_on_fastest     INTEGER NOT NULL DEFAULT 0,
    CHECK (
        (state = 'error'      AND error_reason IS NOT NULL) OR
        (state = 'destroying') OR
        (state NOT IN ('error','destroying') AND error_reason IS NULL)
    )
);

INSERT INTO tier_pools_new
    (id, name, filesystem, state, error_reason, created_at, updated_at,
     last_reconciled_at, region_size_mb, meta_on_fastest)
SELECT id, name, 'xfs', state, error_reason, created_at, updated_at,
       last_reconciled_at, region_size_mb, meta_on_fastest
FROM tier_pools;

DROP TABLE tier_pools;
ALTER TABLE tier_pools_new RENAME TO tier_pools;

PRAGMA foreign_keys=on;

-- +goose StatementEnd
