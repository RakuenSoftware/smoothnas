package db

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

const (
	TierSlotNVME = "NVME"
	TierSlotSSD  = "SSD"
	TierSlotHDD  = "HDD"
)

const (
	TierPoolStateProvisioning = "provisioning"
	TierPoolStateHealthy      = "healthy"
	TierPoolStateDegraded     = "degraded"
	TierPoolStateUnmounted    = "unmounted"
	TierPoolStateError        = "error"
	TierPoolStateDestroying   = "destroying"
)

const (
	TierSlotStateEmpty    = "empty"
	TierSlotStateAssigned = "assigned"
	TierSlotStateDegraded = "degraded"
	TierSlotStateMissing  = "missing"
)

var (
	tierNameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)
	tierNameStartRe  = regexp.MustCompile(`^[a-z0-9]`)
	tierReservedName = map[string]struct{}{
		"data":       {},
		"root":       {},
		"home":       {},
		"boot":       {},
		"tmp":        {},
		"var":        {},
		"sys":        {},
		"proc":       {},
		"run":        {},
		"dev":        {},
		"mnt":        {},
		"srv":        {},
		"opt":        {},
		"etc":        {},
		"lost+found": {},
		"policy":     {}, // reserved as a top-level API endpoint
	}
)

const (
	maxTierNameLength = 31
	maxVGNameLength   = 127
	tierVGPrefix      = "tier-"
)

var TierMountRoot = "/mnt"

var validTierPoolTransitions = map[string]map[string]struct{}{
	TierPoolStateProvisioning: {
		TierPoolStateHealthy:    {},
		TierPoolStateError:      {},
		TierPoolStateDestroying: {},
	},
	TierPoolStateHealthy: {
		TierPoolStateProvisioning: {},
		TierPoolStateDegraded:     {},
		TierPoolStateError:        {},
		TierPoolStateUnmounted:    {},
		TierPoolStateDestroying:   {},
	},
	TierPoolStateDegraded: {
		TierPoolStateProvisioning: {},
		TierPoolStateHealthy:      {},
		TierPoolStateError:        {},
		TierPoolStateDestroying:   {},
	},
	TierPoolStateUnmounted: {
		TierPoolStateProvisioning: {},
		TierPoolStateError:        {},
		TierPoolStateDestroying:   {},
	},
	TierPoolStateError: {
		TierPoolStateProvisioning: {},
		TierPoolStateDestroying:   {},
		// Allow self-healing back to healthy once the reconciler verifies
		// the underlying condition that tripped the error has cleared.
		TierPoolStateHealthy:  {},
		TierPoolStateDegraded: {},
	},
}

var validTierSlotTransitions = map[string]map[string]struct{}{
	TierSlotStateEmpty: {
		TierSlotStateAssigned: {},
	},
	TierSlotStateAssigned: {
		TierSlotStateEmpty:    {},
		TierSlotStateDegraded: {},
		TierSlotStateMissing:  {},
	},
	TierSlotStateDegraded: {
		TierSlotStateAssigned: {},
	},
	TierSlotStateMissing: {
		TierSlotStateAssigned: {},
	},
}

