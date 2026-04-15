package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BackupRun records a single execution of a backup config.
type BackupRun struct {
	ID          int64  `json:"id"`
	ConfigID    int64  `json:"config_id"`
	Status      string `json:"status"`       // "running", "completed", "failed"
	Progress    string `json:"progress"`
	FilesDone   int    `json:"files_done"`
	FilesTotal  int    `json:"files_total"`
	ProgressPct int    `json:"progress_pct"` // 0-100, or -1 for indeterminate
	Error       string `json:"error,omitempty"`
	Summary     string `json:"summary,omitempty"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// MigrateBackupRuns creates the backup_runs table if it does not exist.
func (s *Store) MigrateBackupRuns() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS backup_runs (
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
	)`)
	if err != nil {
		return fmt.Errorf("migrate backup_runs: %w", err)
	}
	return nil
}

// CreateBackupRun inserts a new "running" backup run row and returns its ID.
func (s *Store) CreateBackupRun(configID int64) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO backup_runs (config_id, status, started_at) VALUES (?, 'running', ?)`,
		configID, now,
	)
	if err != nil {
		return 0, fmt.Errorf("create backup run: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UpdateBackupRunProgress updates the progress message and file counts on a running backup run.
// done and total may be -1 if unknown (e.g. rsync). progress_pct is computed automatically.
func (s *Store) UpdateBackupRunProgress(id int64, progress string, done, total int) error {
	pct := -1
	if total > 0 && done >= 0 {
		pct = done * 100 / total
		if pct > 100 {
			pct = 100
		}
	}
	_, err := s.db.Exec(
		`UPDATE backup_runs SET progress=?, files_done=?, files_total=?, progress_pct=? WHERE id=?`,
		progress, done, total, pct, id,
	)
	if err != nil {
		return fmt.Errorf("update backup run progress: %w", err)
	}
	return nil
}

// CompleteBackupRun marks a backup run as successfully completed.
func (s *Store) CompleteBackupRun(id int64, summary string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`UPDATE backup_runs SET status='completed', summary=?, progress='', progress_pct=100, completed_at=? WHERE id=?`,
		summary, now, id,
	)
	if err != nil {
		return fmt.Errorf("complete backup run: %w", err)
	}
	return nil
}

// FailBackupRun marks a backup run as failed.
func (s *Store) FailBackupRun(id int64, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`UPDATE backup_runs SET status='failed', error=?, progress='', completed_at=? WHERE id=?`,
		errMsg, now, id,
	)
	if err != nil {
		return fmt.Errorf("fail backup run: %w", err)
	}
	return nil
}

// GetBackupRun retrieves a single backup run by ID.
func (s *Store) GetBackupRun(id int64) (*BackupRun, error) {
	var r BackupRun
	err := s.db.QueryRow(
		`SELECT id, config_id, status, progress, files_done, files_total, progress_pct,
		        error, summary, started_at, completed_at
		 FROM backup_runs WHERE id = ?`, id,
	).Scan(
		&r.ID, &r.ConfigID, &r.Status, &r.Progress,
		&r.FilesDone, &r.FilesTotal, &r.ProgressPct,
		&r.Error, &r.Summary, &r.StartedAt, &r.CompletedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ListBackupRunsByConfig returns backup runs for a given config, newest first.
// If activeOnly is true, only "running" runs are returned.
func (s *Store) ListBackupRunsByConfig(configID int64, activeOnly bool) ([]BackupRun, error) {
	query := `SELECT id, config_id, status, progress, files_done, files_total, progress_pct,
	                 error, summary, started_at, completed_at
	          FROM backup_runs WHERE config_id = ?`
	if activeOnly {
		query += ` AND status = 'running'`
	}
	query += ` ORDER BY id DESC LIMIT 20`

	rows, err := s.db.Query(query, configID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []BackupRun
	for rows.Next() {
		var r BackupRun
		if err := rows.Scan(
			&r.ID, &r.ConfigID, &r.Status, &r.Progress,
			&r.FilesDone, &r.FilesTotal, &r.ProgressPct,
			&r.Error, &r.Summary, &r.StartedAt, &r.CompletedAt,
		); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// ListActiveBackupRuns returns all currently running backup runs across all configs.
func (s *Store) ListActiveBackupRuns() ([]BackupRun, error) {
	rows, err := s.db.Query(
		`SELECT id, config_id, status, progress, files_done, files_total, progress_pct,
		        error, summary, started_at, completed_at
		 FROM backup_runs WHERE status = 'running' ORDER BY id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []BackupRun
	for rows.Next() {
		var r BackupRun
		if err := rows.Scan(
			&r.ID, &r.ConfigID, &r.Status, &r.Progress,
			&r.FilesDone, &r.FilesTotal, &r.ProgressPct,
			&r.Error, &r.Summary, &r.StartedAt, &r.CompletedAt,
		); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// MarkStaleRunsFailed marks any "running" backup runs as failed — called on startup
// to clean up runs that were interrupted by a tierd restart.
func (s *Store) MarkStaleRunsFailed() error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`UPDATE backup_runs SET status='failed', error='tierd restarted', completed_at=?
		 WHERE status='running'`,
		now,
	)
	if err != nil {
		return fmt.Errorf("mark stale backup runs failed: %w", err)
	}
	return nil
}
