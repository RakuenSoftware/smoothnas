package plugin

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// Plugin states. Per-instance states match StateInstalled..StateFailed.
// The aggregate `plugins.state` column also accepts StateDegraded (used
// when some instances are running and others are failed).
const (
	StateInstalled = "installed"
	StatePulling   = "pulling"
	StateCreating  = "creating"
	StateStarting  = "starting"
	StateRunning   = "running"
	StateStopped   = "stopped"
	StateFailed    = "failed"
	StateDegraded  = "degraded"
)

// Store is the persistence layer for the plugin subsystem. It wraps
// the shared db.Store and exposes plugin-shaped CRUD on the six
// tables introduced in migration 00011_plugins.sql.
type Store struct {
	store *db.Store
	db    *sql.DB
}

// NewStore constructs a plugin.Store from the shared db.Store.
// The db.Store is the source of truth for the connection; we keep
// a *sql.DB reference for transactional work.
func NewStore(s *db.Store) *Store {
	return &Store{store: s, db: s.DB()}
}

// PluginRow is the in-memory image of one row in the `plugins` table
// joined with derived fields. Used by Get / List.
type PluginRow struct {
	Name                 string
	Version              string
	State                string
	ManifestYAML         string
	ArtifactType         string
	ImageRef             string
	DistroSummary        string
	InstanceCount        int
	InstanceConfigurable bool
	InstalledAt          string
	UpdatedAt            string
}

// InstanceRow is the in-memory image of one plugin_instances row.
type InstanceRow struct {
	PluginName  string
	Instance    int
	ContainerID string
	State       string
	BridgeIP    string
	LastChange  string
	LastError   string
}

// VolumeRow is the in-memory image of one plugin_volumes row plus
// its expanded per-instance host paths joined from
// plugin_volume_paths. Paths is keyed by instance number (1..N).
type VolumeRow struct {
	PluginName  string
	Name        string
	Mode        string
	Slot        string
	TierPool    string
	PerInstance bool
	BindPath    string
	Paths       map[int]string
}

// PortRow is the in-memory image of one plugin_ports row.
type PortRow struct {
	PluginName    string
	Name          string
	ContainerPort int
	Protocol      string
	Expose        bool
	HostExpose    bool
}

// ConfigRow is the in-memory image of one plugin_config row.
type ConfigRow struct {
	PluginName string
	Key        string
	Value      string
}

// PluginRecord bundles every table's view of a single plugin into
// one structure, the shape callers usually want.
type PluginRecord struct {
	Plugin    PluginRow
	Instances []InstanceRow
	Volumes   []VolumeRow
	Ports     []PortRow
	Config    []ConfigRow
}

// InsertParams is the input to Insert: the validated manifest plus
// the resolved per-instance host paths for each volume. install.go
// fills this in.
type InsertParams struct {
	Manifest *Manifest
	// Paths is keyed by (volume name) → (instance number → host path).
	// Tier-bound volumes whose host path has not yet been resolved
	// (phase 03 territory) MUST still appear in this map with their
	// instance entries set to "" (empty string). The plugin_volumes
	// row's tier_pool gets the sentinel "<unresolved>".
	Paths map[string]map[int]string
}

