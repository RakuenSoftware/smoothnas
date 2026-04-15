package db

import (
	"database/sql"
	"fmt"
	"time"
)

// ---- ZFS managed adapter row types ----------------------------------------

// ZFSManagedTargetRow is a row in the zfs_managed_targets table.
type ZFSManagedTargetRow struct {
	TierTargetID string
	PoolName     string
	DatasetName  string
	DatasetPath  string
	FUSEMode     string // passthrough | fallback | unknown
}

// ZFSManagedNamespaceRow is a row in the zfs_managed_namespaces table.
type ZFSManagedNamespaceRow struct {
	NamespaceID            string
	PoolName               string
	MetaDataset            string
	SocketPath             string
	MountPath              string
	DaemonPID              int    // 0 if not running
	DaemonState            string // stopped | starting | running | crashed
	FUSEMode               string // passthrough | fallback | unknown
	SnapshotMode           string // none | coordinated-namespace
	SnapshotPoolName       string // non-empty when coordinated-namespace
	SnapshotQuiesceTimeout int    // seconds, default 30
}

// ZFSManagedNamespaceSnapshotRow is a row in the zfs_managed_namespace_snapshots table.
type ZFSManagedNamespaceSnapshotRow struct {
	ID                   string
	NamespaceID          string
	PoolName             string
	ZFSSnapshotName      string
	BackingSnapshotsJSON string
	MetaSnapshotJSON     string
	CreatedAt            string
	Consistency          string
}

// ---- zfs_managed_targets ----------------------------------------------------

// UpsertZFSManagedTarget inserts or replaces a ZFS managed target row.
func (s *Store) UpsertZFSManagedTarget(row *ZFSManagedTargetRow) error {
	_, err := s.db.Exec(`
		INSERT INTO zfs_managed_targets
			(tier_target_id, pool_name, dataset_name, dataset_path, fuse_mode)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tier_target_id) DO UPDATE SET
			pool_name    = excluded.pool_name,
			dataset_name = excluded.dataset_name,
			dataset_path = excluded.dataset_path,
			fuse_mode    = excluded.fuse_mode`,
		row.TierTargetID, row.PoolName, row.DatasetName, row.DatasetPath, row.FUSEMode)
	if err != nil {
		return fmt.Errorf("upsert zfs managed target: %w", err)
	}
	return nil
}

// GetZFSManagedTarget returns the ZFS managed target row for the given tier_target_id.
func (s *Store) GetZFSManagedTarget(tierTargetID string) (*ZFSManagedTargetRow, error) {
	var r ZFSManagedTargetRow
	err := s.db.QueryRow(`
		SELECT tier_target_id, pool_name, dataset_name, dataset_path, fuse_mode
		FROM zfs_managed_targets WHERE tier_target_id = ?`, tierTargetID).
		Scan(&r.TierTargetID, &r.PoolName, &r.DatasetName, &r.DatasetPath, &r.FUSEMode)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get zfs managed target %q: %w", tierTargetID, err)
	}
	return &r, nil
}

