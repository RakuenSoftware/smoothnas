package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrCrossDomainMovement is returned when a movement job targets two tier
// targets that belong to different placement domains.
var ErrCrossDomainMovement = errors.New("movement job source and destination must be in the same placement domain")

// Movement job state constants.
const (
	MovementJobStatePending   = "pending"
	MovementJobStateRunning   = "running"
	MovementJobStateCompleted = "completed"
	MovementJobStateFailed    = "failed"
	MovementJobStateCancelled = "cancelled"
	MovementJobStateStale     = "stale"
)

// DegradedStateSeverity constants.
const (
	DegradedSeverityWarning  = "warning"
	DegradedSeverityCritical = "critical"
)

// NewControlPlaneID generates a random 32-character hex ID for control-plane rows.
// It is the exported equivalent of newControlPlaneID for use outside this package.
func NewControlPlaneID() (string, error) {
	return newControlPlaneID()
}

// newControlPlaneID generates a random 32-character hex ID for control-plane rows.
func newControlPlaneID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate control-plane id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// nowUTC returns the current time formatted as an RFC3339 string in UTC.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ---- Row types ---------------------------------------------------------------

// PlacementDomainRow is a row in the placement_domains table. Domains are
// created automatically when the first tier_target referencing them is
// registered and removed when no targets remain.
type PlacementDomainRow struct {
	ID          string
	BackendKind string
	Description string
	CreatedAt   string
	UpdatedAt   string
}

// TierTargetRow is a row in the tier_targets table.
type TierTargetRow struct {
	ID               string
	Name             string
	PlacementDomain  string
	BackendKind      string
	Rank             int
	TargetFillPct    int
	FullThresholdPct int
	PolicyRevision   int64
	Health           string
	ActivityBand     string
	ActivityTrend    string
	CapabilitiesJSON string
	BackingRef       string
	CreatedAt        string
	UpdatedAt        string
}

// ManagedNamespaceRow is a row in the managed_namespaces table.
type ManagedNamespaceRow struct {
	ID                  string
	Name                string
	PlacementDomain     string
	BackendKind         string
	NamespaceKind       string
	ExposedPath         string
	PinState            string
	IntentRevision      int64
	Health              string
	PlacementState      string
	BackendRef          string
	CapacityBytes       uint64
	UsedBytes           uint64
	PolicyTargetIDsJSON string
	CreatedAt           string
	UpdatedAt           string
}

// ManagedObjectRow is a row in the managed_objects table.
type ManagedObjectRow struct {
	ID                   string
	NamespaceID          string
	ObjectKind           string
	ObjectKey            string
	PinState             string
	ActivityBand         string
	PlacementSummaryJSON string
	BackendRef           string
	UpdatedAt            string
}

// MovementJobRow is a row in the movement_jobs table.
type MovementJobRow struct {
	ID              string
	BackendKind     string
	NamespaceID     string
	ObjectID        string
	MovementUnit    string
	PlacementDomain string
	SourceTargetID  string
	DestTargetID    string
	PolicyRevision  int64
	IntentRevision  int64
	PlannerEpoch    int64
	State           string
	TriggeredBy     string
	ProgressBytes   int64
	TotalBytes      int64
	FailureReason   string
	StartedAt       string
	UpdatedAt       string
	CompletedAt     string
}

// PlacementIntentRow is a row in the placement_intents table.
type PlacementIntentRow struct {
	ID               string
	NamespaceID      string
	ObjectID         string
	IntendedTargetID string
	PlacementDomain  string
	PolicyRevision   int64
	IntentRevision   int64
	Reason           string
	State            string
	UpdatedAt        string
}

// DegradedStateRow is a row in the degraded_states table.
type DegradedStateRow struct {
	ID          string
	BackendKind string
	ScopeKind   string
	ScopeID     string
	Severity    string
	Code        string
	Message     string
	UpdatedAt   string
	ResolvedAt  sql.NullString // set when the condition clears; purged after 7 days
}