type TierInstance struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Filesystem       string `json:"filesystem"`
	MountPoint       string `json:"mount_point"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	LastReconciledAt string `json:"last_reconciled_at,omitempty"`
	State            string `json:"state"`
	ErrorReason      string `json:"error_reason,omitempty"`
	// MetaOnFastest: when true, PoolMetaStore holds all metadata on the
	// fastest tier only. Reduces metadata redundancy (failure of the
	// fastest tier loses the index) in exchange for locality.
	MetaOnFastest bool `json:"meta_on_fastest"`
}

type TierDefinition struct {
	Name string `json:"name"`
	Rank int    `json:"rank"`
}

type TierArrayAssignment struct {
	ID        int64  `json:"id"`
	PoolID    int64  `json:"pool_id"`
	TierName  string `json:"tier_name"`
	Slot      string `json:"slot"`
	Rank      int    `json:"rank"`
	ArrayPath string `json:"array_path"`
	PVDevice  string `json:"pv_device"`
	State     string `json:"state"`
}

type TierSlot struct {
	ID               int64   `json:"id"`
	PoolID           int64   `json:"pool_id"`
	PoolName         string  `json:"pool_name"`
	Name             string  `json:"name"`
	Rank             int     `json:"rank"`
	State            string  `json:"state"`
	ArrayID          *int64  `json:"array_id,omitempty"`
	ArrayPath        string  `json:"array_path,omitempty"`
	PVDevice         *string `json:"pv_device,omitempty"`
	TargetFillPct    int     `json:"target_fill_pct"`
	FullThresholdPct int     `json:"full_threshold_pct"`
	// BackingKind identifies the storage layer that supplies this slot:
	// "mdadm" (default; array_id + pv_device are the block device),
	// "zfs" (backing_ref is the zpool name; tier data goes in a dataset),
	// "btrfs", "bcachefs" (reserved for follow-up work).
	BackingKind string `json:"backing_kind"`
	// BackingRef is the kind-specific identifier:
	//   mdadm    → block device path (/dev/md0), duplicated in PVDevice
	//   zfs      → zpool name
	//   btrfs    → device path or label
	//   bcachefs → device path or UUID
	BackingRef string `json:"backing_ref,omitempty"`
}

func NormalizeArrayPath(array string) string {
	array = strings.TrimSpace(array)
	if array == "" {
		return ""
	}
	if strings.HasPrefix(array, "/dev/") {
		return array
	}
	return "/dev/" + array
}

// IsValidTierSlot reports whether slot is a non-empty tier name. Tier names
// are user-defined (seeded with NVME, SSD, HDD by default, but operators may
// add more), so any non-empty string is accepted here; existence within a pool
// is verified by the DB layer.
func IsValidTierSlot(slot string) bool {
	return strings.TrimSpace(slot) != ""
}

func DefaultTierDefinitions() []TierDefinition {
	return []TierDefinition{
		{Name: TierSlotNVME, Rank: 1},
		{Name: TierSlotSSD, Rank: 2},
		{Name: TierSlotHDD, Rank: 3},
	}
}

func ValidateTierPoolState(state string) error {
	switch state {
	case TierPoolStateProvisioning, TierPoolStateHealthy, TierPoolStateDegraded, TierPoolStateUnmounted, TierPoolStateError, TierPoolStateDestroying:
		return nil
	default:
		return fmt.Errorf("invalid tier pool state %q", state)
	}
}

func ValidateTierSlotState(state string) error {
	switch state {
	case TierSlotStateEmpty, TierSlotStateAssigned, TierSlotStateDegraded, TierSlotStateMissing:
		return nil
	default:
		return fmt.Errorf("invalid tier slot state %q", state)
	}
}

func canTransition(valid map[string]map[string]struct{}, from, to string) bool {
	if from == to {
		return true
	}
	next := valid[from]
	if next == nil {
		return false
	}
	_, ok := next[to]
	return ok
}

func ValidateTierInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("tier name is required")
	}
	if _, reserved := tierReservedName[name]; reserved {
		return fmt.Errorf("tier name %q is reserved", name)
	}
	if len(name) > maxTierNameLength {
		return fmt.Errorf("tier name %q must be 31 characters or fewer", name)
	}
	if !tierNameStartRe.MatchString(name) {
		return fmt.Errorf("tier name %q must start with a lowercase letter or digit", name)
	}
	if !tierNameRe.MatchString(name) {
		return fmt.Errorf("tier name %q must contain only lowercase letters, digits, hyphens, or underscores", name)
	}
	if len(tierVGPrefix)+len(name) > maxVGNameLength {
		return fmt.Errorf("volume group name %q exceeds the 127 character LVM limit", tierVGPrefix+name)
	}
	return nil
}

func ValidateTierDefinitions(tiers []TierDefinition) error {
	if len(tiers) == 0 {
		return nil
	}
	seenNames := map[string]struct{}{}
	seenRanks := map[int]struct{}{}
	for _, tier := range tiers {
		name := strings.TrimSpace(tier.Name)
		if name == "" {
			return fmt.Errorf("tier name is required")
		}
		if tier.Rank <= 0 {
			return fmt.Errorf("tier rank for %q must be a positive integer", name)
		}
		if _, exists := seenNames[name]; exists {
			return fmt.Errorf("duplicate tier name %q", name)
		}
		if _, exists := seenRanks[tier.Rank]; exists {
			return fmt.Errorf("duplicate tier rank %d", tier.Rank)
		}
		seenNames[name] = struct{}{}
		seenRanks[tier.Rank] = struct{}{}
	}
	return nil
}

func TierMountPoint(name string) string {
	return TierMountRoot + "/" + name
}

func (s *Store) CreateTierInstance(name string) error {
	return s.CreateTierPool(name, "xfs", nil)
}

func (s *Store) CreateTierPool(name, filesystem string, tiers []TierDefinition) error {
	return s.CreateTierPoolWithOptions(name, filesystem, tiers, false)
}

// CreateTierPoolWithOptions creates a tier pool. metaOnFastest controls
// whether PoolMetaStore will concentrate all metadata on the fastest tier
// (true) or spread it across every tier's .tierd-meta (false, default).
func (s *Store) CreateTierPoolWithOptions(name, filesystem string, tiers []TierDefinition, metaOnFastest bool) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	if filesystem == "" {
		filesystem = "xfs"
	}
	if len(tiers) == 0 {
		tiers = DefaultTierDefinitions()
	}
	if err := ValidateTierDefinitions(tiers); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	metaFlag := 0
	if metaOnFastest {
		metaFlag = 1
	}
	res, err := tx.Exec(
		`INSERT INTO tier_pools (name, filesystem, state, error_reason, last_reconciled_at, meta_on_fastest)
		 VALUES (?, ?, ?, NULL, NULL, ?)`,
		name, filesystem, TierPoolStateProvisioning, metaFlag,
	)
	if err != nil {
		return fmt.Errorf("create tier pool: %w", err)
	}
	poolID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("tier pool id: %w", err)
	}
	for _, slot := range tiers {
		if _, err := tx.Exec(
			`INSERT INTO tiers (pool_id, name, rank, state, array_id, pv_device)
			 VALUES (?, ?, ?, ?, NULL, NULL)`,
			poolID, slot.Name, slot.Rank, TierSlotStateEmpty,
		); err != nil {
			return fmt.Errorf("seed tier slot %s: %w", slot.Name, err)
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteTierInstance(name string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT id FROM tier_pools WHERE name = ?`, name).Scan(&poolID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier pool: %w", err)
	}

	// Explicitly delete tier slots in the same transaction before the pool row
	// so that an interrupt cannot leave orphaned rows (ON DELETE CASCADE would
	// also handle it, but explicit deletion guarantees atomicity regardless of
	// the SQLite foreign-key pragma setting).
	if _, err := tx.Exec(`DELETE FROM tiers WHERE pool_id = ?`, poolID); err != nil {
		return fmt.Errorf("delete tier slots: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM tier_pools WHERE id = ?`, poolID); err != nil {
		return fmt.Errorf("delete tier pool: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Clean up orphaned tiering control-plane rows that reference this pool
	// as their placement domain.
	return s.DeleteTierTargetsByPlacementDomain(name)
}

func scanTierInstance(row *sql.Row) (*TierInstance, error) {
	var t TierInstance
	var errorReason sql.NullString
	var reconciledAt sql.NullString
	var metaOnFastest int
	if err := row.Scan(
		&t.ID,
		&t.Name,
		&t.Filesystem,
		&t.State,
		&errorReason,
		&t.CreatedAt,
		&t.UpdatedAt,
		&reconciledAt,
		&metaOnFastest,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.MountPoint = TierMountPoint(t.Name)
	t.MetaOnFastest = metaOnFastest != 0
	if errorReason.Valid {
		t.ErrorReason = errorReason.String
	}
	if reconciledAt.Valid {
		t.LastReconciledAt = reconciledAt.String
	}
	return &t, nil
}

func (s *Store) GetTierInstance(name string) (*TierInstance, error) {
	if err := ValidateTierInstanceName(name); err != nil {
		return nil, err
	}
	t, err := scanTierInstance(s.db.QueryRow(
		`SELECT id, name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at, meta_on_fastest
		 FROM tier_pools
		 WHERE name = ?`,
		name,
	))
	if err != nil {
		if err == ErrNotFound {
			return nil, err
		}
		return nil, fmt.Errorf("get tier pool: %w", err)
	}
	return t, nil
}

func (s *Store) ListTierInstances() ([]TierInstance, error) {
	rows, err := s.db.Query(
		`SELECT id, name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at, meta_on_fastest
		 FROM tier_pools
		 ORDER BY created_at, name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list tier pools: %w", err)
	}
	defer rows.Close()

	var out []TierInstance
	for rows.Next() {
		var t TierInstance
		var errorReason sql.NullString
		var reconciledAt sql.NullString
		var metaOnFastest int
		if err := rows.Scan(
			&t.ID,
			&t.Name,
			&t.Filesystem,
			&t.State,
			&errorReason,
			&t.CreatedAt,
			&t.UpdatedAt,
			&reconciledAt,
			&metaOnFastest,
		); err != nil {
			return nil, fmt.Errorf("scan tier pool: %w", err)
		}
		t.MountPoint = TierMountPoint(t.Name)
		t.MetaOnFastest = metaOnFastest != 0
		if errorReason.Valid {
			t.ErrorReason = errorReason.String
		}
		if reconciledAt.Valid {
			t.LastReconciledAt = reconciledAt.String
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func findOrCreateArrayID(tx *sql.Tx, arrayPath string) (int64, error) {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO mdadm_arrays (path) VALUES (?)`, arrayPath); err != nil {
		return 0, fmt.Errorf("upsert mdadm array: %w", err)
	}
	var arrayID int64
	if err := tx.QueryRow(`SELECT id FROM mdadm_arrays WHERE path = ?`, arrayPath).Scan(&arrayID); err != nil {
		return 0, fmt.Errorf("lookup mdadm array: %w", err)
	}
	return arrayID, nil
}

func (s *Store) EnsureMDADMArray(arrayPath string) (int64, error) {
	arrayPath = NormalizeArrayPath(arrayPath)
	if arrayPath == "" {
		return 0, fmt.Errorf("array path required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	arrayID, err := findOrCreateArrayID(tx, arrayPath)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit array registry: %w", err)
	}
	return arrayID, nil
}

func (s *Store) GetMDADMArrayPath(arrayID int64) (string, error) {
	if arrayID <= 0 {
		return "", fmt.Errorf("array id must be positive")
	}

	var arrayPath string
	if err := s.db.QueryRow(`SELECT path FROM mdadm_arrays WHERE id = ?`, arrayID).Scan(&arrayPath); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get mdadm array: %w", err)
	}
	return arrayPath, nil
}

func scanTierSlot(row scanner) (*TierSlot, error) {
	var slot TierSlot
	var arrayID sql.NullInt64
	var arrayPath sql.NullString
	var pvDevice sql.NullString
	var backingRef sql.NullString
	if err := row.Scan(
		&slot.ID,
		&slot.PoolID,
		&slot.PoolName,
		&slot.Name,
		&slot.Rank,
		&slot.State,
		&arrayID,
		&arrayPath,
		&pvDevice,
		&slot.TargetFillPct,
		&slot.FullThresholdPct,
		&slot.BackingKind,
		&backingRef,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if arrayID.Valid {
		id := arrayID.Int64
		slot.ArrayID = &id
	}
	if arrayPath.Valid {
		slot.ArrayPath = arrayPath.String
	}
	if pvDevice.Valid {
		pv := pvDevice.String
		slot.PVDevice = &pv
	}
	if backingRef.Valid {
		slot.BackingRef = backingRef.String
	}
	if slot.BackingKind == "" {
		slot.BackingKind = "mdadm"
	}
	return &slot, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func (s *Store) GetTierSlot(poolName, tierName string) (*TierSlot, error) {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return nil, err
	}
	tierName = strings.TrimSpace(tierName)
	if tierName == "" {
		return nil, fmt.Errorf("tier name is required")
	}

	slot, err := scanTierSlot(s.db.QueryRow(
		`SELECT t.id, tp.id, tp.name, t.name, t.rank, t.state, t.array_id, ma.path, t.pv_device,
		        t.target_fill_pct, t.full_threshold_pct, t.backing_kind, t.backing_ref
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 LEFT JOIN mdadm_arrays ma ON ma.id = t.array_id
		 WHERE tp.name = ? AND t.name = ?`,
		poolName, tierName,
	))
	if err != nil {
		if err == ErrNotFound {
			return nil, err
		}
		return nil, fmt.Errorf("get tier slot: %w", err)
	}
	return slot, nil
}

func (s *Store) ListTierSlots(poolName string) ([]TierSlot, error) {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT t.id, tp.id, tp.name, t.name, t.rank, t.state, t.array_id, ma.path, t.pv_device,
		        t.target_fill_pct, t.full_threshold_pct, t.backing_kind, t.backing_ref
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 LEFT JOIN mdadm_arrays ma ON ma.id = t.array_id
		 WHERE tp.name = ?
		 ORDER BY t.rank`,
		poolName,
	)
	if err != nil {
		return nil, fmt.Errorf("list tier slots: %w", err)
	}
	defer rows.Close()

	var slots []TierSlot
	for rows.Next() {
		slot, err := scanTierSlot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tier slot: %w", err)
		}
		slots = append(slots, *slot)
	}
	return slots, rows.Err()
}

func (s *Store) AssignArrayToTier(poolName, tierName string, arrayID int64, arrayPath string) error {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return err
	}
	tierName = strings.TrimSpace(tierName)
	if tierName == "" {
		return fmt.Errorf("tier name is required")
	}
	if arrayID <= 0 {
		return fmt.Errorf("array id must be positive")
	}
	arrayPath = NormalizeArrayPath(arrayPath)
	if arrayPath == "" {
		return fmt.Errorf("array path required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT id FROM tier_pools WHERE name = ?`, poolName).Scan(&poolID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier pool: %w", err)
	}

	var currentState string
	if err := tx.QueryRow(
		`SELECT state FROM tiers WHERE pool_id = ? AND name = ?`,
		poolID, tierName,
	).Scan(&currentState); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier slot: %w", err)
	}
	if currentState != TierSlotStateEmpty {
		return fmt.Errorf("tier %s/%s is already in state %s", poolName, tierName, currentState)
	}

	var existingPoolName sql.NullString
	var existingTierName sql.NullString
	if err := tx.QueryRow(
		`SELECT tp.name, t.name
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 WHERE t.array_id = ?`,
		arrayID,
	).Scan(&existingPoolName, &existingTierName); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("check array assignment: %w", err)
	}
	if existingPoolName.Valid && existingTierName.Valid {
		return fmt.Errorf("array %s is already assigned to tier %s/%s", arrayPath, existingPoolName.String, existingTierName.String)
	}

	if _, err := tx.Exec(
		`UPDATE tiers
		 SET state = ?, array_id = ?, pv_device = ?,
		     backing_kind = 'mdadm', backing_ref = ?
		 WHERE pool_id = ? AND name = ?`,
		TierSlotStateAssigned, arrayID, arrayPath, arrayPath, poolID, tierName,
	); err != nil {
		return fmt.Errorf("assign tier slot: %w", err)
	}
	return tx.Commit()
}

