package db

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// migrations is an ordered list of SQL statements. Each entry runs once,
// tracked by its index in the schema_version table.
var migrations = []string{
	// 0: schema_version tracking table
	`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY
	)`,

	// 1: sessions (web UI login sessions, keyed by token)
	`CREATE TABLE sessions (
		token      TEXT    PRIMARY KEY,
		username   TEXT    NOT NULL,
		created_at TEXT    NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT    NOT NULL
	)`,

	// 2: rate limiting for failed logins
	`CREATE TABLE login_attempts (
		ip           TEXT    NOT NULL,
		attempted_at TEXT    NOT NULL DEFAULT (datetime('now'))
	)`,

	// 3: index for cleanup and lookup
	`CREATE INDEX idx_login_attempts_ip ON login_attempts(ip, attempted_at)`,

	// 4: appliance configuration (key-value)
	`CREATE TABLE config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,

	// 5: named tier instances. Each tier instance has a mount point that
	// always follows /mnt/{tier_name}.
	`CREATE TABLE IF NOT EXISTS tier_instances (
		name        TEXT PRIMARY KEY,
		mount_point TEXT NOT NULL UNIQUE,
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 6: one array per slot (NVME/SSD/HDD) for each tier instance, and each
	// mdadm array can only be assigned once globally.
	`CREATE TABLE IF NOT EXISTS tier_instance_arrays (
		tier_name  TEXT NOT NULL,
		slot       TEXT NOT NULL CHECK (slot IN ('NVME','SSD','HDD')),
		array_path TEXT NOT NULL UNIQUE,
		PRIMARY KEY (tier_name, slot),
		FOREIGN KEY (tier_name) REFERENCES tier_instances(name) ON DELETE CASCADE
	)`,

	// 7: persisted pool state machine for named tier instances.
	`ALTER TABLE tier_instances
		ADD COLUMN state TEXT NOT NULL DEFAULT 'healthy'
		CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying'))`,

	// 8: persisted slot state machine for assigned tier arrays.
	`ALTER TABLE tier_instance_arrays
		ADD COLUMN state TEXT NOT NULL DEFAULT 'assigned'
		CHECK (state IN ('assigned','degraded','missing'))`,

	// 9: registry of mdadm arrays so tier rows can refer to them by ID.
	`CREATE TABLE IF NOT EXISTS mdadm_arrays (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL UNIQUE
	)`,

	// 10: proposal-04 pool data model.
	`CREATE TABLE IF NOT EXISTS tier_pools (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		name               TEXT NOT NULL UNIQUE,
		filesystem         TEXT NOT NULL DEFAULT 'xfs' CHECK (filesystem IN ('xfs','ext4')),
		state              TEXT NOT NULL CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying')),
		error_reason       TEXT,
		created_at         TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
		last_reconciled_at TEXT,
		CHECK (
			(state = 'error' AND error_reason IS NOT NULL) OR
			(state != 'error' AND error_reason IS NULL)
		)
	)`,

	// 11: proposal-04 tier slot data model.
	`CREATE TABLE IF NOT EXISTS tiers (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		pool_id   INTEGER NOT NULL,
		name      TEXT NOT NULL,
		rank      INTEGER NOT NULL,
		state     TEXT NOT NULL CHECK (state IN ('empty','assigned','degraded','missing')),
		array_id  INTEGER UNIQUE,
		pv_device TEXT,
		FOREIGN KEY (pool_id) REFERENCES tier_pools(id) ON DELETE CASCADE,
		FOREIGN KEY (array_id) REFERENCES mdadm_arrays(id),
		UNIQUE (pool_id, name),
		UNIQUE (pool_id, rank),
		CHECK (
			(state = 'empty' AND array_id IS NULL AND pv_device IS NULL) OR
			(state != 'empty' AND array_id IS NOT NULL AND pv_device IS NOT NULL)
		)
	)`,

	// 12: seed mdadm array registry from the legacy assignment table.
	`INSERT OR IGNORE INTO mdadm_arrays (path)
	 SELECT DISTINCT array_path
	 FROM tier_instance_arrays`,

	// 13: migrate legacy tier instances into proposal-04 pools.
	`INSERT INTO tier_pools (name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at)
	 SELECT
	 	name,
	 	'xfs',
	 	state,
	 	CASE WHEN state = 'error' THEN 'legacy tier in error state' ELSE NULL END,
	 	created_at,
	 	created_at,
	 	NULL
	 FROM tier_instances`,

	// 14: seed fixed slot rows for every migrated pool.
	`INSERT INTO tiers (pool_id, name, rank, state, array_id, pv_device)
	 SELECT tier_pools.id, slots.name, slots.rank, 'empty', NULL, NULL
	 FROM tier_pools
	 CROSS JOIN (
	 	SELECT 'NVME' AS name, 1 AS rank
	 	UNION ALL SELECT 'SSD', 2
	 	UNION ALL SELECT 'HDD', 3
	 ) AS slots`,

	// 15: backfill assigned slot rows from the legacy assignment table.
	`UPDATE tiers
	 SET state = (
	 	SELECT tia.state
	 	FROM tier_instance_arrays tia
	 	JOIN tier_pools tp ON tp.name = tia.tier_name
	 	WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
	 ),
	     array_id = (
	     	SELECT ma.id
	     	FROM tier_instance_arrays tia
	     	JOIN tier_pools tp ON tp.name = tia.tier_name
	     	JOIN mdadm_arrays ma ON ma.path = tia.array_path
	     	WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
	     ),
	     pv_device = (
	     	SELECT tia.array_path
	     	FROM tier_instance_arrays tia
	     	JOIN tier_pools tp ON tp.name = tia.tier_name
	     	WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
	     )
	 WHERE EXISTS (
	 	SELECT 1
	 	FROM tier_instance_arrays tia
	 	JOIN tier_pools tp ON tp.name = tia.tier_name
	 	WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
	 )`,

	// 16: allow destroying pools to retain an error_reason after partial delete failures.
	`PRAGMA foreign_keys=OFF;
	 CREATE TABLE tier_pools_v2 (
	 	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	 	name               TEXT NOT NULL UNIQUE,
	 	filesystem         TEXT NOT NULL DEFAULT 'xfs' CHECK (filesystem IN ('xfs','ext4')),
	 	state              TEXT NOT NULL CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying')),
	 	error_reason       TEXT,
	 	created_at         TEXT NOT NULL DEFAULT (datetime('now')),
	 	updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
	 	last_reconciled_at TEXT,
	 	CHECK (
	 		(state = 'error' AND error_reason IS NOT NULL) OR
	 		(state = 'destroying') OR
	 		(state NOT IN ('error','destroying') AND error_reason IS NULL)
	 	)
	 );
	 INSERT INTO tier_pools_v2 (id, name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at)
	 SELECT id, name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at
	 FROM tier_pools;
	 DROP TABLE tier_pools;
	 ALTER TABLE tier_pools_v2 RENAME TO tier_pools;
	 PRAGMA foreign_keys=ON;`,

	// 17: drop ext4 support — xfs only. Convert any existing ext4 rows to xfs
	// (the LV will need to be re-provisioned, but the metadata must be valid).
	// The CREATE TABLE IF NOT EXISTS guards against the drifted-schema path
	// where tier_pools does not yet exist when this migration runs.
	`PRAGMA foreign_keys=OFF;
	 CREATE TABLE IF NOT EXISTS tier_pools (
	 	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	 	name               TEXT NOT NULL UNIQUE,
	 	filesystem         TEXT NOT NULL DEFAULT 'xfs',
	 	state              TEXT NOT NULL DEFAULT 'provisioning',
	 	error_reason       TEXT,
	 	created_at         TEXT NOT NULL DEFAULT (datetime('now')),
	 	updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
	 	last_reconciled_at TEXT
	 );
	 CREATE TABLE tier_pools_v3 (
	 	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	 	name               TEXT NOT NULL UNIQUE,
	 	filesystem         TEXT NOT NULL DEFAULT 'xfs' CHECK (filesystem IN ('xfs')),
	 	state              TEXT NOT NULL CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying')),
	 	error_reason       TEXT,
	 	created_at         TEXT NOT NULL DEFAULT (datetime('now')),
	 	updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
	 	last_reconciled_at TEXT,
	 	CHECK (
	 		(state = 'error' AND error_reason IS NOT NULL) OR
	 		(state = 'destroying') OR
	 		(state NOT IN ('error','destroying') AND error_reason IS NULL)
	 	)
	 );
	 INSERT INTO tier_pools_v3 (id, name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at)
	 SELECT id, name, 'xfs', state, error_reason, created_at, updated_at, last_reconciled_at
	 FROM tier_pools;
	 DROP TABLE tier_pools;
	 ALTER TABLE tier_pools_v3 RENAME TO tier_pools;
	 PRAGMA foreign_keys=ON;`,

	// 18: heat engine — add region_size_mb to tier pools (default 256 MiB).
	`ALTER TABLE tier_pools ADD COLUMN region_size_mb INTEGER NOT NULL DEFAULT 256`,

	// 19: heat engine — target_fill_pct added to tiers via repairTierSchema
	// so that the repair path (drifted schema_version) also gets the column.
	`SELECT 1`,

	// 20: heat engine — full_threshold_pct added to tiers via repairTierSchema
	// for the same reason.
	`SELECT 1`,

	// 21: heat engine — managed volumes: first-class LVs tracked per tier pool.
	`CREATE TABLE managed_volumes (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		pool_name   TEXT NOT NULL,
		vg_name     TEXT NOT NULL,
		lv_name     TEXT NOT NULL,
		mount_point TEXT NOT NULL,
		filesystem  TEXT NOT NULL DEFAULT 'xfs',
		size_bytes  INTEGER NOT NULL DEFAULT 0,
		pinned      INTEGER NOT NULL DEFAULT 0,
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
		FOREIGN KEY (pool_name) REFERENCES tier_pools(name) ON DELETE CASCADE,
		UNIQUE (vg_name, lv_name)
	)`,

	// 22: heat engine — managed volume regions: fixed-size chunks per managed LV.
	`CREATE TABLE managed_volume_regions (
		id                              INTEGER PRIMARY KEY AUTOINCREMENT,
		volume_id                       INTEGER NOT NULL,
		region_index                    INTEGER NOT NULL,
		region_offset_bytes             INTEGER NOT NULL,
		region_size_bytes               INTEGER NOT NULL,
		current_tier                    TEXT NOT NULL,
		intended_tier                   TEXT,
		spilled                         INTEGER NOT NULL DEFAULT 0,
		heat_score                      REAL NOT NULL DEFAULT 0,
		heat_sampled_at                 TEXT,
		consecutive_wrong_tier_cycles   INTEGER NOT NULL DEFAULT 0,
		migration_state                 TEXT NOT NULL DEFAULT 'idle',
		migration_triggered_by          TEXT,
		migration_dest_tier             TEXT,
		migration_bytes_moved           INTEGER NOT NULL DEFAULT 0,
		migration_bytes_total           INTEGER NOT NULL DEFAULT 0,
		migration_started_at            TEXT,
		migration_ended_at              TEXT,
		migration_failure_reason        TEXT,
		last_movement_reason            TEXT,
		last_movement_at                TEXT,
		FOREIGN KEY (volume_id) REFERENCES managed_volumes(id) ON DELETE CASCADE,
		UNIQUE (volume_id, region_index)
	)`,

	// 23: heat engine — global policy configuration (single-row enforced by CHECK).
	`CREATE TABLE tier_policy_config (
		id                                  INTEGER PRIMARY KEY CHECK (id = 1),
		poll_interval_minutes               INTEGER NOT NULL DEFAULT 5,
		rolling_window_hours                INTEGER NOT NULL DEFAULT 24,
		evaluation_interval_minutes         INTEGER NOT NULL DEFAULT 15,
		consecutive_cycles_before_migration INTEGER NOT NULL DEFAULT 3,
		migration_reserve_pct               INTEGER NOT NULL DEFAULT 10,
		migration_iops_cap_mb               INTEGER NOT NULL DEFAULT 100,
		migration_io_high_water_pct         INTEGER NOT NULL DEFAULT 80
	);
	INSERT INTO tier_policy_config (id) VALUES (1)`,

	// 24: heat engine — dm-stats region tracking for restart reconciliation.
	`CREATE TABLE dmstats_regions (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		volume_id         INTEGER NOT NULL,
		dmstats_region_id INTEGER NOT NULL,
		region_index      INTEGER NOT NULL,
		last_reads        INTEGER NOT NULL DEFAULT 0,
		last_writes       INTEGER NOT NULL DEFAULT 0,
		last_read_ios     INTEGER NOT NULL DEFAULT 0,
		last_write_ios    INTEGER NOT NULL DEFAULT 0,
		last_sampled_at   TEXT,
		FOREIGN KEY (volume_id) REFERENCES managed_volumes(id) ON DELETE CASCADE,
		UNIQUE (volume_id, dmstats_region_id)
	)`,

	// 25: heat engine — managed volume health status (tracked by startup
	// reconciliation). Values: 'healthy', 'degraded', 'missing'.
	`ALTER TABLE managed_volumes ADD COLUMN status TEXT NOT NULL DEFAULT 'healthy'`,

	// 26: unified-tiering-01 — placement domains. Rows are created automatically
	// when the first tier_target referencing a domain is registered and removed
	// when no targets remain.
	`CREATE TABLE IF NOT EXISTS placement_domains (
		id           TEXT PRIMARY KEY,
		backend_kind TEXT NOT NULL DEFAULT '',
		description  TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 27: unified-tiering-01 — unified tier targets (control-plane canonical).
	`CREATE TABLE IF NOT EXISTS tier_targets (
		id                TEXT PRIMARY KEY,
		name              TEXT NOT NULL,
		placement_domain  TEXT NOT NULL,
		backend_kind      TEXT NOT NULL,
		rank              INTEGER NOT NULL DEFAULT 1,
		target_fill_pct   INTEGER NOT NULL DEFAULT 50,
		full_threshold_pct INTEGER NOT NULL DEFAULT 95,
		policy_revision   INTEGER NOT NULL DEFAULT 1,
		health            TEXT NOT NULL DEFAULT 'unknown',
		activity_band     TEXT NOT NULL DEFAULT '',
		activity_trend    TEXT NOT NULL DEFAULT '',
		capabilities_json TEXT NOT NULL DEFAULT '{}',
		backing_ref       TEXT NOT NULL DEFAULT '',
		created_at        TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
		FOREIGN KEY (placement_domain) REFERENCES placement_domains(id)
	)`,

	// 28: unified-tiering-01 — managed namespaces.
	`CREATE TABLE IF NOT EXISTS managed_namespaces (
		id               TEXT PRIMARY KEY,
		name             TEXT NOT NULL,
		placement_domain TEXT NOT NULL,
		backend_kind     TEXT NOT NULL,
		namespace_kind   TEXT NOT NULL DEFAULT 'volume',
		exposed_path     TEXT NOT NULL DEFAULT '',
		pin_state        TEXT NOT NULL DEFAULT 'none',
		intent_revision  INTEGER NOT NULL DEFAULT 1,
		health           TEXT NOT NULL DEFAULT 'unknown',
		placement_state  TEXT NOT NULL DEFAULT 'unknown',
		backend_ref      TEXT NOT NULL DEFAULT '',
		created_at       TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 29: unified-tiering-01 — managed objects within namespaces.
	`CREATE TABLE IF NOT EXISTS managed_objects (
		id                    TEXT PRIMARY KEY,
		namespace_id          TEXT NOT NULL,
		object_kind           TEXT NOT NULL DEFAULT 'volume',
		object_key            TEXT NOT NULL DEFAULT '',
		pin_state             TEXT NOT NULL DEFAULT 'none',
		activity_band         TEXT NOT NULL DEFAULT '',
		placement_summary_json TEXT NOT NULL DEFAULT '{}',
		backend_ref           TEXT NOT NULL DEFAULT '',
		updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`,

	// 30: unified-tiering-01 — movement jobs.
	`CREATE TABLE IF NOT EXISTS movement_jobs (
		id               TEXT PRIMARY KEY,
		backend_kind     TEXT NOT NULL,
		namespace_id     TEXT NOT NULL,
		object_id        TEXT,
		movement_unit    TEXT NOT NULL DEFAULT '',
		placement_domain TEXT NOT NULL,
		source_target_id TEXT NOT NULL,
		dest_target_id   TEXT NOT NULL,
		policy_revision  INTEGER NOT NULL DEFAULT 1,
		intent_revision  INTEGER NOT NULL DEFAULT 1,
		planner_epoch    INTEGER NOT NULL DEFAULT 0,
		state            TEXT NOT NULL DEFAULT 'pending'
		                 CHECK (state IN ('pending','running','completed','failed','cancelled','stale')),
		triggered_by     TEXT NOT NULL DEFAULT '',
		progress_bytes   INTEGER NOT NULL DEFAULT 0,
		total_bytes      INTEGER NOT NULL DEFAULT 0,
		failure_reason   TEXT,
		started_at       TEXT,
		updated_at       TEXT NOT NULL DEFAULT (datetime('now')),
		completed_at     TEXT
	)`,

	// 31: unified-tiering-01 — placement intents.
	`CREATE TABLE IF NOT EXISTS placement_intents (
		id                TEXT PRIMARY KEY,
		namespace_id      TEXT NOT NULL,
		object_id         TEXT,
		intended_target_id TEXT NOT NULL,
		placement_domain  TEXT NOT NULL,
		policy_revision   INTEGER NOT NULL DEFAULT 1,
		intent_revision   INTEGER NOT NULL DEFAULT 1,
		reason            TEXT NOT NULL DEFAULT '',
		state             TEXT NOT NULL DEFAULT 'pending',
		updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 32: unified-tiering-01 — degraded-state signals from adapters.
	`CREATE TABLE IF NOT EXISTS degraded_states (
		id           TEXT PRIMARY KEY,
		backend_kind TEXT NOT NULL,
		scope_kind   TEXT NOT NULL DEFAULT '',
		scope_id     TEXT NOT NULL DEFAULT '',
		severity     TEXT NOT NULL DEFAULT 'warning' CHECK (severity IN ('warning','critical')),
		code         TEXT NOT NULL DEFAULT '',
		message      TEXT NOT NULL DEFAULT '',
		updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 33: unified-tiering-01 — reconciliation state (single-row, id=1).
	`CREATE TABLE IF NOT EXISTS tiering_reconcile_state (
		id                INTEGER PRIMARY KEY CHECK (id = 1),
		last_reconciled_at TEXT
	)`,

	// 34: unified-tiering-04 — ZFS managed adapter target registry.
	`CREATE TABLE IF NOT EXISTS zfs_managed_targets (
		tier_target_id  TEXT NOT NULL PRIMARY KEY,
		pool_name       TEXT NOT NULL,
		dataset_name    TEXT NOT NULL,
		dataset_path    TEXT NOT NULL,
		fuse_mode       TEXT NOT NULL DEFAULT 'unknown'
		                CHECK (fuse_mode IN ('passthrough','fallback','unknown')),
		FOREIGN KEY (tier_target_id) REFERENCES tier_targets(id) ON DELETE CASCADE
	)`,

	// 35: unified-tiering-04 — ZFS managed namespace daemon state.
	`CREATE TABLE IF NOT EXISTS zfs_managed_namespaces (
		namespace_id  TEXT NOT NULL PRIMARY KEY,
		pool_name     TEXT NOT NULL,
		meta_dataset  TEXT NOT NULL,
		socket_path   TEXT NOT NULL,
		mount_path    TEXT NOT NULL,
		daemon_pid    INTEGER,
		daemon_state  TEXT NOT NULL DEFAULT 'stopped'
		              CHECK (daemon_state IN ('stopped','starting','running','crashed')),
		fuse_mode     TEXT NOT NULL DEFAULT 'unknown'
		              CHECK (fuse_mode IN ('passthrough','fallback','unknown')),
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`,

	// 36: unified-tiering-04B — ZFS managed adapter movement log.
	// Tracks intermediate copy/verify/switch/cleanup state for crash recovery.
	// Rows are never deleted during normal operation; terminal states are
	// cleanup_complete and failed. State transitions are applied atomically
	// within SQLite transactions.
	`CREATE TABLE IF NOT EXISTS zfs_movement_log (
		id               TEXT PRIMARY KEY,
		object_id        TEXT NOT NULL,
		namespace_id     TEXT NOT NULL,
		source_target_id TEXT NOT NULL,
		dest_target_id   TEXT NOT NULL,
		object_key       TEXT NOT NULL DEFAULT '',
		state            TEXT NOT NULL
		                 CHECK (state IN ('copy_in_progress','copy_complete','switched','cleanup_complete','failed')),
		failure_reason   TEXT NOT NULL DEFAULT '',
		started_at       INTEGER NOT NULL DEFAULT 0,
		updated_at       INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`,

	// 37: unified-tiering-04B — recall_pending flag on managed objects.
	// Set to 1 when a synchronous recall is in flight for this object.
	// Movement workers poll this flag between copy chunks.
	`ALTER TABLE managed_objects ADD COLUMN recall_pending INTEGER NOT NULL DEFAULT 0`,

	// 38: unified-tiering-06 — add snapshot_mode to ZFS managed namespaces.
	`ALTER TABLE zfs_managed_namespaces ADD COLUMN snapshot_mode TEXT NOT NULL DEFAULT 'none'`,

	// 39: unified-tiering-06 — add snapshot_pool_name to ZFS managed namespaces.
	`ALTER TABLE zfs_managed_namespaces ADD COLUMN snapshot_pool_name TEXT NOT NULL DEFAULT ''`,

	// 40: unified-tiering-06 — add snapshot_quiesce_timeout_seconds to ZFS managed namespaces.
	`ALTER TABLE zfs_managed_namespaces ADD COLUMN snapshot_quiesce_timeout_seconds INTEGER NOT NULL DEFAULT 30`,

	// 41: unified-tiering-06 — coordinated namespace snapshot records.
	`CREATE TABLE IF NOT EXISTS zfs_managed_namespace_snapshots (
		id                     TEXT NOT NULL PRIMARY KEY,
		namespace_id           TEXT NOT NULL,
		pool_name              TEXT NOT NULL,
		zfs_snapshot_name      TEXT NOT NULL,
		backing_snapshots_json TEXT NOT NULL DEFAULT '[]',
		meta_snapshot_json     TEXT NOT NULL DEFAULT '{}',
		created_at             TEXT NOT NULL,
		consistency            TEXT NOT NULL DEFAULT 'atomic',
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`,

	// 42: index for snapshot listing ordered by created_at desc.
	`CREATE INDEX IF NOT EXISTS idx_zfs_ns_snapshots_ns_created
	 ON zfs_managed_namespace_snapshots(namespace_id, created_at DESC)`,

	// 43: performance — index for fast object lookup by (namespace_id, object_key).
	`CREATE INDEX IF NOT EXISTS idx_managed_objects_ns_key ON managed_objects (namespace_id, object_key)`,

	// 44: per-tier LV storage — mdadm managed adapter target registry.
	// Each row maps a unified tier_target to its per-tier VG/LV and mount path.
	`CREATE TABLE IF NOT EXISTS mdadm_managed_targets (
		tier_target_id TEXT PRIMARY KEY REFERENCES tier_targets(id) ON DELETE CASCADE,
		pool_name      TEXT NOT NULL,
		tier_name      TEXT NOT NULL,
		vg_name        TEXT NOT NULL,
		lv_name        TEXT NOT NULL DEFAULT 'data',
		mount_path     TEXT NOT NULL,
		UNIQUE (pool_name, tier_name)
	)`,

	// 45: per-tier LV storage — mdadm managed namespace daemon state.
	// Each row tracks the FUSE daemon for a namespace (pool).
	`CREATE TABLE IF NOT EXISTS mdadm_managed_namespaces (
		namespace_id    TEXT PRIMARY KEY REFERENCES managed_namespaces(id) ON DELETE CASCADE,
		pool_name       TEXT NOT NULL UNIQUE,
		socket_path     TEXT NOT NULL DEFAULT '',
		mount_path      TEXT NOT NULL,
		daemon_pid      INTEGER NOT NULL DEFAULT 0,
		daemon_state    TEXT NOT NULL DEFAULT 'stopped'
	)`,

	// 46: per-tier LV storage — mdadm movement log for crash recovery.
	`CREATE TABLE IF NOT EXISTS mdadm_movement_log (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		object_id        TEXT NOT NULL,
		namespace_id     TEXT NOT NULL,
		source_target_id TEXT NOT NULL,
		dest_target_id   TEXT NOT NULL,
		object_key       TEXT NOT NULL,
		state            TEXT NOT NULL DEFAULT 'copy_in_progress',
		failure_reason   TEXT,
		started_at       TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 47: unified-tiering-01 — capacity and policy-target fields on namespaces.
	`ALTER TABLE managed_namespaces ADD COLUMN capacity_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE managed_namespaces ADD COLUMN used_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE managed_namespaces ADD COLUMN policy_target_ids_json TEXT NOT NULL DEFAULT '[]'`,

	// 48: unified-tiering-01 — resolved_at on degraded_states for purge.
	`ALTER TABLE degraded_states ADD COLUMN resolved_at TEXT`,

	// 49: unified-tiering-01 — control-plane configuration key/value table.
	`CREATE TABLE IF NOT EXISTS control_plane_config (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// 50: unified-tiering-01 — seed control_plane_config defaults.
	`INSERT OR IGNORE INTO control_plane_config (key, value) VALUES
		('movement_queue_depth_warn',       '50'),
		('movement_queue_age_minutes',       '30'),
		('movement_failed_rate_window_minutes', '60'),
		('movement_failed_rate_max',         '10'),
		('reconciliation_max_age_minutes',   '60'),
		('planner_interval_minutes',         '15'),
		('reconcile_debounce_seconds',       '60'),
		('migration_io_high_water_pct',      '80'),
		('recall_timeout_seconds',           '300'),
		('movement_worker_concurrency',      '4')`,

	// 51: drop dead tables. managed_volume_regions was the legacy per-region
	// heat table; CollectActivity never populated it. dmstats_regions is a
	// restart-reconciliation cache rebuildable from `dmsetup status`.
	`DROP TABLE IF EXISTS managed_volume_regions;
	 DROP TABLE IF EXISTS dmstats_regions;`,

	// 52: drop managed_objects and movement_jobs. All per-file metadata
	// lives on the per-pool meta store on the fastest tier now. zfsmgd
	// will get its own meta store when ZFS-managed namespaces are
	// actually used; until then its planning/listing silently returns
	// empty. movement_jobs was written by the scheduler when PlanMovements
	// produced plans, but the mdadm planner already returns nil and no ZFS
	// namespaces exist, so the table has never held live data.
	`DROP TABLE IF EXISTS managed_objects;
	 DROP TABLE IF EXISTS movement_jobs;`,
}

// Migrate runs all pending migrations.
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(migrations[0]); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	var current int
	row := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version")
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for i := current + 1; i < len(migrations); i++ {
		log.Printf("running migration %d", i)
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
		if _, err := s.db.Exec("INSERT INTO schema_version (version) VALUES (?)", i); err != nil {
			return fmt.Errorf("record migration %d: %w", i, err)
		}
	}

	if err := s.repairTierSchema(); err != nil {
		return fmt.Errorf("repair tier schema: %w", err)
	}
	if err := s.repairTieringSchema(); err != nil {
		return fmt.Errorf("repair tiering schema: %w", err)
	}
	if err := s.repairZFSManagedSchema(); err != nil {
		return fmt.Errorf("repair zfs managed schema: %w", err)
	}

	return nil
}

func (s *Store) repairTierSchema() error {
	if err := s.ensureColumn("tier_instances", "state", `ALTER TABLE tier_instances
		ADD COLUMN state TEXT NOT NULL DEFAULT 'healthy'
		CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying'))`); err != nil {
		return err
	}
	if err := s.ensureColumn("tier_instance_arrays", "state", `ALTER TABLE tier_instance_arrays
		ADD COLUMN state TEXT NOT NULL DEFAULT 'assigned'
		CHECK (state IN ('assigned','degraded','missing'))`); err != nil {
		return err
	}
	if err := s.ensureColumn("tier_pools", "region_size_mb",
		`ALTER TABLE tier_pools ADD COLUMN region_size_mb INTEGER NOT NULL DEFAULT 256`); err != nil {
		return err
	}

	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS mdadm_arrays (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL UNIQUE
	)`); err != nil {
		return fmt.Errorf("ensure mdadm_arrays: %w", err)
	}

	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS managed_volumes (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		pool_name   TEXT NOT NULL,
		vg_name     TEXT NOT NULL,
		lv_name     TEXT NOT NULL,
		mount_point TEXT NOT NULL,
		filesystem  TEXT NOT NULL DEFAULT 'xfs',
		size_bytes  INTEGER NOT NULL DEFAULT 0,
		pinned      INTEGER NOT NULL DEFAULT 0,
		status      TEXT NOT NULL DEFAULT 'healthy',
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
		FOREIGN KEY (pool_name) REFERENCES tier_pools(name) ON DELETE CASCADE,
		UNIQUE (vg_name, lv_name)
	)`); err != nil {
		return fmt.Errorf("ensure managed_volumes: %w", err)
	}
	if err := s.ensureColumn("managed_volumes", "status",
		`ALTER TABLE managed_volumes ADD COLUMN status TEXT NOT NULL DEFAULT 'healthy'`); err != nil {
		return err
	}

	// managed_volume_regions is intentionally not recreated here — migration
	// 51 drops it and the code that populated it is gone.

	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tier_policy_config (
		id                                  INTEGER PRIMARY KEY CHECK (id = 1),
		poll_interval_minutes               INTEGER NOT NULL DEFAULT 5,
		rolling_window_hours                INTEGER NOT NULL DEFAULT 24,
		evaluation_interval_minutes         INTEGER NOT NULL DEFAULT 15,
		consecutive_cycles_before_migration INTEGER NOT NULL DEFAULT 3,
		migration_reserve_pct               INTEGER NOT NULL DEFAULT 10,
		migration_iops_cap_mb               INTEGER NOT NULL DEFAULT 100,
		migration_io_high_water_pct         INTEGER NOT NULL DEFAULT 80
	)`); err != nil {
		return fmt.Errorf("ensure tier_policy_config: %w", err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO tier_policy_config (id) VALUES (1)`); err != nil {
		return fmt.Errorf("seed tier_policy_config: %w", err)
	}

	// dmstats_regions is intentionally not recreated here — migration 51
	// drops it and the dm-stats state is rebuilt from the kernel at boot.

	if err := s.ensureTierPoolsTable(); err != nil {
		return err
	}

	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tiers (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		pool_id   INTEGER NOT NULL,
		name      TEXT NOT NULL,
		rank      INTEGER NOT NULL,
		state     TEXT NOT NULL CHECK (state IN ('empty','assigned','degraded','missing')),
		array_id  INTEGER UNIQUE,
		pv_device TEXT,
		FOREIGN KEY (pool_id) REFERENCES tier_pools(id) ON DELETE CASCADE,
		FOREIGN KEY (array_id) REFERENCES mdadm_arrays(id),
		UNIQUE (pool_id, name),
		UNIQUE (pool_id, rank),
		CHECK (
			(state = 'empty' AND array_id IS NULL AND pv_device IS NULL) OR
			(state != 'empty' AND array_id IS NOT NULL AND pv_device IS NOT NULL)
		)
	)`); err != nil {
		return fmt.Errorf("ensure tiers: %w", err)
	}

	// Add heat-engine columns to tiers after the table is guaranteed to exist.
	if err := s.ensureColumn("tiers", "target_fill_pct",
		`ALTER TABLE tiers ADD COLUMN target_fill_pct INTEGER NOT NULL DEFAULT 50`); err != nil {
		return err
	}
	if err := s.ensureColumn("tiers", "full_threshold_pct",
		`ALTER TABLE tiers ADD COLUMN full_threshold_pct INTEGER NOT NULL DEFAULT 95`); err != nil {
		return err
	}

	hasLegacyPools, err := s.tableExists("tier_instances")
	if err != nil {
		return err
	}
	hasLegacyAssignments, err := s.tableExists("tier_instance_arrays")
	if err != nil {
		return err
	}

	if hasLegacyAssignments {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO mdadm_arrays (path)
			SELECT DISTINCT array_path
			FROM tier_instance_arrays
			WHERE array_path IS NOT NULL AND array_path != ''`); err != nil {
			return fmt.Errorf("seed mdadm_arrays from legacy assignments: %w", err)
		}
	}

	if hasLegacyPools {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO tier_pools (name, filesystem, state, error_reason, created_at, updated_at, last_reconciled_at)
			SELECT
				name,
				'xfs',
				state,
				CASE WHEN state = 'error' THEN 'legacy tier in error state' ELSE NULL END,
				created_at,
				created_at,
				NULL
			FROM tier_instances`); err != nil {
			return fmt.Errorf("seed tier_pools from legacy tiers: %w", err)
		}
	}

	if _, err := s.db.Exec(`INSERT OR IGNORE INTO tiers (pool_id, name, rank, state, array_id, pv_device)
		SELECT tp.id, slots.name, slots.rank, 'empty', NULL, NULL
		FROM tier_pools tp
		CROSS JOIN (
			SELECT 'NVME' AS name, 1 AS rank
			UNION ALL SELECT 'SSD', 2
			UNION ALL SELECT 'HDD', 3
		) AS slots`); err != nil {
		return fmt.Errorf("seed tiers from pools: %w", err)
	}

	if hasLegacyPools && hasLegacyAssignments {
		if _, err := s.db.Exec(`UPDATE tiers
			SET state = (
				SELECT tia.state
				FROM tier_instance_arrays tia
				JOIN tier_pools tp ON tp.name = tia.tier_name
				WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
			),
			    array_id = (
			    	SELECT ma.id
			    	FROM tier_instance_arrays tia
			    	JOIN tier_pools tp ON tp.name = tia.tier_name
			    	JOIN mdadm_arrays ma ON ma.path = tia.array_path
			    	WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
			    ),
			    pv_device = (
			    	SELECT tia.array_path
			    	FROM tier_instance_arrays tia
			    	JOIN tier_pools tp ON tp.name = tia.tier_name
			    	WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
			    )
			WHERE EXISTS (
				SELECT 1
				FROM tier_instance_arrays tia
				JOIN tier_pools tp ON tp.name = tia.tier_name
				WHERE tp.id = tiers.pool_id AND tia.slot = tiers.name
			)`); err != nil {
			return fmt.Errorf("backfill tiers from legacy assignments: %w", err)
		}
	}

	return nil
}

// repairTieringSchema ensures all unified-tiering-01 tables exist on schema
// paths where the numbered migrations may not have run yet (e.g. drifted
// schema_version). Mirrors the pattern used by repairTierSchema.
func (s *Store) repairTieringSchema() error {
	ddls := []struct {
		table string
		sql   string
	}{
		{"placement_domains", `CREATE TABLE IF NOT EXISTS placement_domains (
			id           TEXT PRIMARY KEY,
			backend_kind TEXT NOT NULL DEFAULT '',
			description  TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
		{"tier_targets", `CREATE TABLE IF NOT EXISTS tier_targets (
			id                TEXT PRIMARY KEY,
			name              TEXT NOT NULL,
			placement_domain  TEXT NOT NULL,
			backend_kind      TEXT NOT NULL,
			rank              INTEGER NOT NULL DEFAULT 1,
			target_fill_pct   INTEGER NOT NULL DEFAULT 50,
			full_threshold_pct INTEGER NOT NULL DEFAULT 95,
			policy_revision   INTEGER NOT NULL DEFAULT 1,
			health            TEXT NOT NULL DEFAULT 'unknown',
			activity_band     TEXT NOT NULL DEFAULT '',
			activity_trend    TEXT NOT NULL DEFAULT '',
			capabilities_json TEXT NOT NULL DEFAULT '{}',
			backing_ref       TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
			FOREIGN KEY (placement_domain) REFERENCES placement_domains(id)
		)`},
		{"managed_namespaces", `CREATE TABLE IF NOT EXISTS managed_namespaces (
			id               TEXT PRIMARY KEY,
			name             TEXT NOT NULL,
			placement_domain TEXT NOT NULL,
			backend_kind     TEXT NOT NULL,
			namespace_kind   TEXT NOT NULL DEFAULT 'volume',
			exposed_path     TEXT NOT NULL DEFAULT '',
			pin_state        TEXT NOT NULL DEFAULT 'none',
			intent_revision  INTEGER NOT NULL DEFAULT 1,
			health           TEXT NOT NULL DEFAULT 'unknown',
			placement_state  TEXT NOT NULL DEFAULT 'unknown',
			backend_ref      TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
		// managed_objects and movement_jobs self-heal re-creates removed:
		// migration 52 drops them. Per-file metadata lives on the per-pool
		// meta store now; movement orchestration rebuilds when we add real
		// planning.
		{"placement_intents", `CREATE TABLE IF NOT EXISTS placement_intents (
			id                TEXT PRIMARY KEY,
			namespace_id      TEXT NOT NULL,
			object_id         TEXT,
			intended_target_id TEXT NOT NULL,
			placement_domain  TEXT NOT NULL,
			policy_revision   INTEGER NOT NULL DEFAULT 1,
			intent_revision   INTEGER NOT NULL DEFAULT 1,
			reason            TEXT NOT NULL DEFAULT '',
			state             TEXT NOT NULL DEFAULT 'pending',
			updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
		{"degraded_states", `CREATE TABLE IF NOT EXISTS degraded_states (
			id           TEXT PRIMARY KEY,
			backend_kind TEXT NOT NULL,
			scope_kind   TEXT NOT NULL DEFAULT '',
			scope_id     TEXT NOT NULL DEFAULT '',
			severity     TEXT NOT NULL DEFAULT 'warning' CHECK (severity IN ('warning','critical')),
			code         TEXT NOT NULL DEFAULT '',
			message      TEXT NOT NULL DEFAULT '',
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
		{"tiering_reconcile_state", `CREATE TABLE IF NOT EXISTS tiering_reconcile_state (
			id                INTEGER PRIMARY KEY CHECK (id = 1),
			last_reconciled_at TEXT
		)`},
		{"control_plane_config", `CREATE TABLE IF NOT EXISTS control_plane_config (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`},
	}
	for _, d := range ddls {
		if _, err := s.db.Exec(d.sql); err != nil {
			return fmt.Errorf("ensure %s: %w", d.table, err)
		}
	}
	// Ensure recall_pending column exists on managed_objects (added in migration 37).
	if err := s.ensureColumn("managed_objects", "recall_pending",
		`ALTER TABLE managed_objects ADD COLUMN recall_pending INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// Ensure capacity/policy columns on managed_namespaces (added in migration 47).
	if err := s.ensureColumn("managed_namespaces", "capacity_bytes",
		`ALTER TABLE managed_namespaces ADD COLUMN capacity_bytes INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("managed_namespaces", "used_bytes",
		`ALTER TABLE managed_namespaces ADD COLUMN used_bytes INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("managed_namespaces", "policy_target_ids_json",
		`ALTER TABLE managed_namespaces ADD COLUMN policy_target_ids_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	// Ensure resolved_at on degraded_states (added in migration 48).
	if err := s.ensureColumn("degraded_states", "resolved_at",
		`ALTER TABLE degraded_states ADD COLUMN resolved_at TEXT`); err != nil {
		return err
	}
	// Seed control_plane_config defaults (idempotent via INSERT OR IGNORE).
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO control_plane_config (key, value) VALUES
		('movement_queue_depth_warn',          '50'),
		('movement_queue_age_minutes',          '30'),
		('movement_failed_rate_window_minutes', '60'),
		('movement_failed_rate_max',            '10'),
		('reconciliation_max_age_minutes',      '60'),
		('planner_interval_minutes',            '15'),
		('reconcile_debounce_seconds',          '60'),
		('migration_io_high_water_pct',         '80'),
		('recall_timeout_seconds',              '300'),
		('movement_worker_concurrency',         '4')`); err != nil {
		return fmt.Errorf("seed control_plane_config: %w", err)
	}
	return nil
}

func (s *Store) repairZFSManagedSchema() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS zfs_managed_targets (
		tier_target_id  TEXT NOT NULL PRIMARY KEY,
		pool_name       TEXT NOT NULL,
		dataset_name    TEXT NOT NULL,
		dataset_path    TEXT NOT NULL,
		fuse_mode       TEXT NOT NULL DEFAULT 'unknown'
		                CHECK (fuse_mode IN ('passthrough','fallback','unknown')),
		FOREIGN KEY (tier_target_id) REFERENCES tier_targets(id) ON DELETE CASCADE
	)`); err != nil {
		return fmt.Errorf("ensure zfs_managed_targets: %w", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS zfs_managed_namespaces (
		namespace_id  TEXT NOT NULL PRIMARY KEY,
		pool_name     TEXT NOT NULL,
		meta_dataset  TEXT NOT NULL,
		socket_path   TEXT NOT NULL,
		mount_path    TEXT NOT NULL,
		daemon_pid    INTEGER,
		daemon_state  TEXT NOT NULL DEFAULT 'stopped'
		              CHECK (daemon_state IN ('stopped','starting','running','crashed')),
		fuse_mode     TEXT NOT NULL DEFAULT 'unknown'
		              CHECK (fuse_mode IN ('passthrough','fallback','unknown')),
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`); err != nil {
		return fmt.Errorf("ensure zfs_managed_namespaces: %w", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS zfs_movement_log (
		id               TEXT PRIMARY KEY,
		object_id        TEXT NOT NULL,
		namespace_id     TEXT NOT NULL,
		source_target_id TEXT NOT NULL,
		dest_target_id   TEXT NOT NULL,
		object_key       TEXT NOT NULL DEFAULT '',
		state            TEXT NOT NULL
		                 CHECK (state IN ('copy_in_progress','copy_complete','switched','cleanup_complete','failed')),
		failure_reason   TEXT NOT NULL DEFAULT '',
		started_at       INTEGER NOT NULL DEFAULT 0,
		updated_at       INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`); err != nil {
		return fmt.Errorf("ensure zfs_movement_log: %w", err)
	}
	// Proposal 06 columns — idempotent via ensureColumn.
	if err := s.ensureColumn("zfs_managed_namespaces", "snapshot_mode",
		`ALTER TABLE zfs_managed_namespaces ADD COLUMN snapshot_mode TEXT NOT NULL DEFAULT 'none'`); err != nil {
		return err
	}
	if err := s.ensureColumn("zfs_managed_namespaces", "snapshot_pool_name",
		`ALTER TABLE zfs_managed_namespaces ADD COLUMN snapshot_pool_name TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn("zfs_managed_namespaces", "snapshot_quiesce_timeout_seconds",
		`ALTER TABLE zfs_managed_namespaces ADD COLUMN snapshot_quiesce_timeout_seconds INTEGER NOT NULL DEFAULT 30`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS zfs_managed_namespace_snapshots (
		id                     TEXT NOT NULL PRIMARY KEY,
		namespace_id           TEXT NOT NULL,
		pool_name              TEXT NOT NULL,
		zfs_snapshot_name      TEXT NOT NULL,
		backing_snapshots_json TEXT NOT NULL DEFAULT '[]',
		meta_snapshot_json     TEXT NOT NULL DEFAULT '{}',
		created_at             TEXT NOT NULL,
		consistency            TEXT NOT NULL DEFAULT 'atomic',
		FOREIGN KEY (namespace_id) REFERENCES managed_namespaces(id) ON DELETE CASCADE
	)`); err != nil {
		return fmt.Errorf("ensure zfs_managed_namespace_snapshots: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_zfs_ns_snapshots_ns_created
		ON zfs_managed_namespace_snapshots(namespace_id, created_at DESC)`); err != nil {
		return fmt.Errorf("ensure zfs snapshot index: %w", err)
	}
	return nil
}

func (s *Store) ensureTierPoolsTable() error {
	exists, err := s.tableExists("tier_pools")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tier_pools (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			name               TEXT NOT NULL UNIQUE,
			filesystem         TEXT NOT NULL DEFAULT 'xfs' CHECK (filesystem IN ('xfs')),
			state              TEXT NOT NULL CHECK (state IN ('provisioning','healthy','degraded','unmounted','error','destroying')),
			error_reason       TEXT,
			created_at         TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
			last_reconciled_at TEXT,
			CHECK (
				(state = 'error' AND error_reason IS NOT NULL) OR
				(state = 'destroying') OR
				(state NOT IN ('error','destroying') AND error_reason IS NULL)
			)
		)`); err != nil {
			return fmt.Errorf("ensure tier_pools: %w", err)
		}
		return nil
	}

	sqlDef, err := s.tableSQL("tier_pools")
	if err != nil {
		return err
	}
	if strings.Contains(sqlDef, "(state = 'destroying')") {
		return nil
	}

	log.Printf("repairing legacy tier_pools constraint")
	if _, err := s.db.Exec(migrations[16]); err != nil {
		return fmt.Errorf("upgrade tier_pools constraint: %w", err)
	}
	return nil
}

func (s *Store) ensureColumn(table, column, ddl string) error {
	exists, err := s.tableExists(table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	hasColumn, err := s.columnExists(table, column)
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}

	log.Printf("repairing missing column %s.%s", table, column)
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) tableExists(name string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return count > 0, nil
}

func (s *Store) tableSQL(name string) (string, error) {
	var sqlDef sql.NullString
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name,
	).Scan(&sqlDef); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get table sql for %s: %w", name, err)
	}
	return sqlDef.String, nil
}

func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("check columns for %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan columns for %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate columns for %s: %w", table, err)
	}
	return false, nil
}
