package db

import (
	"database/sql"
	"errors"
	"time"
)

// BackupConfig stores a named backup job configuration.
type BackupConfig struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	TargetType  string `json:"target_type"` // "nfs" or "smb" (used by method=cp; ignored for method=rsync)
	Host        string `json:"host"`
	Share       string `json:"share"`
	SMBUser     string `json:"smb_user"`
	SMBPass     string `json:"-"` // never exposed in JSON
	HasCreds    bool   `json:"has_creds"`
	SSHUser     string `json:"ssh_user"`
	SSHPass     string `json:"-"` // never exposed in JSON
	HasSSHCreds bool   `json:"has_ssh_creds"`
	LocalPath   string `json:"local_path"`
	RemotePath  string `json:"remote_path"` // subdirectory on share/remote path, may be empty
	Direction   string `json:"direction"`   // "push" or "pull"
	Method      string `json:"method"`      // "cp" or "rsync"
	Parallelism int    `json:"parallelism"` // retained for cp method; rsync always uses 1 stream
	UseSSH      bool   `json:"use_ssh"`     // rsync transport: true=direct SSH, false=mount NFS/SMB and rsync locally (method=="rsync" only)
	Compress    bool   `json:"compress"`    // rsync --compress when method=="rsync"
	DeleteMode  bool   `json:"delete_mode"` // rsync --delete when method=="rsync"
	CreatedAt   string `json:"created_at"`
}

func (s *Store) CreateBackupConfig(cfg BackupConfig) (*BackupConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if cfg.Parallelism < 1 {
		cfg.Parallelism = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO backup_configs
		 (name, target_type, host, share, smb_user, smb_pass, ssh_user, ssh_pass, local_path, remote_path, direction, method, parallelism, use_ssh, compress, delete_mode, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		cfg.Name, cfg.TargetType, cfg.Host, cfg.Share,
		cfg.SMBUser, cfg.SMBPass, cfg.SSHUser, cfg.SSHPass,
		cfg.LocalPath, cfg.RemotePath,
		cfg.Direction, cfg.Method, cfg.Parallelism,
		boolToInt(cfg.UseSSH), boolToInt(cfg.Compress), boolToInt(cfg.DeleteMode), now,
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
	cfg.HasSSHCreds = cfg.SSHUser != "" || cfg.SSHPass != ""
	cfg.SMBPass = ""
	cfg.SSHPass = ""
	return &cfg, nil
}

func (s *Store) ListBackupConfigs() ([]BackupConfig, error) {
	rows, err := s.db.Query(
		`SELECT id, name, target_type, host, share, smb_user, smb_pass, ssh_user, ssh_pass,
		        local_path, remote_path, direction, method, parallelism, use_ssh, compress, delete_mode, created_at
		 FROM backup_configs ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cfgs []BackupConfig
	for rows.Next() {
		var c BackupConfig
		var useSSH, compress, deleteMode int
		if err := rows.Scan(
			&c.ID, &c.Name, &c.TargetType, &c.Host, &c.Share,
			&c.SMBUser, &c.SMBPass, &c.SSHUser, &c.SSHPass,
			&c.LocalPath, &c.RemotePath, &c.Direction, &c.Method, &c.Parallelism,
			&useSSH, &compress, &deleteMode, &c.CreatedAt,
		); err != nil {
			return nil, err
		}
		c.UseSSH = useSSH != 0
		c.Compress = compress != 0
		c.DeleteMode = deleteMode != 0
		if c.Parallelism < 1 {
			c.Parallelism = 1
		}
		c.HasCreds = c.SMBUser != ""
		c.HasSSHCreds = c.SSHUser != "" || c.SSHPass != ""
		c.SMBPass = ""
		c.SSHPass = ""
		cfgs = append(cfgs, c)
	}
	return cfgs, rows.Err()
}

func (s *Store) GetBackupConfig(id int64) (*BackupConfig, error) {
	var c BackupConfig
	var useSSH, compress, deleteMode int
	err := s.db.QueryRow(
		`SELECT id, name, target_type, host, share, smb_user, smb_pass, ssh_user, ssh_pass,
		        local_path, remote_path, direction, method, parallelism, use_ssh, compress, delete_mode, created_at
		 FROM backup_configs WHERE id = ?`, id,
	).Scan(
		&c.ID, &c.Name, &c.TargetType, &c.Host, &c.Share,
		&c.SMBUser, &c.SMBPass, &c.SSHUser, &c.SSHPass,
		&c.LocalPath, &c.RemotePath, &c.Direction, &c.Method, &c.Parallelism,
		&useSSH, &compress, &deleteMode, &c.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.UseSSH = useSSH != 0
	c.Compress = compress != 0
	c.DeleteMode = deleteMode != 0
	if c.Parallelism < 1 {
		c.Parallelism = 1
	}
	c.HasCreds = c.SMBUser != ""
	c.HasSSHCreds = c.SSHUser != "" || c.SSHPass != ""
	return &c, nil
}

// UpdateBackupConfig replaces the row's mutable fields. CreatedAt and ID
// are preserved. Returns ErrNotFound if the row does not exist and
// ErrDuplicate if Name collides with another row.
func (s *Store) UpdateBackupConfig(id int64, cfg BackupConfig) (*BackupConfig, error) {
	if cfg.Parallelism < 1 {
		cfg.Parallelism = 1
	}
	res, err := s.db.Exec(
		`UPDATE backup_configs
		 SET name=?, target_type=?, host=?, share=?,
		     smb_user=?, smb_pass=?, ssh_user=?, ssh_pass=?,
		     local_path=?, remote_path=?, direction=?, method=?, parallelism=?,
		     use_ssh=?, compress=?, delete_mode=?
		 WHERE id=?`,
		cfg.Name, cfg.TargetType, cfg.Host, cfg.Share,
		cfg.SMBUser, cfg.SMBPass, cfg.SSHUser, cfg.SSHPass,
		cfg.LocalPath, cfg.RemotePath,
		cfg.Direction, cfg.Method, cfg.Parallelism,
		boolToInt(cfg.UseSSH), boolToInt(cfg.Compress), boolToInt(cfg.DeleteMode),
		id,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetBackupConfig(id)
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