// TieringHealthAlert is emitted by CheckTieringHealth when a monitoring
// threshold is breached.
type TieringHealthAlert struct {
	// Check is the name of the health check (e.g. "movement_queue_depth").
	Check   string
	Message string
}

// ---- placement_domains -------------------------------------------------------

// ListPlacementDomains returns all placement domains.
func (s *Store) ListPlacementDomains() ([]PlacementDomainRow, error) {
	rows, err := s.db.Query(`
		SELECT id, backend_kind, description, created_at, updated_at
		FROM placement_domains
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list placement domains: %w", err)
	}
	defer rows.Close()
	var out []PlacementDomainRow
	for rows.Next() {
		var d PlacementDomainRow
		if err := rows.Scan(&d.ID, &d.BackendKind, &d.Description, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan placement domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetPlacementDomain returns the placement domain with the given id.
func (s *Store) GetPlacementDomain(id string) (*PlacementDomainRow, error) {
	var d PlacementDomainRow
	err := s.db.QueryRow(`
		SELECT id, backend_kind, description, created_at, updated_at
		FROM placement_domains WHERE id = ?`, id).
		Scan(&d.ID, &d.BackendKind, &d.Description, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get placement domain %q: %w", id, err)
	}
	return &d, nil
}

// ensurePlacementDomain creates the domain if it does not already exist. Called
// automatically by CreateTierTarget.
func (s *Store) ensurePlacementDomain(id, backendKind string) error {
	now := nowUTC()
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO placement_domains (id, backend_kind, description, created_at, updated_at)
		VALUES (?, ?, '', ?, ?)`, id, backendKind, now, now)
	if err != nil {
		return fmt.Errorf("ensure placement domain %q: %w", id, err)
	}
	return nil
}

// pruneEmptyPlacementDomain removes the domain if no tier_targets reference it
// any longer. Called automatically by DeleteTierTarget.
func (s *Store) pruneEmptyPlacementDomain(id string) error {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM tier_targets WHERE placement_domain = ?`, id,
	).Scan(&count); err != nil {
		return fmt.Errorf("count targets in domain %q: %w", id, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM placement_domains WHERE id = ?`, id); err != nil {
		return fmt.Errorf("prune placement domain %q: %w", id, err)
	}
	return nil
}

// ---- tier_targets ------------------------------------------------------------