// AssignBackingToTier assigns a non-mdadm backing (ZFS pool, btrfs device,
// bcachefs device) to a tier slot. array_id stays NULL since the backing
// isn't an mdadm array; backing_kind / backing_ref carry the identity.
// ref is interpreted per kind:
//
//	zfs      — the zpool name (tierd will create a dataset on it at provision)
//	btrfs    — path or label of the btrfs filesystem (reserved)
//	bcachefs — path or UUID (reserved)
func (s *Store) AssignBackingToTier(poolName, tierName, kind, ref string) error {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return err
	}
	tierName = strings.TrimSpace(tierName)
	if tierName == "" {
		return fmt.Errorf("tier name is required")
	}
	switch kind {
	case "zfs", "btrfs", "bcachefs":
		// accepted
	case "mdadm":
		return fmt.Errorf("use AssignArrayToTier for mdadm backings")
	default:
		return fmt.Errorf("unsupported backing kind %q", kind)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("backing ref required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT id FROM tier_pools WHERE name = ?`, poolName).Scan(&poolID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier pool: %w", err)
	}

	var currentState string
	if err := tx.QueryRow(
		`SELECT state FROM tiers WHERE pool_id = ? AND name = ?`,
		poolID, tierName,
	).Scan(&currentState); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier slot: %w", err)
	}
	if currentState != TierSlotStateEmpty {
		return fmt.Errorf("tier %s/%s is already in state %s", poolName, tierName, currentState)
	}

	// Refuse double-assignment of the same backing ref to another slot.
	var existingPoolName sql.NullString
	var existingTierName sql.NullString
	if err := tx.QueryRow(
		`SELECT tp.name, t.name
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 WHERE t.backing_kind = ? AND t.backing_ref = ?`,
		kind, ref,
	).Scan(&existingPoolName, &existingTierName); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("check backing assignment: %w", err)
	}
	if existingPoolName.Valid && existingTierName.Valid {
		return fmt.Errorf("%s %q is already assigned to tier %s/%s",
			kind, ref, existingPoolName.String, existingTierName.String)
	}

	if _, err := tx.Exec(
		`UPDATE tiers
		 SET state = ?, array_id = NULL, pv_device = NULL,
		     backing_kind = ?, backing_ref = ?
		 WHERE pool_id = ? AND name = ?`,
		TierSlotStateAssigned, kind, ref, poolID, tierName,
	); err != nil {
		return fmt.Errorf("assign tier slot: %w", err)
	}
	return tx.Commit()
}

