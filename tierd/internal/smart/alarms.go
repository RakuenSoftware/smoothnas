package smart

import (
	"database/sql"
	"fmt"
	"time"
)

// AlarmSeverity represents the severity of a triggered alarm.
type AlarmSeverity string

const (
	SeverityOK       AlarmSeverity = "ok"
	SeverityWarning  AlarmSeverity = "warning"
	SeverityCritical AlarmSeverity = "critical"
)

// AlarmRule defines a threshold for a SMART attribute.
type AlarmRule struct {
	ID              int64  `json:"id"`
	AttributeID     int    `json:"attribute_id"`
	AttributeName   string `json:"attribute_name"`
	WarningAbove    *int64 `json:"warning_above,omitempty"`    // trigger warning when raw_value > this
	CriticalAbove   *int64 `json:"critical_above,omitempty"`   // trigger critical when raw_value > this
	WarningBelow    *int64 `json:"warning_below,omitempty"`    // trigger warning when current_val < this (for wear indicators)
	CriticalBelow   *int64 `json:"critical_below,omitempty"`   // trigger critical when current_val < this
	DevicePath      string `json:"device_path"`                // empty = applies to all disks
}

// AlarmEvent records when an alarm was triggered.
type AlarmEvent struct {
	ID            int64         `json:"id"`
	RuleID        int64         `json:"rule_id"`
	DevicePath    string        `json:"device_path"`
	AttributeID   int           `json:"attribute_id"`
	AttributeName string        `json:"attribute_name"`
	Severity      AlarmSeverity `json:"severity"`
	Value         int64         `json:"value"`
	Threshold     int64         `json:"threshold"`
	Timestamp     string        `json:"timestamp"`
}

// AlarmStore manages alarm rules and events in SQLite.
type AlarmStore struct {
	db *sql.DB
}

