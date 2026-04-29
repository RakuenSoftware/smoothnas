-- +goose Up
-- +goose StatementBegin

-- Phase 2 columns on smoothfs_objects: heat aggregation + last-movement
-- bookkeeping per Phase 0 §0.5. ewma_value uses the IEEE 754 double bit
-- pattern so it survives unmodified through SQLite REAL.

ALTER TABLE smoothfs_objects ADD COLUMN ewma_value REAL NOT NULL DEFAULT 0.0;
ALTER TABLE smoothfs_objects ADD COLUMN last_heat_sample_at TEXT;
ALTER TABLE smoothfs_objects ADD COLUMN last_movement_at TEXT;
ALTER TABLE smoothfs_objects ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '';

-- Per-object movement transition log. One row per state transition,
-- including the payload the kernel emitted (reserved dest inode,
-- copied byte count, cutover commit timestamp, etc.) as JSON. The log
-- is the userspace mirror of the kernel's per-pool placement.log;
-- together they support the deterministic crash repair in Phase 0 §0.8.
CREATE TABLE smoothfs_movement_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    object_id       TEXT    NOT NULL CHECK(length(object_id) = 32),
    transaction_seq INTEGER NOT NULL,
    from_state      TEXT,
    to_state        TEXT    NOT NULL,
    source_tier     TEXT,
    dest_tier       TEXT,
    payload_json    TEXT    NOT NULL DEFAULT '{}',
    written_at      TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX smoothfs_movement_log_object
    ON smoothfs_movement_log(object_id, transaction_seq);

CREATE INDEX smoothfs_movement_log_active
    ON smoothfs_movement_log(to_state, written_at)
    WHERE to_state NOT IN ('cleanup_complete','failed','stale');

-- Seed control_plane_config with Phase 2 tunables. Values match the
-- defaults in Phase 0 §0.5.
INSERT OR IGNORE INTO control_plane_config (key, value) VALUES
    ('smoothfs_heat_halflife_seconds',     '86400'),  -- 24h EWMA half-life
    ('smoothfs_planner_interval_seconds',  '900'),    -- 15min between planner runs
    ('smoothfs_min_residency_seconds',     '3600'),   -- 1h before movement allowed
    ('smoothfs_movement_cooldown_seconds', '21600'),  -- 6h after a move
    ('smoothfs_hysteresis_pct',            '20'),     -- 20% relative hysteresis
    ('smoothfs_promote_percentile',        '80'),     -- promote above this percentile
    ('smoothfs_demote_percentile',         '20'),     -- demote below this percentile
    ('smoothfs_movement_worker_count',     '4');      -- per-pool concurrency

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM control_plane_config WHERE key IN (
    'smoothfs_heat_halflife_seconds',
    'smoothfs_planner_interval_seconds',
    'smoothfs_min_residency_seconds',
    'smoothfs_movement_cooldown_seconds',
    'smoothfs_hysteresis_pct',
    'smoothfs_promote_percentile',
    'smoothfs_demote_percentile',
    'smoothfs_movement_worker_count'
);

DROP INDEX IF EXISTS smoothfs_movement_log_active;
DROP INDEX IF EXISTS smoothfs_movement_log_object;
DROP TABLE IF EXISTS smoothfs_movement_log;

-- SQLite < 3.35 doesn't support DROP COLUMN. The four columns added
-- above are dropped via table-recreate. For dev scratch DBs this is
-- fine; production never rolls back the baseline.
CREATE TABLE smoothfs_objects_pre_phase2 AS
SELECT object_id, namespace_id, current_tier_id, intended_tier_id,
       movement_state, transaction_seq, last_committed_cutover_gen,
       pin_state, nlink, created_at, updated_at
FROM smoothfs_objects;
DROP TABLE smoothfs_objects;
ALTER TABLE smoothfs_objects_pre_phase2 RENAME TO smoothfs_objects;

-- +goose StatementEnd
