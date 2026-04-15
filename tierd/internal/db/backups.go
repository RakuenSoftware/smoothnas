package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BackupConfig stores a named backup job configuration.
type BackupConfig struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	TargetType  string `json:"target_type"` // "nfs" or "smb"
	Host        string `json:"host"`
	Share       string `json:"share"`
	SMBUser     string `json:"smb_user"`
	SMBPass     string `json:"-"` // never exposed in JSON
	HasCreds    bool   `json:"has_creds"`
	LocalPath   string `json:"local_path"`
	RemotePath  string `json:"remote_path"` // subdirectory on share, may be empty
	Direction   string `json:"direction"`   // "push" or "pull"
	Method      string `json:"method"`      // "cp" or "rsync"
	Parallelism int    `json:"parallelism"` // 1 = single rsync; >1 fans out top-level dirs across N concurrent rsyncs
	CreatedAt   string `json:"created_at"`
}

// MigrateBackups creates the backup_configs table if it does not exist
// and adds the parallelism column to existing schemas.
func (s *Store) MigrateBackups() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS backup_configs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT NOT NULL UNIQUE,
		target_type TEXT NOT NULL CHECK (target_type IN ('nfs','smb')),
		host        TEXT NOT NULL,
		share       TEXT NOT NULL,
		smb_user    TEXT NOT NULL DEFAULT '',
		smb_pass    TEXT NOT NULL DEFAULT '',
		local_path  TEXT NOT NULL,
		remote_path TEXT NOT NULL DEFAULT '',
		direction   TEXT NOT NULL CHECK (direction IN ('push','pull')),
		method      TEXT NOT NULL CHECK (method IN ('cp','rsync')),
		parallelism INTEGER NOT NULL DEFAULT 1,
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("migrate backup_configs: %w", err)
	}
	if err := s.ensureColumn("backup_configs", "parallelism",
		`ALTER TABLE backup_configs ADD COLUMN parallelism INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateBackupConfig(cfg BackupConfig) (*BackupConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if cfg.Parallelism < 1 {
		cfg.Parallelism = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO backup_configs
		 (name, target_type, host, share, smb_user, smb_pass, local_path, remote_path, direction, method, parallelism, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		cfg.Name, cfg.TargetType, cfg.Host, cfg.Share,
		cfg.SMBUser, cfg.SMBPass,
		cfg.LocalPath, cfg.RemotePath,
		cfg.Direction, cfg.Method, cfg.Parallelism, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	cfg.ID, _ = res.LastInsertId()
	cfg.CreatedAt = now
	cfg.HasCreds = cfg.SMBUser != ""
	cfg.SMBPass = ""
	return &cfg, nil
}

func (s *Store) ListBackupConfigs() ([]BackupConfig, error) {
	rows, err := s.db.Query(
		`SELECT id, name, target_type, host, share, smb_user, smb_pass,
		        local_path, remote_path, direction, method, parallelism, created_at
		 FROM backup_configs ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cfgs []BackupConfig
	for rows.Next() {
		var c BackupConfig
		if err := rows.Scan(
			&c.ID, &c.Name, &c.TargetType, &c.Host, &c.Share,
			&c.SMBUser, &c.SMBPass,
			&c.LocalPath, &c.RemotePath, &c.Direction, &c.Method, &c.Parallelism, &c.CreatedAt,
		); err != nil {
			return nil, err
		}
		if c.Parallelism < 1 {
			c.Parallelism = 1
		}
		c.HasCreds = c.SMBUser != ""
		c.SMBPass = ""
		cfgs = append(cfgs, c)
	}
	return cfgs, rows.Err()
}

func (s *Store) GetBackupConfig(id int64) (*BackupConfig, error) {
	var c BackupConfig
	err := s.db.QueryRow(
		`SELECT id, name, target_type, host, share, smb_user, smb_pass,
		        local_path, remote_path, direction, method, parallelism, created_at
		 FROM backup_configs WHERE id = ?`, id,
	).Scan(
		&c.ID, &c.Name, &c.TargetType, &c.Host, &c.Share,
		&c.SMBUser, &c.SMBPass,
		&c.LocalPath, &c.RemotePath, &c.Direction, &c.Method, &c.Parallelism, &c.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if c.Parallelism < 1 {
		c.Parallelism = 1
	}
	c.HasCreds = c.SMBUser != ""
	return &c, nil
}

func (s *Store) DeleteBackupConfig(id int64) error {
	res, err := s.db.Exec("DELETE FROM backup_configs WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
