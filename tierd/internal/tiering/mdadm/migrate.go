package mdadm

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// Migrate migrates existing mdadm tier state into the unified control-plane
// schema. It runs inside a single SQLite transaction; on any error the
// transaction is rolled back and the error is returned — the caller (main)
// should treat a non-nil error as fatal and exit before serving traffic.
//
// Migration is idempotent: re-running against an already-migrated database
// produces no duplicate rows. This is guaranteed by checking for an existing
// row with the same backing_ref before inserting.
//
// Migration covers:
//   - Each tier pool → one placement_domain (pool name as domain ID).
//   - Each assigned tier slot → one tier_target with backing_ref
//     "mdadm:{poolName}:{slotName}".
//   - Each managed_volume → one managed_namespace with backend_ref
//     "mdadm:{vgName}/{lvName}".
//   - Regions in in_progress migration state → one movement_job each.
//   - Terminal region migration states (done/failed) are discarded.
func Migrate(store *db.Store) error {
	tx, err := store.DB().Begin()
	if err != nil {
		return fmt.Errorf("mdadm migrate: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := migrateInTx(tx); err != nil {
		return fmt.Errorf("mdadm migrate: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mdadm migrate: commit: %w", err)
	}
	return nil
}

func migrateInTx(tx *sql.Tx) error {
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. Enumerate tier pools.
	poolRows, err := tx.Query(`
		SELECT id, name, state, error_reason
		FROM tier_pools
		ORDER BY name`)
	if err != nil {
		return fmt.Errorf("query tier_pools: %w", err)
	}
	defer poolRows.Close()

	type poolRow struct {
		id          int64
		name        string
		state       string
		errorReason sql.NullString
	}
	var pools []poolRow
	for poolRows.Next() {
		var p poolRow
		if err := poolRows.Scan(&p.id, &p.name, &p.state, &p.errorReason); err != nil {
			return fmt.Errorf("scan tier_pool: %w", err)
		}
		pools = append(pools, p)
	}
	if err := poolRows.Err(); err != nil {
		return fmt.Errorf("iterate tier_pools: %w", err)
	}

	for _, pool := range pools {
		// 2. Ensure placement_domain for the pool (INSERT OR IGNORE by name).
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO placement_domains
				(id, backend_kind, description, created_at, updated_at)
			VALUES (?, ?, '', ?, ?)`,
			pool.name, BackendKind, now, now,
		); err != nil {
			return fmt.Errorf("ensure placement_domain for pool %q: %w", pool.name, err)
		}

		// 3. Enumerate assigned tier slots for this pool.
		slotRows, err := tx.Query(`
			SELECT t.name, t.rank, t.state,
			       COALESCE(t.target_fill_pct, 0),
			       COALESCE(t.full_threshold_pct, 0)
			FROM tiers t
			WHERE t.pool_id = ? AND t.state != 'empty'
			ORDER BY t.rank`, pool.id)
		if err != nil {
			return fmt.Errorf("query tiers for pool %q: %w", pool.name, err)
		}
		defer slotRows.Close()

		type slotRow struct {
			name             string
			rank             int
			state            string
			targetFillPct    int
			fullThresholdPct int
		}
		var slots []slotRow
		for slotRows.Next() {
			var s slotRow
			if err := slotRows.Scan(&s.name, &s.rank, &s.state, &s.targetFillPct, &s.fullThresholdPct); err != nil {
				return fmt.Errorf("scan tier slot for pool %q: %w", pool.name, err)
			}
			slots = append(slots, s)
		}
		if err := slotRows.Err(); err != nil {
			return fmt.Errorf("iterate tiers for pool %q: %w", pool.name, err)
		}

		for _, slot := range slots {
			bref := backingRefTarget(pool.name, slot.name)

			// Check if already migrated.
			var existing string
			scanErr := tx.QueryRow(`
				SELECT id FROM tier_targets
				WHERE backing_ref = ? AND backend_kind = ?`,
				bref, BackendKind).Scan(&existing)
			if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
				return fmt.Errorf("check existing tier_target for %q: %w", bref, scanErr)
			}
			if existing != "" {
				continue // already migrated
			}

			// Generate a new control-plane ID.
			id, err := newMigrateID()
			if err != nil {
				return fmt.Errorf("generate id for tier_target %q: %w", bref, err)
			}

			health := slotHealthToUnified(slot.state)
			if _, err := tx.Exec(`
				INSERT INTO tier_targets
					(id, name, placement_domain, backend_kind, rank,
					 target_fill_pct, full_threshold_pct, policy_revision,
					 health, activity_band, activity_trend,
					 capabilities_json, backing_ref, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,1,?,?,?,?,?,?,?)`,
				id, slot.name, pool.name, BackendKind, slot.rank,
				slot.targetFillPct, slot.fullThresholdPct,
				health, "", "",
				capabilitiesJSON, bref, now, now,
			); err != nil {
				return fmt.Errorf("insert tier_target for %q: %w", bref, err)
			}
		}

		// 4. Enumerate managed volumes for this pool.
		volRows, err := tx.Query(`
			SELECT id, vg_name, lv_name, mount_point, size_bytes, pinned, status
			FROM managed_volumes
			WHERE pool_name = ?
			ORDER BY lv_name`, pool.name)
		if err != nil {
			return fmt.Errorf("query managed_volumes for pool %q: %w", pool.name, err)
		}
		defer volRows.Close()

		type volRow struct {
			id         int64
			vgName     string
			lvName     string
			mountPoint string
			sizeBytes  int64
			pinned     int
			status     string
		}
		var vols []volRow
		for volRows.Next() {
			var v volRow
			if err := volRows.Scan(&v.id, &v.vgName, &v.lvName, &v.mountPoint, &v.sizeBytes, &v.pinned, &v.status); err != nil {
				return fmt.Errorf("scan managed_volume for pool %q: %w", pool.name, err)
			}
			vols = append(vols, v)
		}
		if err := volRows.Err(); err != nil {
			return fmt.Errorf("iterate managed_volumes for pool %q: %w", pool.name, err)
		}

		for _, vol := range vols {
			bref := backingRefNamespace(vol.vgName, vol.lvName)

			// Check if already migrated.
			var existing string
			scanErr := tx.QueryRow(`
				SELECT id FROM managed_namespaces
				WHERE backend_ref = ? AND backend_kind = ?`,
				bref, BackendKind).Scan(&existing)
			if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
				return fmt.Errorf("check existing managed_namespace for %q: %w", bref, scanErr)
			}
			if existing != "" {
				continue // already migrated
			}

			id, err := newMigrateID()
			if err != nil {
				return fmt.Errorf("generate id for managed_namespace %q: %w", bref, err)
			}

			pinState := "none"
			if vol.pinned != 0 {
				pinState = "pinned-hot"
			}

			if _, err := tx.Exec(`
				INSERT INTO managed_namespaces
					(id, name, placement_domain, backend_kind, namespace_kind,
					 exposed_path, pin_state, intent_revision,
					 health, placement_state, backend_ref, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,1,?,?,?,?,?)`,
				id, vol.lvName, pool.name, BackendKind, "volume",
				vol.mountPoint, pinState,
				vol.status, "placed",
				bref, now, now,
			); err != nil {
				return fmt.Errorf("insert managed_namespace for %q: %w", bref, err)
			}

			// 5. Migrate in-progress region movements to movement_jobs.
			if err := migrateInProgressRegions(tx, vol.id, id, pool.name, BackendKind, now); err != nil {
				return fmt.Errorf("migrate in-progress regions for volume %d: %w", vol.id, err)
			}
		}
	}

	return nil
}

// migrateInProgressRegions creates movement_job rows for any
// managed_volume_regions that are currently in_progress. Terminal states
// (done/failed) and idle/queued regions are omitted.
func migrateInProgressRegions(tx *sql.Tx, volumeID int64, namespaceID, poolName, backendKind, now string) error {
	rows, err := tx.Query(`
		SELECT id, region_index, current_tier, migration_dest_tier,
		       migration_bytes_moved,
		       migration_triggered_by
		FROM managed_volume_regions
		WHERE volume_id = ? AND migration_state = 'in_progress'`,
		volumeID)
	if err != nil {
		return fmt.Errorf("query in-progress regions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			regionID        int64
			regionIndex     int
			currentTier     string
			destTierNull    sql.NullString
			bytesMoved      int64
			triggeredByNull sql.NullString
		)
		if err := rows.Scan(&regionID, &regionIndex, &currentTier, &destTierNull,
			&bytesMoved, &triggeredByNull); err != nil {
			return fmt.Errorf("scan in-progress region: %w", err)
		}

		// Resolve source and destination tier_target IDs from backing_refs.
		srcBref := backingRefTarget(poolName, currentTier)
		var srcTargetID string
		if err := tx.QueryRow(`
			SELECT id FROM tier_targets WHERE backing_ref = ? AND backend_kind = ?`,
			srcBref, backendKind).Scan(&srcTargetID); err != nil {
			// Source target not yet migrated or missing — skip this job.
			continue
		}

		if !destTierNull.Valid || destTierNull.String == "" {
			continue
		}
		dstBref := backingRefTarget(poolName, destTierNull.String)
		var dstTargetID string
		if err := tx.QueryRow(`
			SELECT id FROM tier_targets WHERE backing_ref = ? AND backend_kind = ?`,
			dstBref, backendKind).Scan(&dstTargetID); err != nil {
			continue
		}

		// Check if a movement_job for this region already exists.
		objectID := fmt.Sprintf("%d:%d", volumeID, regionIndex)
		var existingJob string
		scanErr := tx.QueryRow(`
			SELECT id FROM movement_jobs
			WHERE namespace_id = ? AND object_id = ?
			  AND state IN ('pending','running')`,
			namespaceID, objectID).Scan(&existingJob)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return fmt.Errorf("check existing movement_job for region %d: %w", regionIndex, scanErr)
		}
		if existingJob != "" {
			continue // already migrated
		}

		jobID, err := newMigrateID()
		if err != nil {
			return fmt.Errorf("generate id for movement_job: %w", err)
		}

		triggeredBy := ""
		if triggeredByNull.Valid {
			triggeredBy = triggeredByNull.String
		}

		if _, err := tx.Exec(`
			INSERT INTO movement_jobs
				(id, backend_kind, namespace_id, object_id, movement_unit,
				 placement_domain, source_target_id, dest_target_id,
				 policy_revision, intent_revision, planner_epoch,
				 state, triggered_by, progress_bytes, total_bytes,
				 failure_reason, started_at, updated_at, completed_at)
			VALUES (?,?,?,?,?,?,?,?,1,1,0,?,?,?,?,NULL,NULL,?,NULL)`,
			jobID, backendKind, namespaceID, objectID, "region",
			poolName, srcTargetID, dstTargetID,
			db.MovementJobStateRunning, triggeredBy,
			bytesMoved, 0, now,
		); err != nil {
			return fmt.Errorf("insert movement_job for region %d: %w", regionIndex, err)
		}
	}
	return rows.Err()
}

// newMigrateID generates a random 32-character hex ID for use during migration.
func newMigrateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate migrate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
