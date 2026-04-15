package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SmbShare represents a persisted SMB share definition.
type SmbShare struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	ReadOnly   bool     `json:"read_only"`
	GuestOK    bool     `json:"guest_ok"`
	AllowUsers string   `json:"allow_users"` // comma-separated
	Comment    string   `json:"comment"`
	CreatedAt  string   `json:"created_at"`
}

// NfsExport represents a persisted NFS export definition.
type NfsExport struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Networks   string `json:"networks"`    // comma-separated
	Sync       bool   `json:"sync"`
	RootSquash bool   `json:"root_squash"`
	ReadOnly   bool   `json:"read_only"`
	NFSv3      bool   `json:"nfsv3"`
	CreatedAt  string `json:"created_at"`
}

// IscsiTarget represents a persisted iSCSI target definition.
type IscsiTarget struct {
	ID          int64  `json:"id"`
	IQN         string `json:"iqn"`
	BlockDevice string `json:"block_device"`
	CHAPUser    string `json:"chap_user"`
	CHAPPass    string `json:"-"`
	HasCHAP     bool   `json:"has_chap"`
	CreatedAt   string `json:"created_at"`
}

// MigrateShares creates the sharing tables if they don't exist.
func (s *Store) MigrateShares() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS smb_shares (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			path        TEXT NOT NULL,
			read_only   INTEGER NOT NULL DEFAULT 0,
			guest_ok    INTEGER NOT NULL DEFAULT 0,
			allow_users TEXT NOT NULL DEFAULT '',
			comment     TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS nfs_exports (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			path        TEXT NOT NULL,
			networks    TEXT NOT NULL DEFAULT '',
			sync_mode   INTEGER NOT NULL DEFAULT 1,
			root_squash INTEGER NOT NULL DEFAULT 1,
			read_only   INTEGER NOT NULL DEFAULT 0,
			nfsv3       INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS iscsi_targets (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			iqn          TEXT NOT NULL UNIQUE,
			block_device TEXT NOT NULL,
			chap_user    TEXT NOT NULL DEFAULT '',
			chap_pass    TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migrate shares: %w", err)
		}
	}
	return nil
}

// --- SMB Shares ---

func (s *Store) CreateSmbShare(share SmbShare) (*SmbShare, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		"INSERT INTO smb_shares (name, path, read_only, guest_ok, allow_users, comment, created_at) VALUES (?,?,?,?,?,?,?)",
		share.Name, share.Path, boolToInt(share.ReadOnly), boolToInt(share.GuestOK), share.AllowUsers, share.Comment, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	share.ID, _ = res.LastInsertId()
	share.CreatedAt = now
	return &share, nil
}

func (s *Store) ListSmbShares() ([]SmbShare, error) {
	rows, err := s.db.Query("SELECT id, name, path, read_only, guest_ok, allow_users, comment, created_at FROM smb_shares ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shares []SmbShare
	for rows.Next() {
		var sh SmbShare
		var ro, guest int
		if err := rows.Scan(&sh.ID, &sh.Name, &sh.Path, &ro, &guest, &sh.AllowUsers, &sh.Comment, &sh.CreatedAt); err != nil {
			return nil, err
		}
		sh.ReadOnly = ro != 0
		sh.GuestOK = guest != 0
		shares = append(shares, sh)
	}
	return shares, rows.Err()
}

func (s *Store) DeleteSmbShare(name string) error {
	res, err := s.db.Exec("DELETE FROM smb_shares WHERE name = ?", name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- NFS Exports ---

func (s *Store) CreateNfsExport(exp NfsExport) (*NfsExport, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		"INSERT INTO nfs_exports (path, networks, sync_mode, root_squash, read_only, nfsv3, created_at) VALUES (?,?,?,?,?,?,?)",
		exp.Path, exp.Networks, boolToInt(exp.Sync), boolToInt(exp.RootSquash), boolToInt(exp.ReadOnly), boolToInt(exp.NFSv3), now,
	)
	if err != nil {
		return nil, err
	}
	exp.ID, _ = res.LastInsertId()
	exp.CreatedAt = now
	return &exp, nil
}

func (s *Store) ListNfsExports() ([]NfsExport, error) {
	rows, err := s.db.Query("SELECT id, path, networks, sync_mode, root_squash, read_only, nfsv3, created_at FROM nfs_exports ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var exports []NfsExport
	for rows.Next() {
		var exp NfsExport
		var syncMode, rootSquash, readOnly, nfsv3 int
		if err := rows.Scan(&exp.ID, &exp.Path, &exp.Networks, &syncMode, &rootSquash, &readOnly, &nfsv3, &exp.CreatedAt); err != nil {
			return nil, err
		}
		exp.Sync = syncMode != 0
		exp.RootSquash = rootSquash != 0
		exp.ReadOnly = readOnly != 0
		exp.NFSv3 = nfsv3 != 0
		exports = append(exports, exp)
	}
	return exports, rows.Err()
}

func (s *Store) DeleteNfsExport(id int64) error {
	res, err := s.db.Exec("DELETE FROM nfs_exports WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- iSCSI Targets ---

func (s *Store) CreateIscsiTarget(target IscsiTarget) (*IscsiTarget, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		"INSERT INTO iscsi_targets (iqn, block_device, chap_user, chap_pass, created_at) VALUES (?,?,?,?,?)",
		target.IQN, target.BlockDevice, target.CHAPUser, target.CHAPPass, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	target.ID, _ = res.LastInsertId()
	target.HasCHAP = target.CHAPUser != ""
	target.CreatedAt = now
	return &target, nil
}

func (s *Store) ListIscsiTargets() ([]IscsiTarget, error) {
	rows, err := s.db.Query("SELECT id, iqn, block_device, chap_user, created_at FROM iscsi_targets ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []IscsiTarget
	for rows.Next() {
		var t IscsiTarget
		if err := rows.Scan(&t.ID, &t.IQN, &t.BlockDevice, &t.CHAPUser, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.HasCHAP = t.CHAPUser != ""
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func (s *Store) DeleteIscsiTarget(iqn string) error {
	res, err := s.db.Exec("DELETE FROM iscsi_targets WHERE iqn = ?", iqn)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- helpers ---

var ErrDuplicate = fmt.Errorf("duplicate")

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return containsCI(s, "UNIQUE") || containsCI(s, "unique")
}

func containsCI(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// Unused import guard.
var _ = sql.ErrNoRows