// NewAlarmStore creates an AlarmStore and ensures the tables exist.
func NewAlarmStore(db *sql.DB) (*AlarmStore, error) {
	store := &AlarmStore{db: db}
	if err := store.migrate(); err != nil {
		return nil, err
	}
	if err := store.seedDefaults(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *AlarmStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS smart_alarm_rules (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			attr_id         INTEGER NOT NULL,
			attr_name       TEXT    NOT NULL,
			warning_above   INTEGER,
			critical_above  INTEGER,
			warning_below   INTEGER,
			critical_below  INTEGER,
			device_path     TEXT    NOT NULL DEFAULT ''
		)
	`)
	if err != nil {
		return fmt.Errorf("create smart_alarm_rules: %w", err)
	}

	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS smart_alarm_events (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id     INTEGER NOT NULL,
			device_path TEXT    NOT NULL,
			attr_id     INTEGER NOT NULL,
			attr_name   TEXT    NOT NULL,
			severity    TEXT    NOT NULL,
			value       INTEGER NOT NULL,
			threshold   INTEGER NOT NULL,
			timestamp   TEXT    NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create smart_alarm_events: %w", err)
	}

	return nil
}

// seedDefaults inserts the default alarm rules if no rules exist.
func (s *AlarmStore) seedDefaults() error {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM smart_alarm_rules").Scan(&count)
	if count > 0 {
		return nil
	}

	defaults := []AlarmRule{
		{AttributeID: 5, AttributeName: "Reallocated_Sector_Ct", WarningAbove: intPtr(0), CriticalAbove: intPtr(50)},
		{AttributeID: 197, AttributeName: "Current_Pending_Sector", WarningAbove: intPtr(0), CriticalAbove: intPtr(10)},
		{AttributeID: 198, AttributeName: "Offline_Uncorrectable", WarningAbove: intPtr(0), CriticalAbove: intPtr(5)},
		{AttributeID: 187, AttributeName: "Reported_Uncorrect", WarningAbove: intPtr(0), CriticalAbove: intPtr(10)},
		{AttributeID: 194, AttributeName: "Temperature_Celsius", WarningAbove: intPtr(50), CriticalAbove: intPtr(60)},
		{AttributeID: 9, AttributeName: "Power_On_Hours", WarningAbove: intPtr(35000), CriticalAbove: intPtr(50000)},
		{AttributeID: 177, AttributeName: "Wear_Leveling_Count", WarningBelow: intPtr(20), CriticalBelow: intPtr(5)},
		{AttributeID: 233, AttributeName: "Media_Wearout_Indicator", WarningBelow: intPtr(20), CriticalBelow: intPtr(5)},
	}

	for _, rule := range defaults {
		if _, err := s.CreateRule(rule); err != nil {
			return fmt.Errorf("seed default rule %s: %w", rule.AttributeName, err)
		}
	}

	return nil
}

// CreateRule inserts a new alarm rule.
func (s *AlarmStore) CreateRule(rule AlarmRule) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO smart_alarm_rules (attr_id, attr_name, warning_above, critical_above, warning_below, critical_below, device_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rule.AttributeID, rule.AttributeName, rule.WarningAbove, rule.CriticalAbove, rule.WarningBelow, rule.CriticalBelow, rule.DevicePath,
	)
	if err != nil {
		return 0, fmt.Errorf("insert rule: %w", err)
	}
	return res.LastInsertId()
}

// UpdateRule updates an existing alarm rule.
func (s *AlarmStore) UpdateRule(id int64, rule AlarmRule) error {
	_, err := s.db.Exec(
		`UPDATE smart_alarm_rules SET attr_id=?, attr_name=?, warning_above=?, critical_above=?, warning_below=?, critical_below=?, device_path=?
		 WHERE id=?`,
		rule.AttributeID, rule.AttributeName, rule.WarningAbove, rule.CriticalAbove, rule.WarningBelow, rule.CriticalBelow, rule.DevicePath, id,
	)
	return err
}

// DeleteRule removes an alarm rule.
func (s *AlarmStore) DeleteRule(id int64) error {
	_, err := s.db.Exec("DELETE FROM smart_alarm_rules WHERE id = ?", id)
	return err
}

// ListRules returns all alarm rules.
func (s *AlarmStore) ListRules() ([]AlarmRule, error) {
	rows, err := s.db.Query("SELECT id, attr_id, attr_name, warning_above, critical_above, warning_below, critical_below, device_path FROM smart_alarm_rules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []AlarmRule
	for rows.Next() {
		var r AlarmRule
		if err := rows.Scan(&r.ID, &r.AttributeID, &r.AttributeName, &r.WarningAbove, &r.CriticalAbove, &r.WarningBelow, &r.CriticalBelow, &r.DevicePath); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// Evaluate checks a disk's SMART data against all applicable alarm rules.
// Returns the list of alarm events triggered (does NOT persist them; caller decides).
func (s *AlarmStore) Evaluate(data *Data) ([]AlarmEvent, error) {
	rules, err := s.ListRules()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var events []AlarmEvent

	for _, rule := range rules {
		// Skip rules scoped to a different device.
		if rule.DevicePath != "" && rule.DevicePath != data.DevicePath {
			continue
		}

		for _, attr := range data.Attributes {
			if attr.ID != rule.AttributeID {
				continue
			}

			// Check "above" thresholds (raw value).
			if rule.CriticalAbove != nil && attr.RawValue > *rule.CriticalAbove {
				events = append(events, AlarmEvent{
					RuleID: rule.ID, DevicePath: data.DevicePath,
					AttributeID: attr.ID, AttributeName: attr.Name,
					Severity: SeverityCritical, Value: attr.RawValue, Threshold: *rule.CriticalAbove,
					Timestamp: now,
				})
			} else if rule.WarningAbove != nil && attr.RawValue > *rule.WarningAbove {
				events = append(events, AlarmEvent{
					RuleID: rule.ID, DevicePath: data.DevicePath,
					AttributeID: attr.ID, AttributeName: attr.Name,
					Severity: SeverityWarning, Value: attr.RawValue, Threshold: *rule.WarningAbove,
					Timestamp: now,
				})
			}

			// Check "below" thresholds (current value, for wear indicators).
			if rule.CriticalBelow != nil && int64(attr.Current) < *rule.CriticalBelow {
				events = append(events, AlarmEvent{
					RuleID: rule.ID, DevicePath: data.DevicePath,
					AttributeID: attr.ID, AttributeName: attr.Name,
					Severity: SeverityCritical, Value: int64(attr.Current), Threshold: *rule.CriticalBelow,
					Timestamp: now,
				})
			} else if rule.WarningBelow != nil && int64(attr.Current) < *rule.WarningBelow {
				events = append(events, AlarmEvent{
					RuleID: rule.ID, DevicePath: data.DevicePath,
					AttributeID: attr.ID, AttributeName: attr.Name,
					Severity: SeverityWarning, Value: int64(attr.Current), Threshold: *rule.WarningBelow,
					Timestamp: now,
				})
			}
		}
	}

	return events, nil
}

// RecordEvent persists an alarm event.
func (s *AlarmStore) RecordEvent(e AlarmEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO smart_alarm_events (rule_id, device_path, attr_id, attr_name, severity, value, threshold, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RuleID, e.DevicePath, e.AttributeID, e.AttributeName, e.Severity, e.Value, e.Threshold, e.Timestamp,
	)
	return err
}

// ListEvents returns alarm events, optionally filtered.
func (s *AlarmStore) ListEvents(devicePath *string, severity *string, limit int) ([]AlarmEvent, error) {
	query := "SELECT id, rule_id, device_path, attr_id, attr_name, severity, value, threshold, timestamp FROM smart_alarm_events WHERE 1=1"
	var args []any

	if devicePath != nil {
		query += " AND device_path = ?"
		args = append(args, *devicePath)
	}
	if severity != nil {
		query += " AND severity = ?"
		args = append(args, *severity)
	}

	query += " ORDER BY timestamp DESC"

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []AlarmEvent
	for rows.Next() {
		var e AlarmEvent
		if err := rows.Scan(&e.ID, &e.RuleID, &e.DevicePath, &e.AttributeID, &e.AttributeName, &e.Severity, &e.Value, &e.Threshold, &e.Timestamp); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func intPtr(v int64) *int64 {
	return &v
}
