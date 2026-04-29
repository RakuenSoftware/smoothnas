package smart

import (
	"database/sql"
	"fmt"
	"time"
)

// HistoryEntry represents a single SMART attribute snapshot.
type HistoryEntry struct {
	Timestamp   string `json:"timestamp"`
	AttributeID int    `json:"attribute_id"`
	Name        string `json:"name"`
	Current     int    `json:"current"`
	RawValue    int64  `json:"raw_value"`
}

// HistoryStore manages SMART history in SQLite.
type HistoryStore struct {
	db *sql.DB
}

// NewHistoryStore returns a HistoryStore against db. The schema is
// owned by the goose migrations under tierd/internal/db/migrations/;
// callers are responsible for having run db.Migrate() / db.MigrateDB()
// before constructing the store.
func NewHistoryStore(db *sql.DB) (*HistoryStore, error) {
	return &HistoryStore{db: db}, nil
}

// RecordSnapshot saves all SMART attributes for a device at the current time.
func (s *HistoryStore) RecordSnapshot(data *Data) error {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO smart_history (device_path, timestamp, attr_id, attr_name, current_val, raw_value)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, attr := range data.Attributes {
		if _, err := stmt.Exec(data.DevicePath, now, attr.ID, attr.Name, attr.Current, attr.RawValue); err != nil {
			return fmt.Errorf("insert attr %d: %w", attr.ID, err)
		}
	}

	// Also record temperature and power-on hours as synthetic entries
	// so they appear in history charts.
	if data.Temperature > 0 {
		if _, err := stmt.Exec(data.DevicePath, now, 194, "Temperature_Celsius", data.Temperature, int64(data.Temperature)); err != nil {
			return fmt.Errorf("insert temperature: %w", err)
		}
	}

	return tx.Commit()
}

// Query returns SMART history for a device, optionally filtered by attribute ID and time range.
func (s *HistoryStore) Query(devicePath string, attrID *int, since, until *string) ([]HistoryEntry, error) {
	query := "SELECT timestamp, attr_id, attr_name, current_val, raw_value FROM smart_history WHERE device_path = ?"
	args := []any{devicePath}

	if attrID != nil {
		query += " AND attr_id = ?"
		args = append(args, *attrID)
	}
	if since != nil {
		query += " AND timestamp >= ?"
		args = append(args, *since)
	}
	if until != nil {
		query += " AND timestamp <= ?"
		args = append(args, *until)
	}

	query += " ORDER BY timestamp ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var entries []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.Timestamp, &e.AttributeID, &e.Name, &e.Current, &e.RawValue); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// CleanOlderThan removes history entries older than the given duration.
func (s *HistoryStore) CleanOlderThan(age time.Duration) error {
	cutoff := time.Now().UTC().Add(-age).Format(time.RFC3339)
	_, err := s.db.Exec("DELETE FROM smart_history WHERE timestamp < ?", cutoff)
	return err
}