// CreateTierTarget inserts a new tier target row and auto-creates its placement
// domain if it does not already exist.
func (s *Store) CreateTierTarget(t *TierTargetRow) error {
	if t.ID == "" {
		id, err := newControlPlaneID()
		if err != nil {
			return err
		}
		t.ID = id
	}
	now := nowUTC()
	if t.CreatedAt == "" {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	t.PolicyRevision = 1

	if err := s.ensurePlacementDomain(t.PlacementDomain, t.BackendKind); err != nil {
		return err
	}

	_, err := s.db.Exec(`
		INSERT INTO tier_targets
			(id, name, placement_domain, backend_kind, rank,
			 target_fill_pct, full_threshold_pct, policy_revision,
			 health, activity_band, activity_trend,
			 capabilities_json, backing_ref, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.PlacementDomain, t.BackendKind, t.Rank,
		t.TargetFillPct, t.FullThresholdPct, t.PolicyRevision,
		t.Health, t.ActivityBand, t.ActivityTrend,
		t.CapabilitiesJSON, t.BackingRef, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create tier target: %w", err)
	}
	return nil
}

// GetTierTarget returns the tier target with the given id.
func (s *Store) GetTierTarget(id string) (*TierTargetRow, error) {
	var t TierTargetRow
	err := s.db.QueryRow(`
		SELECT id, name, placement_domain, backend_kind, rank,
		       target_fill_pct, full_threshold_pct, policy_revision,
		       health, activity_band, activity_trend,
		       capabilities_json, backing_ref, created_at, updated_at
		FROM tier_targets WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.PlacementDomain, &t.BackendKind, &t.Rank,
			&t.TargetFillPct, &t.FullThresholdPct, &t.PolicyRevision,
			&t.Health, &t.ActivityBand, &t.ActivityTrend,
			&t.CapabilitiesJSON, &t.BackingRef, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tier target %q: %w", id, err)
	}
	return &t, nil
}

// ListTierTargets returns all tier targets ordered by placement_domain, rank.
func (s *Store) ListTierTargets() ([]TierTargetRow, error) {
	rows, err := s.db.Query(`
		SELECT id, name, placement_domain, backend_kind, rank,
		       target_fill_pct, full_threshold_pct, policy_revision,
		       health, activity_band, activity_trend,
		       capabilities_json, backing_ref, created_at, updated_at
		FROM tier_targets
		ORDER BY placement_domain, rank`)
	if err != nil {
		return nil, fmt.Errorf("list tier targets: %w", err)
	}
	defer rows.Close()
	var out []TierTargetRow
	for rows.Next() {
		var t TierTargetRow
		if err := rows.Scan(
			&t.ID, &t.Name, &t.PlacementDomain, &t.BackendKind, &t.Rank,
			&t.TargetFillPct, &t.FullThresholdPct, &t.PolicyRevision,
			&t.Health, &t.ActivityBand, &t.ActivityTrend,
			&t.CapabilitiesJSON, &t.BackingRef, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan tier target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTierTargetPolicy updates the fill and threshold policy for a target,
// increments policy_revision, and marks any movement jobs in the target's
// placement domain as stale.
func (s *Store) UpdateTierTargetPolicy(id string, targetFillPct, fullThresholdPct int) error {
	t, err := s.GetTierTarget(id)
	if err != nil {
		return err
	}
	newRevision := t.PolicyRevision + 1
	_, err = s.db.Exec(`
		UPDATE tier_targets
		SET target_fill_pct = ?, full_threshold_pct = ?,
		    policy_revision = ?, updated_at = ?
		WHERE id = ?`,
		targetFillPct, fullThresholdPct, newRevision, nowUTC(), id)
	if err != nil {
		return fmt.Errorf("update tier target policy: %w", err)
	}
	return s.invalidateMovementJobsByDomainPolicy(t.PlacementDomain, newRevision)
}

// DeleteTierTarget removes a tier target and prunes its placement domain if
// it becomes empty.
func (s *Store) DeleteTierTarget(id string) error {
	t, err := s.GetTierTarget(id)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM tier_targets WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete tier target: %w", err)
	}
	return s.pruneEmptyPlacementDomain(t.PlacementDomain)
}

// DeleteTierTargetsByPlacementDomain removes all tier targets belonging to the
// given placement domain, then prunes the domain row itself.
func (s *Store) DeleteTierTargetsByPlacementDomain(domain string) error {
	if _, err := s.db.Exec(`DELETE FROM tier_targets WHERE placement_domain = ?`, domain); err != nil {
		return fmt.Errorf("delete tier targets for domain %q: %w", domain, err)
	}
	return s.pruneEmptyPlacementDomain(domain)
}

// ---- managed_namespaces ------------------------------------------------------

// CreateManagedNamespace inserts a new namespace row.
func (s *Store) CreateManagedNamespace(ns *ManagedNamespaceRow) error {
	if ns.ID == "" {
		id, err := newControlPlaneID()
		if err != nil {
			return err
		}
		ns.ID = id
	}
	now := nowUTC()
	if ns.CreatedAt == "" {
		ns.CreatedAt = now
	}
	ns.UpdatedAt = now
	ns.IntentRevision = 1

	if ns.PolicyTargetIDsJSON == "" {
		ns.PolicyTargetIDsJSON = "[]"
	}
	_, err := s.db.Exec(`
		INSERT INTO managed_namespaces
			(id, name, placement_domain, backend_kind, namespace_kind,
			 exposed_path, pin_state, intent_revision,
			 health, placement_state, backend_ref,
			 capacity_bytes, used_bytes, policy_target_ids_json,
			 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ns.ID, ns.Name, ns.PlacementDomain, ns.BackendKind, ns.NamespaceKind,
		ns.ExposedPath, ns.PinState, ns.IntentRevision,
		ns.Health, ns.PlacementState, ns.BackendRef,
		ns.CapacityBytes, ns.UsedBytes, ns.PolicyTargetIDsJSON,
		ns.CreatedAt, ns.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create managed namespace: %w", err)
	}
	return nil
}

// GetManagedNamespace returns the namespace with the given id.
func (s *Store) GetManagedNamespace(id string) (*ManagedNamespaceRow, error) {
	var ns ManagedNamespaceRow
	err := s.db.QueryRow(`
		SELECT id, name, placement_domain, backend_kind, namespace_kind,
		       exposed_path, pin_state, intent_revision,
		       health, placement_state, backend_ref,
		       capacity_bytes, used_bytes, policy_target_ids_json,
		       created_at, updated_at
		FROM managed_namespaces WHERE id = ?`, id).
		Scan(&ns.ID, &ns.Name, &ns.PlacementDomain, &ns.BackendKind, &ns.NamespaceKind,
			&ns.ExposedPath, &ns.PinState, &ns.IntentRevision,
			&ns.Health, &ns.PlacementState, &ns.BackendRef,
			&ns.CapacityBytes, &ns.UsedBytes, &ns.PolicyTargetIDsJSON,
			&ns.CreatedAt, &ns.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get managed namespace %q: %w", id, err)
	}
	return &ns, nil
}

// ListManagedNamespaces returns all managed namespaces.
func (s *Store) ListManagedNamespaces() ([]ManagedNamespaceRow, error) {
	rows, err := s.db.Query(`
		SELECT id, name, placement_domain, backend_kind, namespace_kind,
		       exposed_path, pin_state, intent_revision,
		       health, placement_state, backend_ref,
		       capacity_bytes, used_bytes, policy_target_ids_json,
		       created_at, updated_at
		FROM managed_namespaces
		ORDER BY placement_domain, name`)
	if err != nil {
		return nil, fmt.Errorf("list managed namespaces: %w", err)
	}
	defer rows.Close()
	var out []ManagedNamespaceRow
	for rows.Next() {
		var ns ManagedNamespaceRow
		if err := rows.Scan(
			&ns.ID, &ns.Name, &ns.PlacementDomain, &ns.BackendKind, &ns.NamespaceKind,
			&ns.ExposedPath, &ns.PinState, &ns.IntentRevision,
			&ns.Health, &ns.PlacementState, &ns.BackendRef,
			&ns.CapacityBytes, &ns.UsedBytes, &ns.PolicyTargetIDsJSON,
			&ns.CreatedAt, &ns.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan managed namespace: %w", err)
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// DeleteManagedNamespace removes a namespace and all its managed objects.
func (s *Store) DeleteManagedNamespace(id string) error {
	if _, err := s.GetManagedNamespace(id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM managed_namespaces WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete managed namespace: %w", err)
	}
	return nil
}

// DeleteManagedNamespacesByPlacementDomain removes all namespaces belonging
// to the given placement domain.
func (s *Store) DeleteManagedNamespacesByPlacementDomain(domain string) error {
	if _, err := s.db.Exec(`DELETE FROM managed_namespaces WHERE placement_domain = ?`, domain); err != nil {
		return fmt.Errorf("delete managed namespaces for domain %q: %w", domain, err)
	}
	return nil
}

// SetNamespacePinState updates the pin state for a namespace and increments
// intent_revision. Any movement jobs for this namespace are marked stale.
func (s *Store) SetNamespacePinState(id, pinState string) error {
	ns, err := s.GetManagedNamespace(id)
	if err != nil {
		return err
	}
	newRevision := ns.IntentRevision + 1
	_, err = s.db.Exec(`
		UPDATE managed_namespaces
		SET pin_state = ?, intent_revision = ?, updated_at = ?
		WHERE id = ?`, pinState, newRevision, nowUTC(), id)
	if err != nil {
		return fmt.Errorf("set namespace pin state: %w", err)
	}
	return s.invalidateMovementJobsByNamespaceIntent(id, newRevision)
}

// ---- managed_objects (DROPPED in migration 52) ------------------------------
//
// Per-file metadata moved to the per-pool meta store on each pool's fastest
// tier. These Store methods remain as no-ops so existing callers compile;
// mdadm no longer calls them on the hot path, and zfsmgd / the unused
// movement planner see empty results until they get their own meta store.

func (s *Store) CreateManagedObject(obj *ManagedObjectRow) error       { return nil }
func (s *Store) GetManagedObject(id string) (*ManagedObjectRow, error) { return nil, ErrNotFound }
func (s *Store) GetManagedObjectByKey(namespaceID, objectKey string) (*ManagedObjectRow, error) {
	return nil, ErrNotFound
}
func (s *Store) ListManagedObjects(namespaceID string) ([]ManagedObjectRow, error) { return nil, nil }
func (s *Store) DeleteManagedObjectByKey(namespaceID, objectKey string) error      { return nil }
func (s *Store) RenameObjectKey(namespaceID, oldKey, newKey string) error          { return nil }
func (s *Store) SetObjectPinState(id, pinState string) error                       { return ErrNotFound }
func (s *Store) UpdateManagedObjectPlacement(id, placementSummaryJSON string) error {
	return nil
}

// SetObjectRecallPending / GetObjectRecallPending were flags on
// managed_objects. The recall pathway is cold-tier specific and not used
// by any active backend, so these are no-ops post table drop.
func (s *Store) SetObjectRecallPending(id string, pending bool) error { return nil }
func (s *Store) GetObjectRecallPending(id string) (bool, error)       { return false, ErrNotFound }

// ---- movement_jobs -----------------------------------------------------------

// movement_jobs was dropped in migration 52. File-level migration
// orchestration will live on the per-pool meta store on the fastest tier
// when real planning comes back. Until then these are no-ops so existing
// schedulers and API handlers compile.

func (s *Store) CreateMovementJob(job *MovementJobRow) error       { return nil }
func (s *Store) GetMovementJob(id string) (*MovementJobRow, error) { return nil, ErrNotFound }
func (s *Store) ListMovementJobs() ([]MovementJobRow, error)       { return nil, nil }
func (s *Store) CancelMovementJob(id string) error                 { return ErrNotFound }
func (s *Store) invalidateMovementJobsByDomainPolicy(domain string, newPolicyRevision int64) error {
	return nil
}
func (s *Store) invalidateMovementJobsByNamespaceIntent(namespaceID string, newIntentRevision int64) error {
	return nil
}

// ---- placement_intents -------------------------------------------------------

// UpsertPlacementIntent inserts or replaces a placement intent.
func (s *Store) UpsertPlacementIntent(intent *PlacementIntentRow) error {
	if intent.ID == "" {
		id, err := newControlPlaneID()
		if err != nil {
			return err
		}
		intent.ID = id
	}
	intent.UpdatedAt = nowUTC()

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO placement_intents
			(id, namespace_id, object_id, intended_target_id, placement_domain,
			 policy_revision, intent_revision, reason, state, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		intent.ID, intent.NamespaceID, intent.ObjectID,
		intent.IntendedTargetID, intent.PlacementDomain,
		intent.PolicyRevision, intent.IntentRevision,
		intent.Reason, intent.State, intent.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert placement intent: %w", err)
	}
	return nil
}

// ListPlacementIntents returns all placement intents.
func (s *Store) ListPlacementIntents() ([]PlacementIntentRow, error) {
	rows, err := s.db.Query(`
		SELECT id, namespace_id, object_id, intended_target_id, placement_domain,
		       policy_revision, intent_revision, reason, state, updated_at
		FROM placement_intents
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list placement intents: %w", err)
	}
	defer rows.Close()
	var out []PlacementIntentRow
	for rows.Next() {
		var pi PlacementIntentRow
		var objectID sql.NullString
		if err := rows.Scan(
			&pi.ID, &pi.NamespaceID, &objectID, &pi.IntendedTargetID, &pi.PlacementDomain,
			&pi.PolicyRevision, &pi.IntentRevision, &pi.Reason, &pi.State, &pi.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan placement intent: %w", err)
		}
		pi.ObjectID = objectID.String
		out = append(out, pi)
	}
	return out, rows.Err()
}

// ---- degraded_states ---------------------------------------------------------

// UpsertDegradedState inserts or replaces a degraded-state signal.
func (s *Store) UpsertDegradedState(d *DegradedStateRow) error {
	if d.ID == "" {
		id, err := newControlPlaneID()
		if err != nil {
			return err
		}
		d.ID = id
	}
	d.UpdatedAt = nowUTC()

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO degraded_states
			(id, backend_kind, scope_kind, scope_id,
			 severity, code, message, updated_at, resolved_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		d.ID, d.BackendKind, d.ScopeKind, d.ScopeID,
		d.Severity, d.Code, d.Message, d.UpdatedAt, d.ResolvedAt)
	if err != nil {
		return fmt.Errorf("upsert degraded state: %w", err)
	}
	return nil
}

// ResolveDegradedState sets resolved_at on a degraded-state row, indicating
// the condition has cleared. Rows are retained until purged by the scheduler.
func (s *Store) ResolveDegradedState(id string) error {
	_, err := s.db.Exec(`
		UPDATE degraded_states
		SET resolved_at = ?, updated_at = ?
		WHERE id = ?`, nowUTC(), nowUTC(), id)
	if err != nil {
		return fmt.Errorf("resolve degraded state %q: %w", id, err)
	}
	return nil
}

// MarkRunningJobsFailed was a crash-recovery helper for movement_jobs; the
// table is gone, so this is a no-op until movement planning is rebuilt on
// the meta store.
func (s *Store) MarkRunningJobsFailed(backendKind, reason string) error { return nil }

// ListDegradedStates returns all active (unresolved) degraded-state signals.
func (s *Store) ListDegradedStates() ([]DegradedStateRow, error) {
	rows, err := s.db.Query(`
		SELECT id, backend_kind, scope_kind, scope_id,
		       severity, code, message, updated_at, resolved_at
		FROM degraded_states
		WHERE resolved_at IS NULL
		ORDER BY severity DESC, updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list degraded states: %w", err)
	}
	defer rows.Close()
	var out []DegradedStateRow
	for rows.Next() {
		var d DegradedStateRow
		if err := rows.Scan(
			&d.ID, &d.BackendKind, &d.ScopeKind, &d.ScopeID,
			&d.Severity, &d.Code, &d.Message, &d.UpdatedAt, &d.ResolvedAt,
		); err != nil {
			return nil, fmt.Errorf("scan degraded state: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PurgeResolvedDegradedStates removes degraded-state rows whose resolved_at is
// older than the given age.
func (s *Store) PurgeResolvedDegradedStates(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.db.Exec(`
		DELETE FROM degraded_states
		WHERE resolved_at IS NOT NULL AND resolved_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge resolved degraded states: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeTerminalMovementJobs was a maintenance helper; movement_jobs is gone
// as of migration 52.
func (s *Store) PurgeTerminalMovementJobs(olderThan time.Duration) (int64, error) {
	return 0, nil
}

// PurgeSatisfiedPlacementIntents removes placement_intent rows in the
// 'satisfied' state whose updated_at is older than the given age.
func (s *Store) PurgeSatisfiedPlacementIntents(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.db.Exec(`
		DELETE FROM placement_intents
		WHERE state = 'satisfied' AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge satisfied placement intents: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetControlPlaneConfig returns the value for the given configuration key.
// Returns "" if the key does not exist.
func (s *Store) GetControlPlaneConfig(key string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM control_plane_config WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get control_plane_config %q: %w", key, err)
	}
	return val, nil
}

// SetControlPlaneConfig upserts a key/value pair in control_plane_config.
func (s *Store) SetControlPlaneConfig(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO control_plane_config (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowUTC())
	if err != nil {
		return fmt.Errorf("set control_plane_config %q: %w", key, err)
	}
	return nil
}

// ---- health monitoring -------------------------------------------------------

// CheckTieringHealth queries the control-plane tables and returns any active
// health alerts based on the defined monitoring thresholds. The caller (e.g. the
// monitor goroutine) is responsible for logging or surfacing returned alerts.
//
// Thresholds used:
//
//	movementQueueDepthThreshold  – alert when > N pending jobs older than movementQueueAgeMinutes
//	maxJobAgeMinutes             – alert when a running job has not been updated for this long
//	failedMovementWindowMinutes  – sliding window for failed-movement-rate check
//	failedMovementThreshold      – alert when > N jobs failed within the window
func (s *Store) CheckTieringHealth(
	movementQueueDepthThreshold int,
	movementQueueAgeMinutes int,
	maxJobAgeMinutes int,
	failedMovementWindowMinutes int,
	failedMovementThreshold int,
) ([]TieringHealthAlert, error) {
	var alerts []TieringHealthAlert

	// movement_jobs was dropped in migration 52; the three movement-related
	// checks below are suppressed until planning comes back on the meta store.
	// The degraded-state check still runs because that table is still live.
	var criticalCount int
	if err := s.db.QueryRow(`
		SELECT COUNT(1) FROM degraded_states WHERE severity = ?`, DegradedSeverityCritical).Scan(&criticalCount); err != nil {
		return nil, fmt.Errorf("degraded state check: %w", err)
	}
	if criticalCount > 0 {
		alerts = append(alerts, TieringHealthAlert{
			Check:   "degraded_state_critical",
			Message: fmt.Sprintf("%d critical degraded-state signals are active", criticalCount),
		})
	}

	return alerts, nil
}

// RecordReconcileTimestamp records a successful reconciliation run in the
// tiering_reconcile_state table.
func (s *Store) RecordReconcileTimestamp() error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO tiering_reconcile_state (id, last_reconciled_at)
		VALUES (1, ?)`, nowUTC())
	if err != nil {
		return fmt.Errorf("record reconcile timestamp: %w", err)
	}
	return nil
}

// LastReconcileTimestamp returns the time of the last successful reconciliation,
// or a zero time if no reconciliation has ever run.
func (s *Store) LastReconcileTimestamp() (time.Time, error) {
	var ts sql.NullString
	err := s.db.QueryRow(`
		SELECT last_reconciled_at FROM tiering_reconcile_state WHERE id = 1`).Scan(&ts)
	if err == sql.ErrNoRows || !ts.Valid {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("last reconcile timestamp: %w", err)
	}
	t, err := time.Parse(time.RFC3339, ts.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse reconcile timestamp: %w", err)
	}
	return t, nil
}

// GetTierTargetByBackingRef returns the tier target with the given backing_ref
// and backend_kind. Returns ErrNotFound if no such target exists.
func (s *Store) GetTierTargetByBackingRef(backingRef, backendKind string) (*TierTargetRow, error) {
	var t TierTargetRow
	err := s.db.QueryRow(`
		SELECT id, name, placement_domain, backend_kind, rank,
		       target_fill_pct, full_threshold_pct, policy_revision,
		       health, activity_band, activity_trend,
		       capabilities_json, backing_ref, created_at, updated_at
		FROM tier_targets
		WHERE backing_ref = ? AND backend_kind = ?`, backingRef, backendKind).
		Scan(&t.ID, &t.Name, &t.PlacementDomain, &t.BackendKind, &t.Rank,
			&t.TargetFillPct, &t.FullThresholdPct, &t.PolicyRevision,
			&t.Health, &t.ActivityBand, &t.ActivityTrend,
			&t.CapabilitiesJSON, &t.BackingRef, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tier target by backing_ref %q: %w", backingRef, err)
	}
	return &t, nil
}

// UpdateTierTargetActivity updates the health, activity_band, and
// activity_trend columns for a tier target without incrementing the
// policy_revision.
func (s *Store) UpdateTierTargetActivity(id, health, activityBand, activityTrend string) error {
	_, err := s.db.Exec(`
		UPDATE tier_targets
		SET health = ?, activity_band = ?, activity_trend = ?, updated_at = ?
		WHERE id = ?`, health, activityBand, activityTrend, nowUTC(), id)
	if err != nil {
		return fmt.Errorf("update tier target activity %q: %w", id, err)
	}
	return nil
}

// GetManagedNamespaceByBackingRef returns the managed namespace with the given
// backend_ref and backend_kind. Returns ErrNotFound if no such namespace exists.
func (s *Store) GetManagedNamespaceByBackingRef(backingRef, backendKind string) (*ManagedNamespaceRow, error) {
	var ns ManagedNamespaceRow
	err := s.db.QueryRow(`
		SELECT id, name, placement_domain, backend_kind, namespace_kind,
		       exposed_path, pin_state, intent_revision,
		       health, placement_state, backend_ref,
		       capacity_bytes, used_bytes, policy_target_ids_json,
		       created_at, updated_at
		FROM managed_namespaces
		WHERE backend_ref = ? AND backend_kind = ?`, backingRef, backendKind).
		Scan(&ns.ID, &ns.Name, &ns.PlacementDomain, &ns.BackendKind, &ns.NamespaceKind,
			&ns.ExposedPath, &ns.PinState, &ns.IntentRevision,
			&ns.Health, &ns.PlacementState, &ns.BackendRef,
			&ns.CapacityBytes, &ns.UsedBytes, &ns.PolicyTargetIDsJSON,
			&ns.CreatedAt, &ns.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get managed namespace by backend_ref %q: %w", backingRef, err)
	}
	return &ns, nil
}

// UpdateMovementJobState was the scheduler's progress update. Table gone
// as of migration 52.
func (s *Store) UpdateMovementJobState(id, state string, progressBytes int64, failureReason string) error {
	return nil
}

// DeleteDegradedStatesByBackend removes all degraded-state signals for the
// given backend_kind. Used by adapters to refresh their degraded state on
// each reconcile cycle.
func (s *Store) DeleteDegradedStatesByBackend(backendKind string) error {
	_, err := s.db.Exec(`DELETE FROM degraded_states WHERE backend_kind = ?`, backendKind)
	if err != nil {
		return fmt.Errorf("delete degraded states for backend %q: %w", backendKind, err)
	}
	return nil
}

// CheckReconcileStaleness returns a TieringHealthAlert if the control plane
// has not reconciled within maxIntervalMinutes.
func (s *Store) CheckReconcileStaleness(maxIntervalMinutes int) (*TieringHealthAlert, error) {
	last, err := s.LastReconcileTimestamp()
	if err != nil {
		return nil, err
	}
	if last.IsZero() {
		return nil, nil // never reconciled; startup condition, not an alert
	}
	age := time.Since(last)
	if age > time.Duration(maxIntervalMinutes)*time.Minute {
		return &TieringHealthAlert{
			Check:   "reconciliation_staleness",
			Message: fmt.Sprintf("control-plane reconciliation has not run in %.0f minutes (max %d)", age.Minutes(), maxIntervalMinutes),
		}, nil
	}
	return nil, nil
}
