package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// GetConfig returns a persisted appliance config value.
func (s *Store) GetConfig(key string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get config %q: %w", key, err)
	}
	return val, nil
}

// SetConfig stores or replaces a persisted appliance config value.
func (s *Store) SetConfig(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO config (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set config %q: %w", key, err)
	}
	return nil
}

// GetBoolConfig returns a boolean config value, falling back to def when unset.
func (s *Store) GetBoolConfig(key string, def bool) (bool, error) {
	val, err := s.GetConfig(key)
	if err == ErrNotFound {
		return def, nil
	}
	if err != nil {
		return false, err
	}
	switch val {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("config %q has invalid boolean value %q", key, val)
	}
}

// SetBoolConfig stores a boolean config value as "1" or "0".
func (s *Store) SetBoolConfig(key string, value bool) error {
	if value {
		return s.SetConfig(key, "1")
	}
	return s.SetConfig(key, "0")
}

// DeleteConfig removes a persisted appliance config value. Missing keys are
// accepted so callers can use it for idempotent cleanup.
func (s *Store) DeleteConfig(key string) error {
	if _, err := s.db.Exec(`DELETE FROM config WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete config %q: %w", key, err)
	}
	return nil
}

// ListConfigByPrefix returns all config key/value pairs whose key starts with
// the given prefix. Used to enumerate per-disk settings (e.g. spindown timers)
// for boot-time reconciliation.
func (s *Store) ListConfigByPrefix(prefix string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM config WHERE key LIKE ? ESCAPE '\'`, escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("list config by prefix %q: %w", prefix, err)
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		result[k] = v
	}
	return result, rows.Err()
}

// escapeLike escapes LIKE wildcards so a literal prefix is matched verbatim.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
