package db

import (
	"database/sql"
	"strings"
	"time"
)

// SmoothfsPool is the persistence shape for an operator-declared
// smoothfs pool — what tierd wrote a systemd mount unit for in
// Phase 7.7. This sits alongside the runtime
// tiering/smoothfs.Pool (planner's in-memory registration) and
// serves as the authoritative list across tierd restarts.
type SmoothfsPool struct {
	UUID       string   `json:"uuid"`
	Name       string   `json:"name"`
	Tiers      []string `json:"tiers"`
	Mountpoint string   `json:"mountpoint"`
	UnitPath   string   `json:"unit_path"`
	CreatedAt  string   `json:"created_at"`
}

func (s *Store) CreateSmoothfsPool(p SmoothfsPool) (*SmoothfsPool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		"INSERT INTO smoothfs_pools (uuid, name, tiers, mountpoint, unit_path, created_at) VALUES (?,?,?,?,?,?)",
		p.UUID, p.Name, strings.Join(p.Tiers, ":"),
		p.Mountpoint, p.UnitPath, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	p.CreatedAt = now
	return &p, nil
}

func (s *Store) UpdateSmoothfsPool(p SmoothfsPool) error {
	res, err := s.db.Exec(
		"UPDATE smoothfs_pools SET uuid = ?, tiers = ?, mountpoint = ?, unit_path = ? WHERE name = ?",
		p.UUID, strings.Join(p.Tiers, ":"), p.Mountpoint, p.UnitPath, p.Name,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListSmoothfsPools() ([]SmoothfsPool, error) {
	rows, err := s.db.Query(
		"SELECT uuid, name, tiers, mountpoint, unit_path, created_at FROM smoothfs_pools ORDER BY name",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pools []SmoothfsPool
	for rows.Next() {
		var p SmoothfsPool
		var tierStr string
		if err := rows.Scan(&p.UUID, &p.Name, &tierStr, &p.Mountpoint, &p.UnitPath, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Tiers = strings.Split(tierStr, ":")
		pools = append(pools, p)
	}
	return pools, rows.Err()
}

// GetSmoothfsPool looks up a pool by name. Used by the destroy
// path so the caller can recover the UnitPath to tear down
// without re-deriving it from the pool name.
func (s *Store) GetSmoothfsPool(name string) (*SmoothfsPool, error) {
	var p SmoothfsPool
	var tierStr string
	err := s.db.QueryRow(
		"SELECT uuid, name, tiers, mountpoint, unit_path, created_at FROM smoothfs_pools WHERE name = ?",
		name,
	).Scan(&p.UUID, &p.Name, &tierStr, &p.Mountpoint, &p.UnitPath, &p.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Tiers = strings.Split(tierStr, ":")
	return &p, nil
}

func (s *Store) DeleteSmoothfsPool(name string) error {
	res, err := s.db.Exec("DELETE FROM smoothfs_pools WHERE name = ?", name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SmoothfsMovementLogEntry is one row from smoothfs_movement_log
// (populated by the Phase 2 movement state machine). Newest rows
// first when returned by ListSmoothfsMovementLog.
type SmoothfsMovementLogEntry struct {
	ID             int64  `json:"id"`
	ObjectID       string `json:"object_id"`
	TransactionSeq int64  `json:"transaction_seq"`
	FromState      string `json:"from_state,omitempty"`
	ToState        string `json:"to_state"`
	SourceTier     string `json:"source_tier,omitempty"`
	DestTier       string `json:"dest_tier,omitempty"`
	PayloadJSON    string `json:"payload_json"`
	WrittenAt      string `json:"written_at"`
}

// ListSmoothfsMovementLog returns the most recent movement-log
// rows, newest first, capped at limit. offset pages further back.
// Global (not per-pool) because the log rows aren't keyed by
// pool — filtering would require joining against smoothfs_objects
// which isn't worth the latency for an operator-visible UI that
// doesn't usually look past the last few hundred entries.
func (s *Store) ListSmoothfsMovementLog(limit, offset int) ([]SmoothfsMovementLogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT id, object_id, transaction_seq,
                COALESCE(from_state, ''), to_state,
                COALESCE(source_tier, ''), COALESCE(dest_tier, ''),
                payload_json, written_at
         FROM smoothfs_movement_log
         ORDER BY id DESC
         LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SmoothfsMovementLogEntry
	for rows.Next() {
		var e SmoothfsMovementLogEntry
		if err := rows.Scan(&e.ID, &e.ObjectID, &e.TransactionSeq,
			&e.FromState, &e.ToState,
			&e.SourceTier, &e.DestTier,
			&e.PayloadJSON, &e.WrittenAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
