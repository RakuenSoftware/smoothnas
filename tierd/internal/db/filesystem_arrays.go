package db

import (
	"database/sql"
	"fmt"
)

// FilesystemArrayRow records a btrfs or bcachefs array created from raw disks.
type FilesystemArrayRow struct {
	ID              int64
	Name            string
	Kind            string
	Label           string
	MountPath       string
	DataProfile     string
	MetadataProfile string
	Replicas        int
	State           string
	SizeBytes       uint64
	ErrorReason     string
	CreatedAt       string
	UpdatedAt       string
	Devices         []FilesystemArrayDeviceRow
}

// FilesystemArrayDeviceRow is one raw disk assigned to a filesystem array.
type FilesystemArrayDeviceRow struct {
	ID         int64
	ArrayID    int64
	DevicePath string
	SizeBytes  uint64
	State      string
	CreatedAt  string
	UpdatedAt  string
}

// CreateFilesystemArray persists a newly-created btrfs/bcachefs array.
func (s *Store) CreateFilesystemArray(row *FilesystemArrayRow, devices []FilesystemArrayDeviceRow) (*FilesystemArrayRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin filesystem array create: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := nowUTC()
	res, err := tx.Exec(`INSERT INTO filesystem_arrays
		(name, kind, label, mount_path, data_profile, metadata_profile, replicas,
		 state, size_bytes, error_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Name, row.Kind, row.Label, row.MountPath, row.DataProfile, row.MetadataProfile,
		row.Replicas, row.State, row.SizeBytes, row.ErrorReason, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert filesystem array: %w", err)
	}
	arrayID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("filesystem array id: %w", err)
	}
	for _, dev := range devices {
		if _, err := tx.Exec(`INSERT INTO filesystem_array_devices
			(array_id, device_path, size_bytes, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			arrayID, dev.DevicePath, dev.SizeBytes, dev.State, now, now); err != nil {
			return nil, fmt.Errorf("insert filesystem array device %s: %w", dev.DevicePath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit filesystem array create: %w", err)
	}
	return s.GetFilesystemArray(row.Name)
}

// ListFilesystemArrays returns every btrfs/bcachefs array with member devices.
func (s *Store) ListFilesystemArrays() ([]FilesystemArrayRow, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, label, mount_path,
	       data_profile, metadata_profile, replicas, state, size_bytes,
	       error_reason, created_at, updated_at
		FROM filesystem_arrays ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("list filesystem arrays: %w", err)
	}
	defer rows.Close()

	var out []FilesystemArrayRow
	for rows.Next() {
		var row FilesystemArrayRow
		if err := scanFilesystemArray(rows, &row); err != nil {
			return nil, err
		}
		devices, err := s.ListFilesystemArrayDevices(row.Name)
		if err != nil {
			return nil, err
		}
		row.Devices = devices
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetFilesystemArray returns a single btrfs/bcachefs array by name.
func (s *Store) GetFilesystemArray(name string) (*FilesystemArrayRow, error) {
	var row FilesystemArrayRow
	err := s.db.QueryRow(`SELECT id, name, kind, label, mount_path,
	       data_profile, metadata_profile, replicas, state, size_bytes,
	       error_reason, created_at, updated_at
		FROM filesystem_arrays WHERE name = ?`, name).
		Scan(&row.ID, &row.Name, &row.Kind, &row.Label, &row.MountPath,
			&row.DataProfile, &row.MetadataProfile, &row.Replicas, &row.State,
			&row.SizeBytes, &row.ErrorReason, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get filesystem array %q: %w", name, err)
	}
	devices, err := s.ListFilesystemArrayDevices(name)
	if err != nil {
		return nil, err
	}
	row.Devices = devices
	return &row, nil
}

// ListFilesystemArrayDevices returns devices for an array name. Empty name returns all.
func (s *Store) ListFilesystemArrayDevices(name string) ([]FilesystemArrayDeviceRow, error) {
	query := `SELECT d.id, d.array_id, d.device_path, d.size_bytes, d.state, d.created_at, d.updated_at
		FROM filesystem_array_devices d
		JOIN filesystem_arrays a ON a.id = d.array_id`
	args := []any{}
	if name != "" {
		query += ` WHERE a.name = ?`
		args = append(args, name)
	}
	query += ` ORDER BY a.kind, a.name, d.device_path`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list filesystem array devices: %w", err)
	}
	defer rows.Close()

	var out []FilesystemArrayDeviceRow
	for rows.Next() {
		var dev FilesystemArrayDeviceRow
		if err := rows.Scan(&dev.ID, &dev.ArrayID, &dev.DevicePath, &dev.SizeBytes,
			&dev.State, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan filesystem array device: %w", err)
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// SetFilesystemArrayState records runtime state and an optional error reason.
func (s *Store) SetFilesystemArrayState(name, state, reason string) error {
	res, err := s.db.Exec(`UPDATE filesystem_arrays
		SET state = ?, error_reason = ?, updated_at = ?
		WHERE name = ?`, state, reason, nowUTC(), name)
	if err != nil {
		return fmt.Errorf("set filesystem array state %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set filesystem array state rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFilesystemArray removes a btrfs/bcachefs array record.
func (s *Store) DeleteFilesystemArray(name string) error {
	res, err := s.db.Exec(`DELETE FROM filesystem_arrays WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete filesystem array %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete filesystem array rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanFilesystemArray(rows *sql.Rows, row *FilesystemArrayRow) error {
	if err := rows.Scan(&row.ID, &row.Name, &row.Kind, &row.Label, &row.MountPath,
		&row.DataProfile, &row.MetadataProfile, &row.Replicas, &row.State,
		&row.SizeBytes, &row.ErrorReason, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return fmt.Errorf("scan filesystem array: %w", err)
	}
	return nil
}
