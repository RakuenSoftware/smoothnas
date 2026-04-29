-- +goose Up
-- +goose StatementBegin

-- Baseline schema. Consolidated from the prior 53-step numeric migration
-- slice plus the side migrations in shares.go / backups.go / backup_runs.go
-- / smart/{history,alarms}.go / tiering/mdadm/migrate.go. This is the
-- single starting point for any fresh tierd database; further schema
-- changes land as new files in this directory.

-- ---- session and authentication ----

CREATE TABLE sessions (
    token      TEXT PRIMARY KEY,
    username   TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at TEXT NOT NULL
);

CREATE TABLE login_attempts (
    ip           TEXT NOT NULL,
    attempted_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_login_attempts_ip ON login_attempts(ip, attempted_at);

-- ---- appliance configuration (key-value) ----

CREATE TABLE config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ---- legacy mdadm tier-instance management ----

CREATE TABLE tier_instances (
    name        TEXT PRIMARY KEY,
    mount_point TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    state       TEXT NOT NULL DEFAULT 'healthy'
        CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying'))
);

CREATE TABLE tier_instance_arrays (
    tier_name  TEXT NOT NULL,
    slot       TEXT NOT NULL CHECK (slot IN ('NVME','SSD','HDD')),
    array_path TEXT NOT NULL UNIQUE,
    state      TEXT NOT NULL DEFAULT 'assigned'
        CHECK (state IN ('assigned','degraded','missing')),
    PRIMARY KEY (tier_name, slot),
    FOREIGN KEY (tier_name) REFERENCES tier_instances(name) ON DELETE CASCADE
);

CREATE TABLE mdadm_arrays (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE
);

CREATE TABLE tier_pools (
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

CREATE TABLE tiers (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    pool_id            INTEGER NOT NULL,
    name               TEXT    NOT NULL,
    rank               INTEGER NOT NULL,
    state              TEXT    NOT NULL CHECK (state IN ('empty','assigned','degraded','missing')),
    array_id           INTEGER UNIQUE,
    pv_device          TEXT,
    target_fill_pct    INTEGER NOT NULL DEFAULT 50,
    full_threshold_pct INTEGER NOT NULL DEFAULT 95,
    backing_kind       TEXT    NOT NULL DEFAULT 'mdadm'
        CHECK (backing_kind IN ('mdadm','zfs','btrfs','bcachefs')),
    backing_ref        TEXT,
    FOREIGN KEY (pool_id)  REFERENCES tier_pools(id) ON DELETE CASCADE,
    FOREIGN KEY (array_id) REFERENCES mdadm_arrays(id),
    UNIQUE (pool_id, name),
    UNIQUE (pool_id, rank),
    CHECK (
        (state = 'empty'  AND backing_ref IS NULL AND array_id IS NULL) OR
        (state != 'empty' AND backing_ref IS NOT NULL)
    )
);

-- ---- managed volumes (per-tier LV tracking) ----

CREATE TABLE managed_volumes (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    pool_name          TEXT    NOT NULL,
    vg_name            TEXT    NOT NULL,
    lv_name            TEXT    NOT NULL,
    mount_point        TEXT    NOT NULL,
    filesystem         TEXT    NOT NULL DEFAULT 'xfs',
    size_bytes         INTEGER NOT NULL DEFAULT 0,
    pinned             INTEGER NOT NULL DEFAULT 0,
    status             TEXT    NOT NULL DEFAULT 'healthy',
    created_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (pool_name) REFERENCES tier_pools(name) ON DELETE CASCADE,
    UNIQUE (vg_name, lv_name)
);

-- ---- heat-engine policy (single-row) ----

CREATE TABLE tier_policy_config (
    id                                  INTEGER PRIMARY KEY CHECK (id = 1),
    poll_interval_minutes               INTEGER NOT NULL DEFAULT 5,
    rolling_window_hours                INTEGER NOT NULL DEFAULT 24,
    evaluation_interval_minutes         INTEGER NOT NULL DEFAULT 15,
    consecutive_cycles_before_migration INTEGER NOT NULL DEFAULT 3,
    migration_reserve_pct               INTEGER NOT NULL DEFAULT 10,
    migration_iops_cap_mb               INTEGER NOT NULL DEFAULT 100,
    migration_io_high_water_pct         INTEGER NOT NULL DEFAULT 80
);

-- ---- unified tiering control plane ----

CREATE TABLE placement_domains (
    id           TEXT PRIMARY KEY,
    backend_kind TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE tier_targets (
    id                 TEXT    PRIMARY KEY,
    name               TEXT    NOT NULL,
    placement_domain   TEXT    NOT NULL,
    backend_kind       TEXT    NOT NULL,
    rank               INTEGER NOT NULL DEFAULT 1,
    target_fill_pct    INTEGER NOT NULL DEFAULT 50,
    full_threshold_pct INTEGER NOT NULL DEFAULT 95,
    policy_revision    INTEGER NOT NULL DEFAULT 1,
    health             TEXT    NOT NULL DEFAULT 'unknown',
    activity_band      TEXT    NOT NULL DEFAULT '',
    activity_trend     TEXT    NOT NULL DEFAULT '',
    capabilities_json  TEXT    NOT NULL DEFAULT '{}',
    backing_ref        TEXT    NOT NULL DEFAULT '',
    created_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (placement_domain) REFERENCES placement_domains(id)
);

CREATE TABLE managed_namespaces (
    id                     TEXT    PRIMARY KEY,
    name                   TEXT    NOT NULL,
    placement_domain       TEXT    NOT NULL,
    backend_kind           TEXT    NOT NULL,
    namespace_kind         TEXT    NOT NULL DEFAULT 'volume',
    exposed_path           TEXT    NOT NULL DEFAULT '',
    pin_state              TEXT    NOT NULL DEFAULT 'none',
    intent_revision        INTEGER NOT NULL DEFAULT 1,
    health                 TEXT    NOT NULL DEFAULT 'unknown',
    placement_state        TEXT    NOT NULL DEFAULT 'unknown',
    backend_ref            TEXT    NOT NULL DEFAULT '',
    capacity_bytes         INTEGER NOT NULL DEFAULT 0,
    used_bytes             INTEGER NOT NULL DEFAULT 0,
    policy_target_ids_json TEXT    NOT NULL DEFAULT '[]',
    created_at             TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at             TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE placement_intents (
    id                  TEXT    PRIMARY KEY,
    namespace_id        TEXT    NOT NULL,
    object_id           TEXT,
    intended_target_id  TEXT    NOT NULL,
    placement_domain    TEXT    NOT NULL,
    policy_revision     INTEGER NOT NULL DEFAULT 1,
    intent_revision     INTEGER NOT NULL DEFAULT 1,
    reason              TEXT    NOT NULL DEFAULT '',
    state               TEXT    NOT NULL DEFAULT 'pending',
    updated_at          TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE degraded_states (
    id           TEXT PRIMARY KEY,
    backend_kind TEXT NOT NULL,
    scope_kind   TEXT NOT NULL DEFAULT '',
    scope_id     TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL DEFAULT 'warning' CHECK (severity IN ('warning','critical')),
    code         TEXT NOT NULL DEFAULT '',
    message      TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at  TEXT
);

CREATE TABLE tiering_reconcile_state (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    last_reconciled_at TEXT
);

CREATE TABLE control_plane_config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO control_plane_config (key, value) VALUES
    ('movement_queue_depth_warn',           '50'),
    ('movement_queue_age_minutes',          '30'),
    ('movement_failed_rate_window_minutes', '60'),
    ('movement_failed_rate_max',            '10'),
    ('reconciliation_max_age_minutes',      '60'),
    ('planner_interval_minutes',            '15'),
    ('reconcile_debounce_seconds',          '60'),
    ('migration_io_high_water_pct',         '80'),
    ('recall_timeout_seconds',              '300'),
    ('movement_worker_concurrency',         '4');

-- ---- ZFS managed backend ----

CREATE TABLE zfs_managed_targets (
    tier_target_id TEXT NOT NULL PRIMARY KEY,
    pool_name      TEXT NOT NULL,
    dataset_name   TEXT NOT NULL,
    dataset_path   TEXT NOT NULL,
    fuse_mode      TEXT NOT NULL DEFAULT 'unknown'
        CHECK (fuse_mode IN ('passthrough','fallback','unknown')),
    FOREIGN KEY (tier_target_id) REFERENCES tier_targets(id) ON DELETE CASCADE
);

CREATE TABLE zfs_managed_namespaces (
    namespace_id                     TEXT    NOT NULL PRIMARY KEY,
    pool_name                        TEXT    NOT NULL,
    meta_dataset                     TEXT    NOT NULL,
    socket_path                      TEXT    NOT NULL,
    mount_path                       TEXT    NOT NULL,
    daemon_pid                       INTEGER,
    daemon_state                     TEXT    NOT NULL DEFAULT 'stopped'
        CHECK (daemon_state IN ('stopped','starting','running','crashed')),
    fuse_mode                        TEXT    NOT NULL DEFAULT 'unknown'
        CHECK (fuse_mode IN ('passthrough','fallback','unknown')),
    snapshot_mode                    TEXT    NOT NULL DEFAULT 'none',
    snapshot_pool_name               TEXT    NOT NULL DEFAULT '',
    snapshot_quiesce_timeout_seconds INTEGER NOT NULL DEFAULT 30,
    FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
);

CREATE TABLE zfs_managed_namespace_snapshots (
    id                     TEXT NOT NULL PRIMARY KEY,
    namespace_id           TEXT NOT NULL,
    pool_name              TEXT NOT NULL,
    zfs_snapshot_name      TEXT NOT NULL,
    backing_snapshots_json TEXT NOT NULL DEFAULT '[]',
    meta_snapshot_json     TEXT NOT NULL DEFAULT '{}',
    created_at             TEXT NOT NULL,
    consistency            TEXT NOT NULL DEFAULT 'atomic',
    FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
);

CREATE INDEX idx_zfs_ns_snapshots_ns_created
    ON zfs_managed_namespace_snapshots(namespace_id, created_at DESC);

CREATE TABLE zfs_movement_log (
    id               TEXT PRIMARY KEY,
    object_id        TEXT NOT NULL,
    namespace_id     TEXT NOT NULL,
    source_target_id TEXT NOT NULL,
    dest_target_id   TEXT NOT NULL,
    object_key       TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL
        CHECK (state IN ('copy_in_progress','copy_complete','switched','cleanup_complete','failed')),
    failure_reason   TEXT NOT NULL DEFAULT '',
    started_at       INTEGER NOT NULL DEFAULT 0,
    updated_at       INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
);

-- ---- mdadm managed backend ----

CREATE TABLE mdadm_managed_targets (
    tier_target_id TEXT PRIMARY KEY REFERENCES tier_targets(id) ON DELETE CASCADE,
    pool_name      TEXT NOT NULL,
    tier_name      TEXT NOT NULL,
    vg_name        TEXT NOT NULL,
    lv_name        TEXT NOT NULL DEFAULT 'data',
    mount_path     TEXT NOT NULL,
    UNIQUE (pool_name, tier_name)
);

CREATE TABLE mdadm_managed_namespaces (
    namespace_id  TEXT PRIMARY KEY REFERENCES managed_namespaces(id) ON DELETE CASCADE,
    pool_name     TEXT NOT NULL UNIQUE,
    socket_path   TEXT NOT NULL DEFAULT '',
    mount_path    TEXT NOT NULL,
    daemon_pid    INTEGER NOT NULL DEFAULT 0,
    daemon_state  TEXT NOT NULL DEFAULT 'stopped'
);

CREATE TABLE mdadm_movement_log (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    object_id        TEXT NOT NULL,
    namespace_id     TEXT NOT NULL,
    source_target_id TEXT NOT NULL,
    dest_target_id   TEXT NOT NULL,
    object_key       TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'copy_in_progress',
    failure_reason   TEXT,
    started_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ---- sharing (SMB / NFS / iSCSI) ----

CREATE TABLE smb_shares (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    path        TEXT    NOT NULL,
    read_only   INTEGER NOT NULL DEFAULT 0,
    guest_ok    INTEGER NOT NULL DEFAULT 0,
    allow_users TEXT    NOT NULL DEFAULT '',
    comment     TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE nfs_exports (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT    NOT NULL,
    networks    TEXT    NOT NULL DEFAULT '',
    sync_mode   INTEGER NOT NULL DEFAULT 0,
    root_squash INTEGER NOT NULL DEFAULT 1,
    read_only   INTEGER NOT NULL DEFAULT 0,
    nfsv3       INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE iscsi_targets (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    iqn          TEXT    NOT NULL UNIQUE,
    block_device TEXT    NOT NULL,
    chap_user    TEXT    NOT NULL DEFAULT '',
    chap_pass    TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- ---- backups ----

CREATE TABLE backup_configs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    target_type TEXT    NOT NULL CHECK (target_type IN ('nfs','smb')),
    host        TEXT    NOT NULL,
    share       TEXT    NOT NULL,
    smb_user    TEXT    NOT NULL DEFAULT '',
    smb_pass    TEXT    NOT NULL DEFAULT '',
    ssh_user    TEXT    NOT NULL DEFAULT '',
    ssh_pass    TEXT    NOT NULL DEFAULT '',
    local_path  TEXT    NOT NULL,
    remote_path TEXT    NOT NULL DEFAULT '',
    direction   TEXT    NOT NULL CHECK (direction IN ('push','pull')),
    method      TEXT    NOT NULL CHECK (method IN ('cp','rsync')),
    parallelism INTEGER NOT NULL DEFAULT 1,
    use_ssh     INTEGER NOT NULL DEFAULT 1,
    compress    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE backup_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    config_id    INTEGER NOT NULL REFERENCES backup_configs(id) ON DELETE CASCADE,
    status       TEXT    NOT NULL DEFAULT 'running' CHECK (status IN ('running','completed','failed')),
    progress     TEXT    NOT NULL DEFAULT '',
    files_done   INTEGER NOT NULL DEFAULT 0,
    files_total  INTEGER NOT NULL DEFAULT -1,
    progress_pct INTEGER NOT NULL DEFAULT -1,
    error        TEXT    NOT NULL DEFAULT '',
    summary      TEXT    NOT NULL DEFAULT '',
    started_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    completed_at TEXT    NOT NULL DEFAULT ''
);

-- ---- SMART monitoring ----

CREATE TABLE smart_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_path TEXT NOT NULL,
    timestamp   TEXT NOT NULL,
    attr_id     INTEGER NOT NULL,
    attr_name   TEXT NOT NULL,
    current_val INTEGER NOT NULL,
    raw_value   INTEGER NOT NULL
);

CREATE INDEX idx_smart_history_device_time
    ON smart_history(device_path, timestamp);

CREATE TABLE smart_alarm_rules (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    attr_id        INTEGER NOT NULL,
    attr_name      TEXT    NOT NULL,
    warning_above  INTEGER,
    critical_above INTEGER,
    warning_below  INTEGER,
    critical_below INTEGER,
    device_path    TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE smart_alarm_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id     INTEGER NOT NULL,
    device_path TEXT    NOT NULL,
    attr_id     INTEGER NOT NULL,
    attr_name   TEXT    NOT NULL,
    severity    TEXT    NOT NULL,
    value       INTEGER NOT NULL,
    threshold   INTEGER NOT NULL,
    timestamp   TEXT    NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- This is the baseline. There is no meaningful "down" — rolling back
-- past the baseline means dropping the whole database. Goose still
-- requires a Down section, so we drop in reverse FK order to make
-- "goose down" work for development scratch databases.

DROP INDEX IF EXISTS idx_smart_history_device_time;
DROP TABLE IF EXISTS smart_alarm_events;
DROP TABLE IF EXISTS smart_alarm_rules;
DROP TABLE IF EXISTS smart_history;
DROP TABLE IF EXISTS backup_runs;
DROP TABLE IF EXISTS backup_configs;
DROP TABLE IF EXISTS iscsi_targets;
DROP TABLE IF EXISTS nfs_exports;
DROP TABLE IF EXISTS smb_shares;
DROP TABLE IF EXISTS mdadm_movement_log;
DROP TABLE IF EXISTS mdadm_managed_namespaces;
DROP TABLE IF EXISTS mdadm_managed_targets;
DROP TABLE IF EXISTS zfs_movement_log;
DROP INDEX IF EXISTS idx_zfs_ns_snapshots_ns_created;
DROP TABLE IF EXISTS zfs_managed_namespace_snapshots;
DROP TABLE IF EXISTS zfs_managed_namespaces;
DROP TABLE IF EXISTS zfs_managed_targets;
DROP TABLE IF EXISTS control_plane_config;
DROP TABLE IF EXISTS tiering_reconcile_state;
DROP TABLE IF EXISTS degraded_states;
DROP TABLE IF EXISTS placement_intents;
DROP TABLE IF EXISTS managed_namespaces;
DROP TABLE IF EXISTS tier_targets;
DROP TABLE IF EXISTS placement_domains;
DROP TABLE IF EXISTS tier_policy_config;
DROP TABLE IF EXISTS managed_volumes;
DROP TABLE IF EXISTS tiers;
DROP TABLE IF EXISTS tier_pools;
DROP TABLE IF EXISTS mdadm_arrays;
DROP TABLE IF EXISTS tier_instance_arrays;
DROP TABLE IF EXISTS tier_instances;
DROP TABLE IF EXISTS config;
DROP INDEX IF EXISTS idx_login_attempts_ip;
DROP TABLE IF EXISTS login_attempts;
DROP TABLE IF EXISTS sessions;

-- +goose StatementEnd