func (s *Store) ClearTierAssignment(poolName, tierName string) error {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return err
	}
	tierName = strings.TrimSpace(tierName)
	if tierName == "" {
		return fmt.Errorf("tier name is required")
	}

	res, err := s.db.Exec(
		`UPDATE tiers
		 SET state = ?, array_id = NULL, pv_device = NULL,
		     backing_kind = 'mdadm', backing_ref = NULL
		 WHERE pool_id = (SELECT id FROM tier_pools WHERE name = ?)
		   AND name = ?`,
		TierSlotStateEmpty, poolName, tierName,
	)
	if err != nil {
		return fmt.Errorf("clear tier slot: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddArrayToTierSlot(tierName, slot, arrayPath string) error {
	if err := ValidateTierInstanceName(tierName); err != nil {
		return err
	}
	if !IsValidTierSlot(slot) {
		return fmt.Errorf("tier slot name is required")
	}
	arrayPath = NormalizeArrayPath(arrayPath)
	if arrayPath == "" {
		return fmt.Errorf("array path required")
	}

	arrayID, err := s.EnsureMDADMArray(arrayPath)
	if err != nil {
		return err
	}
	return s.AssignArrayToTier(tierName, slot, arrayID, arrayPath)
}

func (s *Store) GetTierAssignmentByArrayPath(arrayPath string) (*TierArrayAssignment, error) {
	arrayPath = NormalizeArrayPath(arrayPath)
	if arrayPath == "" {
		return nil, fmt.Errorf("array path required")
	}

	row := s.db.QueryRow(
		`SELECT t.id, tp.id, tp.name, t.name, t.rank, ma.path, t.pv_device, t.state
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 JOIN mdadm_arrays ma ON ma.id = t.array_id
		 WHERE ma.path = ?`,
		arrayPath,
	)
	var a TierArrayAssignment
	if err := row.Scan(&a.ID, &a.PoolID, &a.TierName, &a.Slot, &a.Rank, &a.ArrayPath, &a.PVDevice, &a.State); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get tier assignment: %w", err)
	}
	return &a, nil
}

