-- +goose Up
CREATE TABLE backup_google_accounts (
 id INTEGER PRIMARY KEY, label TEXT NOT NULL UNIQUE, email TEXT NOT NULL DEFAULT '',
 client_id TEXT NOT NULL, client_secret TEXT NOT NULL, refresh_token TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE TABLE backup_configs_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    target_type TEXT    NOT NULL CHECK (target_type IN ('nfs','smb','gdrive')),
    host        TEXT    NOT NULL,
    share       TEXT    NOT NULL,
    smb_user    TEXT    NOT NULL DEFAULT '',
    smb_pass    TEXT    NOT NULL DEFAULT '',
    ssh_user    TEXT    NOT NULL DEFAULT '',
    ssh_pass    TEXT    NOT NULL DEFAULT '',
    local_path  TEXT    NOT NULL,
    remote_path TEXT    NOT NULL DEFAULT '',
    direction   TEXT    NOT NULL CHECK (direction IN ('push','pull')),
    method      TEXT    NOT NULL CHECK (method IN ('cp','rsync','rclone')),
    parallelism INTEGER NOT NULL DEFAULT 1,
    use_ssh     INTEGER NOT NULL DEFAULT 1,
    compress    INTEGER NOT NULL DEFAULT 0,
    delete_mode INTEGER NOT NULL DEFAULT 0,
    schedule_enabled INTEGER NOT NULL DEFAULT 0,
    schedule_time TEXT NOT NULL DEFAULT '',
    schedule_days INTEGER NOT NULL DEFAULT 127,
    last_scheduled_at TEXT NOT NULL DEFAULT '',
    google_account_id INTEGER REFERENCES backup_google_accounts(id),
    excludes TEXT NOT NULL DEFAULT '',
    encrypt INTEGER NOT NULL DEFAULT 0,
    crypt_password TEXT NOT NULL DEFAULT '',
    crypt_salt TEXT NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE backup_runs_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    config_id    INTEGER NOT NULL REFERENCES backup_configs_new(id) ON DELETE CASCADE,
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

INSERT INTO backup_configs_new (id,name,target_type,host,share,smb_user,smb_pass,ssh_user,ssh_pass,local_path,remote_path,direction,method,parallelism,use_ssh,compress,created_at,delete_mode,schedule_enabled,schedule_time,schedule_days,last_scheduled_at) SELECT id,name,target_type,host,share,smb_user,smb_pass,ssh_user,ssh_pass,local_path,remote_path,direction,method,parallelism,use_ssh,compress,created_at,delete_mode,schedule_enabled,schedule_time,schedule_days,last_scheduled_at FROM backup_configs;
INSERT INTO backup_runs_new SELECT * FROM backup_runs;
DROP TABLE backup_runs;
DROP TABLE backup_configs;
ALTER TABLE backup_configs_new RENAME TO backup_configs;
ALTER TABLE backup_runs_new RENAME TO backup_runs;

-- +goose Down
-- Cloud configs and recovery keys must survive a binary rollback.
SELECT 1;
