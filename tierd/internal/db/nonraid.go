package db

import (
	"database/sql"
	"fmt"
)

// NonRaidArrayRow is the durable control-plane record for one nonRaid array.
type NonRaidArrayRow struct {
	ID             int64
	Name           string
	State          string
	UUID           string
	Filesystem     string
	MountPath      string
	ParityCount    int
	MinParityBytes uint64
	CapacityBytes  uint64
	ErrorReason    string
	CreatedAt      string
	UpdatedAt      string
	Devices        []NonRaidDeviceRow
}

// NonRaidDeviceRow is one data or parity member assigned to a nonRaid array.
type NonRaidDeviceRow struct {
	ID                int64
	ArrayID           int64
	Role              string
	Slot              int
	DevicePath        string
	VirtualDevicePath string
	Serial            string
	SizeBytes         uint64
	UsableBytes       uint64
	MountPath         string
	State             string
	CreatedAt         string
	UpdatedAt         string
}

// CreateNonRaidArray persists a validated nonRaid array layout.
func (s *Store) CreateNonRaidArray(row *NonRaidArrayRow, devices []NonRaidDeviceRow) (*NonRaidArrayRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin nonraid create: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // harmless after Commit

	now := nowUTC()
	res, err := tx.Exec(`INSERT INTO nonraid_arrays
		(name, state, uuid, filesystem, mount_path, parity_count, min_parity_bytes,
		 capacity_bytes, error_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Name, row.State, row.UUID, row.Filesystem, row.MountPath, row.ParityCount,
		row.MinParityBytes, row.CapacityBytes, row.ErrorReason, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert nonraid array: %w", err)
	}
	arrayID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("nonraid array id: %w", err)
	}
	for i := range devices {
		dev := devices[i]
		if _, err := tx.Exec(`INSERT INTO nonraid_array_devices
			(array_id, role, slot, device_path, virtual_device_path, serial, size_bytes, usable_bytes,
			 mount_path, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			arrayID, dev.Role, dev.Slot, dev.DevicePath, dev.VirtualDevicePath, dev.Serial,
			dev.SizeBytes, dev.UsableBytes, dev.MountPath, dev.State, now, now); err != nil {
			return nil, fmt.Errorf("insert nonraid device %s: %w", dev.DevicePath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit nonraid create: %w", err)
	}
	return s.GetNonRaidArray(row.Name)
}

// ListNonRaidArrays returns every nonRaid array with its member devices.
func (s *Store) ListNonRaidArrays() ([]NonRaidArrayRow, error) {
	rows, err := s.db.Query(`SELECT id, name, state, filesystem, mount_path,
	       uuid, parity_count, min_parity_bytes, capacity_bytes, error_reason,
	       created_at, updated_at
		FROM nonraid_arrays ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list nonraid arrays: %w", err)
	}
	defer rows.Close()

	var out []NonRaidArrayRow
	for rows.Next() {
		var row NonRaidArrayRow
		if err := scanNonRaidArray(rows, &row); err != nil {
			return nil, err
		}
		devices, err := s.ListNonRaidDevices(row.Name)
		if err != nil {
			return nil, err
		}
		row.Devices = devices
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetNonRaidArray returns a single nonRaid array by name.
func (s *Store) GetNonRaidArray(name string) (*NonRaidArrayRow, error) {
	var row NonRaidArrayRow
	err := s.db.QueryRow(`SELECT id, name, state, filesystem, mount_path,
	       uuid, parity_count, min_parity_bytes, capacity_bytes, error_reason,
	       created_at, updated_at
		FROM nonraid_arrays WHERE name = ?`, name).
		Scan(&row.ID, &row.Name, &row.State, &row.Filesystem, &row.MountPath,
			&row.UUID, &row.ParityCount, &row.MinParityBytes, &row.CapacityBytes,
			&row.ErrorReason, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get nonraid array %q: %w", name, err)
	}
	devices, err := s.ListNonRaidDevices(name)
	if err != nil {
		return nil, err
	}
	row.Devices = devices
	return &row, nil
}

// ListNonRaidDevices returns devices for an array name. Empty name returns all.
func (s *Store) ListNonRaidDevices(name string) ([]NonRaidDeviceRow, error) {
	query := `SELECT d.id, d.array_id, d.role, d.slot, d.device_path,
	       d.virtual_device_path, d.serial,
	       d.size_bytes, d.usable_bytes, d.mount_path, d.state, d.created_at, d.updated_at
		FROM nonraid_array_devices d
		JOIN nonraid_arrays a ON a.id = d.array_id`
	args := []any{}
	if name != "" {
		query += ` WHERE a.name = ?`
		args = append(args, name)
	}
	query += ` ORDER BY a.name, d.role, d.slot`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list nonraid devices: %w", err)
	}
	defer rows.Close()

	var out []NonRaidDeviceRow
	for rows.Next() {
		var dev NonRaidDeviceRow
		if err := rows.Scan(&dev.ID, &dev.ArrayID, &dev.Role, &dev.Slot,
			&dev.DevicePath, &dev.VirtualDevicePath, &dev.Serial, &dev.SizeBytes, &dev.UsableBytes,
			&dev.MountPath, &dev.State, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan nonraid device: %w", err)
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// SetNonRaidArrayState records runtime state and an optional error reason.
func (s *Store) SetNonRaidArrayState(name, state, reason string) error {
	res, err := s.db.Exec(`UPDATE nonraid_arrays
		SET state = ?, error_reason = ?, updated_at = ?
		WHERE name = ?`, state, reason, nowUTC(), name)
	if err != nil {
		return fmt.Errorf("set nonraid array state %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set nonraid array state rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNonRaidDeviceRuntime records the current virtual data device and state.
func (s *Store) SetNonRaidDeviceRuntime(arrayID int64, role string, slot int, virtualPath, state string) error {
	res, err := s.db.Exec(`UPDATE nonraid_array_devices
		SET virtual_device_path = ?, state = ?, updated_at = ?
		WHERE array_id = ? AND role = ? AND slot = ?`,
		virtualPath, state, nowUTC(), arrayID, role, slot)
	if err != nil {
		return fmt.Errorf("set nonraid device runtime: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set nonraid device runtime rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteNonRaidArray removes a configured nonRaid array record.
func (s *Store) DeleteNonRaidArray(name string) error {
	res, err := s.db.Exec(`DELETE FROM nonraid_arrays WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete nonraid array %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete nonraid array rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanNonRaidArray(rows *sql.Rows, row *NonRaidArrayRow) error {
	if err := rows.Scan(&row.ID, &row.Name, &row.State, &row.Filesystem,
		&row.MountPath, &row.UUID, &row.ParityCount, &row.MinParityBytes,
		&row.CapacityBytes, &row.ErrorReason, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return fmt.Errorf("scan nonraid array: %w", err)
	}
	return nil
}