func listAssignments(rows *sql.Rows) ([]TierArrayAssignment, error) {
	var out []TierArrayAssignment
	for rows.Next() {
		var a TierArrayAssignment
		if err := rows.Scan(&a.ID, &a.PoolID, &a.TierName, &a.Slot, &a.Rank, &a.ArrayPath, &a.PVDevice, &a.State); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListTierArrayAssignments() ([]TierArrayAssignment, error) {
	rows, err := s.db.Query(
		`SELECT t.id, tp.id, tp.name, t.name, t.rank, ma.path, t.pv_device, t.state
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 JOIN mdadm_arrays ma ON ma.id = t.array_id
		 WHERE t.array_id IS NOT NULL
		 ORDER BY tp.name, t.rank`,
	)
	if err != nil {
		return nil, fmt.Errorf("list tier assignments: %w", err)
	}
	defer rows.Close()
	out, err := listAssignments(rows)
	if err != nil {
		return nil, fmt.Errorf("scan tier assignments: %w", err)
	}
	return out, nil
}

func (s *Store) GetTierAssignments(tierName string) ([]TierArrayAssignment, error) {
	if err := ValidateTierInstanceName(tierName); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT t.id, tp.id, tp.name, t.name, t.rank, ma.path, t.pv_device, t.state
		 FROM tiers t
		 JOIN tier_pools tp ON tp.id = t.pool_id
		 JOIN mdadm_arrays ma ON ma.id = t.array_id
		 WHERE tp.name = ? AND t.array_id IS NOT NULL
		 ORDER BY t.rank`,
		tierName,
	)
	if err != nil {
		return nil, fmt.Errorf("get tier assignments: %w", err)
	}
	defer rows.Close()
	out, err := listAssignments(rows)
	if err != nil {
		return nil, fmt.Errorf("scan tier assignments: %w", err)
	}
	return out, nil
}

func (s *Store) TransitionTierInstanceState(name, next string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	if err := ValidateTierPoolState(next); err != nil {
		return err
	}

	current, err := s.GetTierInstance(name)
	if err != nil {
		return err
	}
	if current.State == next {
		return nil
	}
	if !canTransition(validTierPoolTransitions, current.State, next) {
		return fmt.Errorf("invalid tier state transition %q -> %q", current.State, next)
	}

	var errorReason any
	if next == TierPoolStateError {
		errorReason = "tier entered error state"
	}
	if _, err := s.db.Exec(
		`UPDATE tier_pools
		 SET state = ?, error_reason = ?, updated_at = datetime('now')
		 WHERE name = ?`,
		next, errorReason, name,
	); err != nil {
		return fmt.Errorf("update tier pool state: %w", err)
	}
	return nil
}

func (s *Store) SetTierInstanceError(name, reason string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("error reason is required")
	}

	current, err := s.GetTierInstance(name)
	if err != nil {
		return err
	}
	if current.State != TierPoolStateError && !canTransition(validTierPoolTransitions, current.State, TierPoolStateError) {
		return fmt.Errorf("invalid tier state transition %q -> %q", current.State, TierPoolStateError)
	}
	if _, err := s.db.Exec(
		`UPDATE tier_pools
		 SET state = ?, error_reason = ?, updated_at = datetime('now')
		 WHERE name = ?`,
		TierPoolStateError, reason, name,
	); err != nil {
		return fmt.Errorf("set tier pool error: %w", err)
	}
	return nil
}

func (s *Store) SetTierInstanceDestroyingReason(name, reason string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("error reason is required")
	}

	current, err := s.GetTierInstance(name)
	if err != nil {
		return err
	}
	if current.State != TierPoolStateDestroying {
		return fmt.Errorf("tier %s is in state %s, not %s", name, current.State, TierPoolStateDestroying)
	}
	if _, err := s.db.Exec(
		`UPDATE tier_pools
		 SET error_reason = ?, updated_at = datetime('now')
		 WHERE name = ?`,
		reason, name,
	); err != nil {
		return fmt.Errorf("set destroying reason: %w", err)
	}
	return nil
}

func (s *Store) MarkTierReconciled(name string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE tier_pools
		 SET last_reconciled_at = datetime('now'),
		     updated_at = datetime('now')
		 WHERE name = ?`,
		name,
	)
	if err != nil {
		return fmt.Errorf("mark tier reconciled: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTierSlotFill updates the target-fill and full-threshold percentages for a
// named tier slot within a pool. Values must be in the range [1, 100], and
// target_fill_pct may not exceed full_threshold_pct.
func (s *Store) SetTierSlotFill(poolName, slotName string, targetFillPct, fullThresholdPct int) error {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return err
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return fmt.Errorf("tier slot name is required")
	}
	if targetFillPct < 1 || targetFillPct > 100 {
		return fmt.Errorf("target_fill_pct must be between 1 and 100")
	}
	if fullThresholdPct < 1 || fullThresholdPct > 100 {
		return fmt.Errorf("full_threshold_pct must be between 1 and 100")
	}
	if targetFillPct > fullThresholdPct {
		return fmt.Errorf("target_fill_pct must be less than or equal to full_threshold_pct")
	}

	res, err := s.db.Exec(
		`UPDATE tiers
		 SET target_fill_pct = ?, full_threshold_pct = ?
		 WHERE pool_id = (SELECT id FROM tier_pools WHERE name = ?)
		   AND name = ?`,
		targetFillPct, fullThresholdPct, poolName, slotName,
	)
	if err != nil {
		return fmt.Errorf("set tier slot fill: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddTierSlot inserts a new tier slot (level) into an existing pool. rank must
// be unique within the pool and positive. targetFillPct must be less than or
// equal to fullThresholdPct; both must be in [1, 100].
func (s *Store) AddTierSlot(poolName, slotName string, rank, targetFillPct, fullThresholdPct int) error {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return err
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return fmt.Errorf("tier slot name is required")
	}
	if rank <= 0 {
		return fmt.Errorf("rank must be a positive integer")
	}
	if targetFillPct < 1 || targetFillPct > 100 {
		return fmt.Errorf("target_fill_pct must be between 1 and 100")
	}
	if fullThresholdPct < 1 || fullThresholdPct > 100 {
		return fmt.Errorf("full_threshold_pct must be between 1 and 100")
	}
	if targetFillPct > fullThresholdPct {
		return fmt.Errorf("target_fill_pct must be less than or equal to full_threshold_pct")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT id FROM tier_pools WHERE name = ?`, poolName).Scan(&poolID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier pool: %w", err)
	}

	var rankCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tiers WHERE pool_id = ? AND rank = ?`,
		poolID, rank,
	).Scan(&rankCount); err != nil {
		return fmt.Errorf("check rank uniqueness: %w", err)
	}
	if rankCount > 0 {
		return fmt.Errorf("a tier level with rank %d already exists in pool %s", rank, poolName)
	}

	if _, err := tx.Exec(
		`INSERT INTO tiers (pool_id, name, rank, state, array_id, pv_device, target_fill_pct, full_threshold_pct)
		 VALUES (?, ?, ?, ?, NULL, NULL, ?, ?)`,
		poolID, slotName, rank, TierSlotStateEmpty, targetFillPct, fullThresholdPct,
	); err != nil {
		return fmt.Errorf("insert tier slot: %w", err)
	}
	return tx.Commit()
}

// DeleteTierSlot removes a tier slot from a pool. Returns ErrTierSlotInUse if
// the slot has a PV currently assigned (state != empty). Returns ErrNotFound if
// the pool or slot does not exist.
func (s *Store) DeleteTierSlot(poolName, slotName string) error {
	if err := ValidateTierInstanceName(poolName); err != nil {
		return err
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return fmt.Errorf("tier slot name is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT id FROM tier_pools WHERE name = ?`, poolName).Scan(&poolID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier pool: %w", err)
	}

	var state string
	if err := tx.QueryRow(
		`SELECT state FROM tiers WHERE pool_id = ? AND name = ?`,
		poolID, slotName,
	).Scan(&state); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier slot: %w", err)
	}

	if state != TierSlotStateEmpty {
		return ErrTierSlotInUse
	}

	if _, err := tx.Exec(
		`DELETE FROM tiers WHERE pool_id = ? AND name = ?`,
		poolID, slotName,
	); err != nil {
		return fmt.Errorf("delete tier slot: %w", err)
	}
	return tx.Commit()
}

func (s *Store) TransitionTierSlotState(tierName, slot, next string) error {
	if err := ValidateTierInstanceName(tierName); err != nil {
		return err
	}
	if !IsValidTierSlot(slot) {
		return fmt.Errorf("tier slot name is required")
	}
	if err := ValidateTierSlotState(next); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT id FROM tier_pools WHERE name = ?`, tierName).Scan(&poolID); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier pool: %w", err)
	}

	var current string
	if err := tx.QueryRow(
		`SELECT state FROM tiers WHERE pool_id = ? AND name = ?`,
		poolID, slot,
	).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("get tier slot state: %w", err)
	}
	if current == next {
		return nil
	}
	if !canTransition(validTierSlotTransitions, current, next) {
		return fmt.Errorf("invalid tier slot state transition %q -> %q", current, next)
	}

	switch next {
	case TierSlotStateEmpty:
		if _, err := tx.Exec(
			`UPDATE tiers
			 SET state = ?, array_id = NULL, pv_device = NULL
			 WHERE pool_id = ? AND name = ?`,
			next, poolID, slot,
		); err != nil {
			return fmt.Errorf("clear tier slot: %w", err)
		}
	case TierSlotStateAssigned:
		if current == TierSlotStateEmpty {
			return fmt.Errorf("cannot transition empty tier slot %s to assigned without an array", slot)
		}
		if _, err := tx.Exec(
			`UPDATE tiers SET state = ? WHERE pool_id = ? AND name = ?`,
			next, poolID, slot,
		); err != nil {
			return fmt.Errorf("update tier slot state: %w", err)
		}
	default:
		if _, err := tx.Exec(
			`UPDATE tiers SET state = ? WHERE pool_id = ? AND name = ?`,
			next, poolID, slot,
		); err != nil {
			return fmt.Errorf("update tier slot state: %w", err)
		}
	}

	return tx.Commit()
}

// SetTierExpansionError records an error reason on a pool row without changing
// its state. Used for non-fatal failures (auto-expansion, unsafe-to-mount)
// where the LV remains functional at its current size.
func (s *Store) SetTierExpansionError(name, reason string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`UPDATE tier_pools SET error_reason = ?, updated_at = datetime('now') WHERE name = ?`,
		reason, name,
	); err != nil {
		return fmt.Errorf("set tier expansion error: %w", err)
	}
	return nil
}

// TouchTierPool bumps updated_at on the pool row without changing any other
// field. Used to signal that expansion completed successfully.
func (s *Store) TouchTierPool(name string) error {
	if err := ValidateTierInstanceName(name); err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`UPDATE tier_pools SET updated_at = datetime('now') WHERE name = ?`,
		name,
	); err != nil {
		return fmt.Errorf("touch tier pool: %w", err)
	}
	return nil
}