// ListZFSManagedTargets returns all ZFS managed target rows.
func (s *Store) ListZFSManagedTargets() ([]ZFSManagedTargetRow, error) {
	rows, err := s.db.Query(`
		SELECT tier_target_id, pool_name, dataset_name, dataset_path, fuse_mode
		FROM zfs_managed_targets
		ORDER BY tier_target_id`)
	if err != nil {
		return nil, fmt.Errorf("list zfs managed targets: %w", err)
	}
	defer rows.Close()
	var out []ZFSManagedTargetRow
	for rows.Next() {
		var r ZFSManagedTargetRow
		if err := rows.Scan(&r.TierTargetID, &r.PoolName, &r.DatasetName, &r.DatasetPath, &r.FUSEMode); err != nil {
			return nil, fmt.Errorf("scan zfs managed target: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteZFSManagedTarget removes the ZFS managed target row for the given tier_target_id.
func (s *Store) DeleteZFSManagedTarget(tierTargetID string) error {
	if _, err := s.db.Exec(`DELETE FROM zfs_managed_targets WHERE tier_target_id = ?`, tierTargetID); err != nil {
		return fmt.Errorf("delete zfs managed target: %w", err)
	}
	return nil
}

// UpdateZFSManagedTargetFUSEMode updates the FUSE mode for a managed target.
func (s *Store) UpdateZFSManagedTargetFUSEMode(tierTargetID, fuseMode string) error {
	_, err := s.db.Exec(`
		UPDATE zfs_managed_targets SET fuse_mode = ? WHERE tier_target_id = ?`,
		fuseMode, tierTargetID)
	if err != nil {
		return fmt.Errorf("update zfs managed target fuse mode: %w", err)
	}
	return nil
}

// ---- zfs_managed_namespaces -------------------------------------------------

// UpsertZFSManagedNamespace inserts or replaces a ZFS managed namespace row.
func (s *Store) UpsertZFSManagedNamespace(row *ZFSManagedNamespaceRow) error {
	var pid sql.NullInt64
	if row.DaemonPID > 0 {
		pid = sql.NullInt64{Int64: int64(row.DaemonPID), Valid: true}
	}
	quiesceTimeout := row.SnapshotQuiesceTimeout
	if quiesceTimeout == 0 {
		quiesceTimeout = 30
	}
	_, err := s.db.Exec(`
		INSERT INTO zfs_managed_namespaces
			(namespace_id, pool_name, meta_dataset, socket_path, mount_path,
			 daemon_pid, daemon_state, fuse_mode,
			 snapshot_mode, snapshot_pool_name, snapshot_quiesce_timeout_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(namespace_id) DO UPDATE SET
			pool_name                        = excluded.pool_name,
			meta_dataset                     = excluded.meta_dataset,
			socket_path                      = excluded.socket_path,
			mount_path                       = excluded.mount_path,
			daemon_pid                       = excluded.daemon_pid,
			daemon_state                     = excluded.daemon_state,
			fuse_mode                        = excluded.fuse_mode,
			snapshot_mode                    = excluded.snapshot_mode,
			snapshot_pool_name               = excluded.snapshot_pool_name,
			snapshot_quiesce_timeout_seconds = excluded.snapshot_quiesce_timeout_seconds`,
		row.NamespaceID, row.PoolName, row.MetaDataset, row.SocketPath, row.MountPath,
		pid, row.DaemonState, row.FUSEMode,
		row.SnapshotMode, row.SnapshotPoolName, quiesceTimeout)
	if err != nil {
		return fmt.Errorf("upsert zfs managed namespace: %w", err)
	}
	return nil
}

// GetZFSManagedNamespace returns the ZFS managed namespace row for the given namespace_id.
func (s *Store) GetZFSManagedNamespace(namespaceID string) (*ZFSManagedNamespaceRow, error) {
	var r ZFSManagedNamespaceRow
	var pid sql.NullInt64
	err := s.db.QueryRow(`
		SELECT namespace_id, pool_name, meta_dataset, socket_path, mount_path,
		       daemon_pid, daemon_state, fuse_mode,
		       snapshot_mode, snapshot_pool_name, snapshot_quiesce_timeout_seconds
		FROM zfs_managed_namespaces WHERE namespace_id = ?`, namespaceID).
		Scan(&r.NamespaceID, &r.PoolName, &r.MetaDataset, &r.SocketPath, &r.MountPath,
			&pid, &r.DaemonState, &r.FUSEMode,
			&r.SnapshotMode, &r.SnapshotPoolName, &r.SnapshotQuiesceTimeout)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get zfs managed namespace %q: %w", namespaceID, err)
	}
	if pid.Valid {
		r.DaemonPID = int(pid.Int64)
	}
	return &r, nil
}

// ListZFSManagedNamespaces returns all ZFS managed namespace rows.
func (s *Store) ListZFSManagedNamespaces() ([]ZFSManagedNamespaceRow, error) {
	rows, err := s.db.Query(`
		SELECT namespace_id, pool_name, meta_dataset, socket_path, mount_path,
		       daemon_pid, daemon_state, fuse_mode,
		       snapshot_mode, snapshot_pool_name, snapshot_quiesce_timeout_seconds
		FROM zfs_managed_namespaces
		ORDER BY namespace_id`)
	if err != nil {
		return nil, fmt.Errorf("list zfs managed namespaces: %w", err)
	}
	defer rows.Close()
	var out []ZFSManagedNamespaceRow
	for rows.Next() {
		var r ZFSManagedNamespaceRow
		var pid sql.NullInt64
		if err := rows.Scan(&r.NamespaceID, &r.PoolName, &r.MetaDataset, &r.SocketPath, &r.MountPath,
			&pid, &r.DaemonState, &r.FUSEMode,
			&r.SnapshotMode, &r.SnapshotPoolName, &r.SnapshotQuiesceTimeout); err != nil {
			return nil, fmt.Errorf("scan zfs managed namespace: %w", err)
		}
		if pid.Valid {
			r.DaemonPID = int(pid.Int64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetZFSManagedNamespaceSnapshotMode returns the snapshot_mode for the given namespace.
// Returns "none" if the namespace is not found or not a ZFS managed namespace.
func (s *Store) GetZFSManagedNamespaceSnapshotMode(namespaceID string) string {
	var mode string
	if err := s.db.QueryRow(
		`SELECT snapshot_mode FROM zfs_managed_namespaces WHERE namespace_id = ?`,
		namespaceID).Scan(&mode); err != nil {
		return "none"
	}
	return mode
}

// DeleteZFSManagedNamespace removes the ZFS managed namespace row.
func (s *Store) DeleteZFSManagedNamespace(namespaceID string) error {
	if _, err := s.db.Exec(`DELETE FROM zfs_managed_namespaces WHERE namespace_id = ?`, namespaceID); err != nil {
		return fmt.Errorf("delete zfs managed namespace: %w", err)
	}
	return nil
}

// SetZFSManagedNamespaceDaemonState updates the daemon state and PID for a namespace.
func (s *Store) SetZFSManagedNamespaceDaemonState(namespaceID, daemonState string, pid int) error {
	var sqlPID sql.NullInt64
	if pid > 0 {
		sqlPID = sql.NullInt64{Int64: int64(pid), Valid: true}
	}
	_, err := s.db.Exec(`
		UPDATE zfs_managed_namespaces
		SET daemon_state = ?, daemon_pid = ?
		WHERE namespace_id = ?`,
		daemonState, sqlPID, namespaceID)
	if err != nil {
		return fmt.Errorf("set zfs managed namespace daemon state: %w", err)
	}
	return nil
}

// SetZFSManagedNamespaceFUSEMode updates the FUSE mode for a namespace.
func (s *Store) SetZFSManagedNamespaceFUSEMode(namespaceID, fuseMode string) error {
	_, err := s.db.Exec(`
		UPDATE zfs_managed_namespaces SET fuse_mode = ? WHERE namespace_id = ?`,
		fuseMode, namespaceID)
	if err != nil {
		return fmt.Errorf("set zfs managed namespace fuse mode: %w", err)
	}
	return nil
}

// ---- zfs_movement_log ---------------------------------------------------

// Movement log state constants (P04B crash-recovery state machine).
const (
	ZFSMoveLogCopyInProgress  = "copy_in_progress"
	ZFSMoveLogCopyComplete    = "copy_complete"
	ZFSMoveLogSwitched        = "switched"
	ZFSMoveLogCleanupComplete = "cleanup_complete"
	ZFSMoveLogFailed          = "failed"
)

// ZFSMovementLogRow is a row in the zfs_movement_log table.
type ZFSMovementLogRow struct {
	ID             string
	ObjectID       string
	NamespaceID    string
	SourceTargetID string
	DestTargetID   string
	ObjectKey      string
	State          string
	FailureReason  string
	StartedAt      int64
	UpdatedAt      int64
}

// InsertZFSMovementLog inserts a new movement log row.
func (s *Store) InsertZFSMovementLog(row *ZFSMovementLogRow) error {
	if row.ID == "" {
		id, err := newControlPlaneID()
		if err != nil {
			return err
		}
		row.ID = id
	}
	_, err := s.db.Exec(`
		INSERT INTO zfs_movement_log
			(id, object_id, namespace_id, source_target_id, dest_target_id,
			 object_key, state, failure_reason, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.ObjectID, row.NamespaceID, row.SourceTargetID, row.DestTargetID,
		row.ObjectKey, row.State, row.FailureReason, row.StartedAt, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert zfs movement log: %w", err)
	}
	return nil
}

// UpdateZFSMovementLogState updates the state and failure_reason for a movement log row.
func (s *Store) UpdateZFSMovementLogState(id, state, failureReason string) error {
	now := unixNow()
	_, err := s.db.Exec(`
		UPDATE zfs_movement_log
		SET state = ?, failure_reason = ?, updated_at = ?
		WHERE id = ?`,
		state, failureReason, now, id)
	if err != nil {
		return fmt.Errorf("update zfs movement log state %q: %w", id, err)
	}
	return nil
}

// ListZFSMovementLogNonTerminal returns all rows not in a terminal state
// (cleanup_complete or failed). Used by Reconcile() for crash recovery.
func (s *Store) ListZFSMovementLogNonTerminal() ([]ZFSMovementLogRow, error) {
	rows, err := s.db.Query(`
		SELECT id, object_id, namespace_id, source_target_id, dest_target_id,
		       object_key, state, failure_reason, started_at, updated_at
		FROM zfs_movement_log
		WHERE state NOT IN ('cleanup_complete', 'failed')
		ORDER BY started_at`)
	if err != nil {
		return nil, fmt.Errorf("list zfs movement log non-terminal: %w", err)
	}
	defer rows.Close()
	var out []ZFSMovementLogRow
	for rows.Next() {
		var r ZFSMovementLogRow
		if err := rows.Scan(&r.ID, &r.ObjectID, &r.NamespaceID, &r.SourceTargetID, &r.DestTargetID,
			&r.ObjectKey, &r.State, &r.FailureReason, &r.StartedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan zfs movement log: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetZFSMovementLog returns the movement log row with the given id.
func (s *Store) GetZFSMovementLog(id string) (*ZFSMovementLogRow, error) {
	var r ZFSMovementLogRow
	err := s.db.QueryRow(`
		SELECT id, object_id, namespace_id, source_target_id, dest_target_id,
		       object_key, state, failure_reason, started_at, updated_at
		FROM zfs_movement_log WHERE id = ?`, id).
		Scan(&r.ID, &r.ObjectID, &r.NamespaceID, &r.SourceTargetID, &r.DestTargetID,
			&r.ObjectKey, &r.State, &r.FailureReason, &r.StartedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get zfs movement log %q: %w", id, err)
	}
	return &r, nil
}

// unixNow returns the current Unix timestamp as int64.
func unixNow() int64 {
	return time.Now().Unix()
}

// SetZFSManagedNamespaceSnapshotMode updates the snapshot_mode and
// snapshot_pool_name for a namespace.
func (s *Store) SetZFSManagedNamespaceSnapshotMode(namespaceID, mode, poolName string) error {
	_, err := s.db.Exec(`
		UPDATE zfs_managed_namespaces
		SET snapshot_mode = ?, snapshot_pool_name = ?
		WHERE namespace_id = ?`,
		mode, poolName, namespaceID)
	if err != nil {
		return fmt.Errorf("set zfs managed namespace snapshot mode: %w", err)
	}
	return nil
}

// ---- zfs_managed_namespace_snapshots ----------------------------------------

// CreateZFSManagedNamespaceSnapshot inserts a new snapshot record.
func (s *Store) CreateZFSManagedNamespaceSnapshot(row *ZFSManagedNamespaceSnapshotRow) error {
	if row.ID == "" {
		id, err := newControlPlaneID()
		if err != nil {
			return err
		}
		row.ID = id
	}
	if row.CreatedAt == "" {
		row.CreatedAt = nowUTC()
	}
	_, err := s.db.Exec(`
		INSERT INTO zfs_managed_namespace_snapshots
			(id, namespace_id, pool_name, zfs_snapshot_name,
			 backing_snapshots_json, meta_snapshot_json, created_at, consistency)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.NamespaceID, row.PoolName, row.ZFSSnapshotName,
		row.BackingSnapshotsJSON, row.MetaSnapshotJSON, row.CreatedAt, row.Consistency)
	if err != nil {
		return fmt.Errorf("create zfs namespace snapshot: %w", err)
	}
	return nil
}

// GetZFSManagedNamespaceSnapshot returns the snapshot record by namespace and snapshot ID.
func (s *Store) GetZFSManagedNamespaceSnapshot(namespaceID, snapshotID string) (*ZFSManagedNamespaceSnapshotRow, error) {
	var r ZFSManagedNamespaceSnapshotRow
	err := s.db.QueryRow(`
		SELECT id, namespace_id, pool_name, zfs_snapshot_name,
		       backing_snapshots_json, meta_snapshot_json, created_at, consistency
		FROM zfs_managed_namespace_snapshots
		WHERE namespace_id = ? AND id = ?`, namespaceID, snapshotID).
		Scan(&r.ID, &r.NamespaceID, &r.PoolName, &r.ZFSSnapshotName,
			&r.BackingSnapshotsJSON, &r.MetaSnapshotJSON, &r.CreatedAt, &r.Consistency)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get zfs namespace snapshot %q: %w", snapshotID, err)
	}
	return &r, nil
}

// ListZFSManagedNamespaceSnapshots returns snapshot summaries for the namespace,
// ordered by created_at descending, capped at 50.
func (s *Store) ListZFSManagedNamespaceSnapshots(namespaceID string) ([]ZFSManagedNamespaceSnapshotRow, error) {
	rows, err := s.db.Query(`
		SELECT id, namespace_id, pool_name, zfs_snapshot_name,
		       backing_snapshots_json, meta_snapshot_json, created_at, consistency
		FROM zfs_managed_namespace_snapshots
		WHERE namespace_id = ?
		ORDER BY created_at DESC
		LIMIT 50`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list zfs namespace snapshots: %w", err)
	}
	defer rows.Close()
	var out []ZFSManagedNamespaceSnapshotRow
	for rows.Next() {
		var r ZFSManagedNamespaceSnapshotRow
		if err := rows.Scan(&r.ID, &r.NamespaceID, &r.PoolName, &r.ZFSSnapshotName,
			&r.BackingSnapshotsJSON, &r.MetaSnapshotJSON, &r.CreatedAt, &r.Consistency); err != nil {
			return nil, fmt.Errorf("scan zfs namespace snapshot: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteZFSManagedNamespaceSnapshot removes a snapshot record.
func (s *Store) DeleteZFSManagedNamespaceSnapshot(namespaceID, snapshotID string) error {
	res, err := s.db.Exec(`
		DELETE FROM zfs_managed_namespace_snapshots
		WHERE namespace_id = ? AND id = ?`, namespaceID, snapshotID)
	if err != nil {
		return fmt.Errorf("delete zfs namespace snapshot: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
