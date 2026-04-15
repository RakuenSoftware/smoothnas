package db

// MdadmManagedTargetRow maps a unified tier_target to its per-tier VG/LV.
type MdadmManagedTargetRow struct {
	TierTargetID string
	PoolName     string
	TierName     string
	VGName       string
	LVName       string
	MountPath    string
}

// MdadmManagedNamespaceRow tracks the FUSE daemon for an mdadm namespace.
type MdadmManagedNamespaceRow struct {
	NamespaceID string
	PoolName    string
	SocketPath  string
	MountPath   string
	DaemonPID   int
	DaemonState string
}

// MdadmMovementLogRow tracks in-flight file movements for crash recovery.
type MdadmMovementLogRow struct {
	ID             int64
	ObjectID       string
	NamespaceID    string
	SourceTargetID string
	DestTargetID   string
	ObjectKey      string
	State          string
	FailureReason  string
	StartedAt      string
	UpdatedAt      string
}

// UpsertMdadmManagedTarget inserts or updates an mdadm managed target row.
func (s *Store) UpsertMdadmManagedTarget(row *MdadmManagedTargetRow) error {
	_, err := s.db.Exec(`INSERT INTO mdadm_managed_targets
		(tier_target_id, pool_name, tier_name, vg_name, lv_name, mount_path)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (tier_target_id) DO UPDATE SET
			pool_name  = excluded.pool_name,
			tier_name  = excluded.tier_name,
			vg_name    = excluded.vg_name,
			lv_name    = excluded.lv_name,
			mount_path = excluded.mount_path`,
		row.TierTargetID, row.PoolName, row.TierName,
		row.VGName, row.LVName, row.MountPath)
	return err
}

// GetMdadmManagedTarget returns the mdadm managed target for a tier_target_id.
func (s *Store) GetMdadmManagedTarget(tierTargetID string) (*MdadmManagedTargetRow, error) {
	row := s.db.QueryRow(`SELECT tier_target_id, pool_name, tier_name, vg_name, lv_name, mount_path
		FROM mdadm_managed_targets WHERE tier_target_id = ?`, tierTargetID)
	var r MdadmManagedTargetRow
	if err := row.Scan(&r.TierTargetID, &r.PoolName, &r.TierName,
		&r.VGName, &r.LVName, &r.MountPath); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListMdadmManagedTargets returns all mdadm managed targets.
func (s *Store) ListMdadmManagedTargets() ([]MdadmManagedTargetRow, error) {
	rows, err := s.db.Query(`SELECT tier_target_id, pool_name, tier_name, vg_name, lv_name, mount_path
		FROM mdadm_managed_targets ORDER BY pool_name, tier_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MdadmManagedTargetRow
	for rows.Next() {
		var r MdadmManagedTargetRow
		if err := rows.Scan(&r.TierTargetID, &r.PoolName, &r.TierName,
			&r.VGName, &r.LVName, &r.MountPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMdadmManagedTargetByPoolTier returns the target for a pool+tier name pair.
func (s *Store) GetMdadmManagedTargetByPoolTier(poolName, tierName string) (*MdadmManagedTargetRow, error) {
	row := s.db.QueryRow(`SELECT tier_target_id, pool_name, tier_name, vg_name, lv_name, mount_path
		FROM mdadm_managed_targets WHERE pool_name = ? AND tier_name = ?`, poolName, tierName)
	var r MdadmManagedTargetRow
	if err := row.Scan(&r.TierTargetID, &r.PoolName, &r.TierName,
		&r.VGName, &r.LVName, &r.MountPath); err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteMdadmManagedTarget removes an mdadm managed target row.
func (s *Store) DeleteMdadmManagedTarget(tierTargetID string) error {
	_, err := s.db.Exec(`DELETE FROM mdadm_managed_targets WHERE tier_target_id = ?`, tierTargetID)
	return err
}

// UpsertMdadmManagedNamespace inserts or updates an mdadm managed namespace.
func (s *Store) UpsertMdadmManagedNamespace(row *MdadmManagedNamespaceRow) error {
	_, err := s.db.Exec(`INSERT INTO mdadm_managed_namespaces
		(namespace_id, pool_name, socket_path, mount_path, daemon_pid, daemon_state)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (namespace_id) DO UPDATE SET
			pool_name    = excluded.pool_name,
			socket_path  = excluded.socket_path,
			mount_path   = excluded.mount_path,
			daemon_pid   = excluded.daemon_pid,
			daemon_state = excluded.daemon_state`,
		row.NamespaceID, row.PoolName, row.SocketPath,
		row.MountPath, row.DaemonPID, row.DaemonState)
	return err
}

// GetMdadmManagedNamespace returns the mdadm managed namespace.
func (s *Store) GetMdadmManagedNamespace(namespaceID string) (*MdadmManagedNamespaceRow, error) {
	row := s.db.QueryRow(`SELECT namespace_id, pool_name, socket_path, mount_path, daemon_pid, daemon_state
		FROM mdadm_managed_namespaces WHERE namespace_id = ?`, namespaceID)
	var r MdadmManagedNamespaceRow
	if err := row.Scan(&r.NamespaceID, &r.PoolName, &r.SocketPath,
		&r.MountPath, &r.DaemonPID, &r.DaemonState); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetMdadmManagedNamespaceByPool returns the namespace for a pool name.
func (s *Store) GetMdadmManagedNamespaceByPool(poolName string) (*MdadmManagedNamespaceRow, error) {
	row := s.db.QueryRow(`SELECT namespace_id, pool_name, socket_path, mount_path, daemon_pid, daemon_state
		FROM mdadm_managed_namespaces WHERE pool_name = ?`, poolName)
	var r MdadmManagedNamespaceRow
	if err := row.Scan(&r.NamespaceID, &r.PoolName, &r.SocketPath,
		&r.MountPath, &r.DaemonPID, &r.DaemonState); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListMdadmManagedNamespaces returns all mdadm managed namespaces.
func (s *Store) ListMdadmManagedNamespaces() ([]MdadmManagedNamespaceRow, error) {
	rows, err := s.db.Query(`SELECT namespace_id, pool_name, socket_path, mount_path, daemon_pid, daemon_state
		FROM mdadm_managed_namespaces ORDER BY pool_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MdadmManagedNamespaceRow
	for rows.Next() {
		var r MdadmManagedNamespaceRow
		if err := rows.Scan(&r.NamespaceID, &r.PoolName, &r.SocketPath,
			&r.MountPath, &r.DaemonPID, &r.DaemonState); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteMdadmManagedNamespace removes an mdadm managed namespace.
func (s *Store) DeleteMdadmManagedNamespace(namespaceID string) error {
	_, err := s.db.Exec(`DELETE FROM mdadm_managed_namespaces WHERE namespace_id = ?`, namespaceID)
	return err
}

// SetMdadmManagedNamespaceDaemonState updates daemon PID and state.
func (s *Store) SetMdadmManagedNamespaceDaemonState(namespaceID, state string, pid int) error {
	_, err := s.db.Exec(`UPDATE mdadm_managed_namespaces SET daemon_state = ?, daemon_pid = ? WHERE namespace_id = ?`,
		state, pid, namespaceID)
	return err
}