// Insert persists a plugin atomically across all six tables. Returns
// ErrPluginExists if a plugin with this name is already installed.
func (s *Store) Insert(p InsertParams) error {
	if p.Manifest == nil {
		return fmt.Errorf("plugin.Insert: nil manifest")
	}
	m := p.Manifest

	// Marshal the original manifest YAML for storage. We could
	// reserialise from the struct but operators sometimes care about
	// the exact bytes they uploaded (comments, ordering); install.go
	// passes the raw input through via a side channel below. For now
	// we store the parsed struct's re-render via fmt — install.go
	// stamps the real bytes immediately after Insert returns. The
	// schema requires a non-empty value, so a placeholder is fine.
	manifestText := fmt.Sprintf("# parsed manifest for %s@%s\n", m.Metadata.Name, m.Metadata.Version)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort on error path

	imageRef := ""
	distro := m.DistroSummary()
	switch m.Artifact.Type {
	case ArtifactOCIImage:
		// Phase 02 will resolve a digest and replace this with the
		// fully-qualified image@sha256:... ref. Phase 1 records the
		// manifest's pre-resolution form so the operator can see it
		// in `plugin show`.
		imageRef = m.Artifact.Image
		if m.Artifact.Digest != "" {
			imageRef = m.Artifact.Image + "@" + m.Artifact.Digest
		}
	}

	count := m.EffectiveCount()
	configurable := 0
	if m.Instances.Configurable {
		configurable = 1
	}

	res, err := tx.Exec(
		`INSERT INTO plugins
		 (name, version, state, manifest, artifact_type,
		  image_ref, distro_summary,
		  instance_count, instance_configurable)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Metadata.Name, m.Metadata.Version, StateInstalled, manifestText,
		m.Artifact.Type, sqlNullable(imageRef), sqlNullable(distro),
		count, configurable,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrPluginExists
		}
		return fmt.Errorf("insert plugins: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("insert plugins: expected 1 row, got %d", n)
	}

	// One plugin_instances row per replica.
	for i := 1; i <= count; i++ {
		if _, err := tx.Exec(
			`INSERT INTO plugin_instances
			 (plugin_name, instance, state)
			 VALUES (?, ?, ?)`,
			m.Metadata.Name, i, StateInstalled,
		); err != nil {
			return fmt.Errorf("insert plugin_instances[%d]: %w", i, err)
		}
	}

	// Volumes + their per-instance paths.
	for _, vol := range m.Volumes {
		perInst := 0
		if vol.PerInstance {
			perInst = 1
		}
		var tierPool sql.NullString
		var slot sql.NullString
		if vol.Mode == VolumeModeTierBound {
			slot = sql.NullString{String: vol.Slot, Valid: vol.Slot != ""}
			// Phase 03 fills in the real tier pool. Phase 1 leaves
			// it as a sentinel so callers can tell "not yet resolved"
			// from "empty by design".
			tierPool = sql.NullString{String: "<unresolved>", Valid: true}
		}
		if _, err := tx.Exec(
			`INSERT INTO plugin_volumes
			 (plugin_name, volume_name, mode, slot, tier_pool,
			  per_instance, bind_path)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.Metadata.Name, vol.Name, vol.Mode, slot, tierPool,
			perInst, vol.Bind,
		); err != nil {
			return fmt.Errorf("insert plugin_volumes[%s]: %w", vol.Name, err)
		}

		paths := p.Paths[vol.Name]
		if paths == nil {
			return fmt.Errorf("plugin.Insert: missing host paths for volume %q", vol.Name)
		}
		// Shared volumes always have one entry at instance=1; per-instance
		// volumes have N entries 1..count.
		var expected int
		if vol.PerInstance {
			expected = count
		} else {
			expected = 1
		}
		if len(paths) != expected {
			return fmt.Errorf("plugin.Insert: volume %q expected %d path(s), got %d", vol.Name, expected, len(paths))
		}
		for inst, host := range paths {
			if _, err := tx.Exec(
				`INSERT INTO plugin_volume_paths
				 (plugin_name, volume_name, instance, host_path)
				 VALUES (?, ?, ?, ?)`,
				m.Metadata.Name, vol.Name, inst, host,
			); err != nil {
				return fmt.Errorf("insert plugin_volume_paths[%s/%d]: %w", vol.Name, inst, err)
			}
		}
	}

	// Ports.
	for _, port := range m.Ports {
		expose := 0
		if port.Expose {
			expose = 1
		}
		hostExpose := 0
		if port.HostExpose {
			hostExpose = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO plugin_ports
			 (plugin_name, port_name, container_port, protocol,
			  expose, host_expose)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			m.Metadata.Name, port.Name, port.Port, port.Protocol,
			expose, hostExpose,
		); err != nil {
			return fmt.Errorf("insert plugin_ports[%s]: %w", port.Name, err)
		}
	}

	// Config defaults. Operators override these post-install via
	// PUT /api/plugins/<name>/config (phase 06); phase 1 just lays
	// down the defaults.
	for _, f := range m.Config {
		if _, err := tx.Exec(
			`INSERT INTO plugin_config (plugin_name, key, value)
			 VALUES (?, ?, ?)`,
			m.Metadata.Name, f.Key, f.Default,
		); err != nil {
			return fmt.Errorf("insert plugin_config[%s]: %w", f.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SetManifestYAML overwrites the stored raw manifest bytes for a
// plugin. install.go calls this immediately after Insert with the
// operator-supplied YAML so what we hand back to the UI matches
// the input verbatim, including comments and field ordering.
func (s *Store) SetManifestYAML(name string, yaml string) error {
	if yaml == "" {
		return fmt.Errorf("plugin.SetManifestYAML: empty yaml")
	}
	res, err := s.db.Exec(
		`UPDATE plugins SET manifest = ?, updated_at = datetime('now') WHERE name = ?`,
		yaml, name,
	)
	if err != nil {
		return fmt.Errorf("update plugins.manifest: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// Get loads a complete PluginRecord. Returns ErrPluginNotFound
// if the plugin does not exist.
func (s *Store) Get(name string) (*PluginRecord, error) {
	row, err := s.getPluginRow(name)
	if err != nil {
		return nil, err
	}
	rec := &PluginRecord{Plugin: row}
	if rec.Instances, err = s.listInstances(name); err != nil {
		return nil, err
	}
	if rec.Volumes, err = s.listVolumes(name); err != nil {
		return nil, err
	}
	if rec.Ports, err = s.listPorts(name); err != nil {
		return nil, err
	}
	if rec.Config, err = s.listConfig(name); err != nil {
		return nil, err
	}
	return rec, nil
}

// List returns the lightweight `plugins` rows for every installed
// plugin, ordered by name. Suitable for the UI list page.
func (s *Store) List() ([]PluginRow, error) {
	rows, err := s.db.Query(
		`SELECT name, version, state, manifest, artifact_type,
		        COALESCE(image_ref, ''), COALESCE(distro_summary, ''),
		        instance_count, instance_configurable,
		        installed_at, updated_at
		 FROM plugins
		 ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PluginRow
	for rows.Next() {
		r, err := scanPluginRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes the plugin and every cascading row. Returns
// ErrPluginNotFound when no row matches. Phase 1 callers use this
// directly; phase 02+ wraps it with container teardown.
func (s *Store) Delete(name string) error {
	res, err := s.db.Exec(`DELETE FROM plugins WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete plugin: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// SetInstanceState updates one instance's state and recomputes the
// aggregate plugins.state column atomically. Returns ErrPluginNotFound
// if the (plugin, instance) row doesn't exist.
//
// Exposed in phase 1 so the CLI can simulate state transitions for
// tests and so phase 02 has a single callsite to plug into.
func (s *Store) SetInstanceState(name string, instance int, state, lastError string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(
		`UPDATE plugin_instances
		 SET state = ?, last_error = ?, last_change = datetime('now')
		 WHERE plugin_name = ? AND instance = ?`,
		state, sqlNullable(lastError), name, instance,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPluginNotFound
	}

	if err := recomputeAggregateStateTx(tx, name); err != nil {
		return err
	}
	return tx.Commit()
}

// recomputeAggregateStateTx runs the per-instance → aggregate state
// rollup defined in plugins-01-foundation.md.
func recomputeAggregateStateTx(tx *sql.Tx, name string) error {
	rows, err := tx.Query(
		`SELECT state FROM plugin_instances WHERE plugin_name = ?`,
		name,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		counts[s]++
		total++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total == 0 {
		// Plugin row exists but no instances? Treat as failed —
		// shouldn't happen but don't leave stale state.
		_, err := tx.Exec(`UPDATE plugins SET state = ? WHERE name = ?`, StateFailed, name)
		return err
	}

	agg := aggregateState(counts, total)
	_, err = tx.Exec(
		`UPDATE plugins SET state = ?, updated_at = datetime('now') WHERE name = ?`,
		agg, name,
	)
	return err
}

// aggregateState applies the rollup table from the proposal:
//   - any in-flight transitional state (pulling/creating/starting)
//     wins (we report progress, not the eventual outcome)
//   - all instances same → that state
//   - some running, some failed → degraded
//   - mixed otherwise → degraded
func aggregateState(counts map[string]int, total int) string {
	for _, transitional := range []string{StatePulling, StateCreating, StateStarting} {
		if counts[transitional] > 0 {
			return transitional
		}
	}
	if counts[StateRunning] == total {
		return StateRunning
	}
	if counts[StateFailed] == total {
		return StateFailed
	}
	if counts[StateStopped] == total {
		return StateStopped
	}
	if counts[StateInstalled] == total {
		return StateInstalled
	}
	return StateDegraded
}

// --- internal helpers ---

func (s *Store) getPluginRow(name string) (PluginRow, error) {
	row := s.db.QueryRow(
		`SELECT name, version, state, manifest, artifact_type,
		        COALESCE(image_ref, ''), COALESCE(distro_summary, ''),
		        instance_count, instance_configurable,
		        installed_at, updated_at
		 FROM plugins WHERE name = ?`,
		name,
	)
	r, err := scanPluginRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PluginRow{}, ErrPluginNotFound
	}
	return r, err
}

// rowScanner is the common interface of *sql.Row and *sql.Rows used
// by scanPluginRow.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPluginRow(rs rowScanner) (PluginRow, error) {
	var r PluginRow
	var configurable int
	if err := rs.Scan(
		&r.Name, &r.Version, &r.State, &r.ManifestYAML, &r.ArtifactType,
		&r.ImageRef, &r.DistroSummary,
		&r.InstanceCount, &configurable,
		&r.InstalledAt, &r.UpdatedAt,
	); err != nil {
		return PluginRow{}, err
	}
	r.InstanceConfigurable = configurable != 0
	return r, nil
}

func (s *Store) listInstances(name string) ([]InstanceRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, instance, COALESCE(container_id, ''), state,
		        COALESCE(bridge_ip, ''), last_change, COALESCE(last_error, '')
		 FROM plugin_instances WHERE plugin_name = ? ORDER BY instance`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceRow
	for rows.Next() {
		var r InstanceRow
		if err := rows.Scan(
			&r.PluginName, &r.Instance, &r.ContainerID, &r.State,
			&r.BridgeIP, &r.LastChange, &r.LastError,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) listVolumes(name string) ([]VolumeRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, volume_name, mode,
		        COALESCE(slot, ''), COALESCE(tier_pool, ''),
		        per_instance, bind_path
		 FROM plugin_volumes WHERE plugin_name = ? ORDER BY volume_name`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vols := map[string]*VolumeRow{}
	var order []string
	for rows.Next() {
		var v VolumeRow
		var perInst int
		if err := rows.Scan(
			&v.PluginName, &v.Name, &v.Mode, &v.Slot, &v.TierPool,
			&perInst, &v.BindPath,
		); err != nil {
			return nil, err
		}
		v.PerInstance = perInst != 0
		v.Paths = map[int]string{}
		vols[v.Name] = &v
		order = append(order, v.Name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pathRows, err := s.db.Query(
		`SELECT volume_name, instance, host_path
		 FROM plugin_volume_paths WHERE plugin_name = ?`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer pathRows.Close()
	for pathRows.Next() {
		var vol string
		var inst int
		var host string
		if err := pathRows.Scan(&vol, &inst, &host); err != nil {
			return nil, err
		}
		if v, ok := vols[vol]; ok {
			v.Paths[inst] = host
		}
	}
	if err := pathRows.Err(); err != nil {
		return nil, err
	}

	out := make([]VolumeRow, 0, len(order))
	for _, n := range order {
		out = append(out, *vols[n])
	}
	return out, nil
}

func (s *Store) listPorts(name string) ([]PortRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, port_name, container_port, protocol,
		        expose, host_expose
		 FROM plugin_ports WHERE plugin_name = ? ORDER BY port_name`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortRow
	for rows.Next() {
		var r PortRow
		var expose, hostExpose int
		if err := rows.Scan(
			&r.PluginName, &r.Name, &r.ContainerPort, &r.Protocol,
			&expose, &hostExpose,
		); err != nil {
			return nil, err
		}
		r.Expose = expose != 0
		r.HostExpose = hostExpose != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) listConfig(name string) ([]ConfigRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, key, value
		 FROM plugin_config WHERE plugin_name = ? ORDER BY key`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigRow
	for rows.Next() {
		var r ConfigRow
		if err := rows.Scan(&r.PluginName, &r.Key, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sqlNullable converts a Go string to a sql.NullString that is
// invalid (NULL) when empty.
func sqlNullable(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// isUniqueViolation matches the SQLite error text for a UNIQUE
// constraint failure on plugins.name. The driver does not expose
// a typed code, so we string-match.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed: plugins.name")
}
