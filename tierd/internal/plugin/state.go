package plugin

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// Plugin states. Per-instance states match StateInstalled..StateFailed.
// The aggregate `plugins.state` column also accepts StateDegraded (used
// when some (service,instance) units are running and others are failed).
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

const bearerExpectedConfigKey = "SMOOTHNAS_BEARER_EXPECTED"

// Store is the persistence layer for the plugin subsystem. It wraps
// the shared db.Store and exposes plugin-shaped CRUD over the
// per-service tables (plugins, plugin_services, and the per-(service)
// child tables introduced/extended in migrations 00011 and 00019).
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
// joined with derived fields. Used by Get / List. ArtifactType /
// ImageRef / DistroSummary are legacy mirrors of the plugin's primary
// (first) service — the authoritative per-service data lives in
// PluginRecord.Services.
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
	// ResolvedProfiles is the profile-name list that was actually
	// applied at install/materialise time (after default-limits
	// auto-injection and operator overrides). Empty list = no profiles.
	ResolvedProfiles []string
}

// ServiceRow is the in-memory image of one plugin_services row.
type ServiceRow struct {
	PluginName    string
	Service       string
	ArtifactType  string
	ImageRef      string
	DistroSummary string
	// DependsOn maps a sibling service name → start condition
	// (service_started / service_healthy).
	DependsOn map[string]string
	Health    *Healthcheck
	Ordinal   int
}

// InstanceRow is the in-memory image of one plugin_instances row. The
// run unit is (PluginName, Service, Instance).
type InstanceRow struct {
	PluginName  string
	Service     string
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
	Service     string
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
	Service       string
	Name          string
	ContainerPort int
	Protocol      string
	Expose        bool
	HostExpose    bool
}

// ConfigRow is the in-memory image of one plugin_config row.
type ConfigRow struct {
	PluginName string
	Service    string
	Key        string
	Value      string
}

// PluginRecord bundles every table's view of a single plugin into
// one structure, the shape callers usually want.
type PluginRecord struct {
	Plugin    PluginRow
	Services  []ServiceRow
	Instances []InstanceRow
	Volumes   []VolumeRow
	Ports     []PortRow
	Config    []ConfigRow
}

// InsertParams is the input to Insert: the validated manifest plus
// the resolved per-(service,volume) host paths. install.go fills this in.
type InsertParams struct {
	Manifest *Manifest
	// Paths is keyed by service → volume name → instance number → host
	// path. Tier-bound volumes whose host path has not yet been resolved
	// MUST still appear with their instance entries set to "" (empty
	// string); the plugin_volumes row's tier_pool gets the sentinel
	// "<unresolved>" in that case.
	Paths map[string]map[string]map[int]string
	// Tiers maps service → volume name → tier pool name for tier-bound
	// volumes the caller has already resolved. Volumes not present fall
	// back to the "<unresolved>" sentinel.
	Tiers map[string]map[string]string
	// Config maps manifest config keys to operator-supplied install-time
	// values. Applied to every service that declares the key. Keys not
	// declared by any service are ignored.
	Config map[string]string
}

// Insert persists a plugin atomically across all tables. Returns
// ErrPluginExists if a plugin with this name is already installed.
func (s *Store) Insert(p InsertParams) error {
	if p.Manifest == nil {
		return fmt.Errorf("plugin.Insert: nil manifest")
	}
	m := p.Manifest
	if len(m.Services) == 0 {
		return fmt.Errorf("plugin.Insert: manifest has no services")
	}

	// install.go stamps the real bytes immediately after Insert; the
	// schema requires a non-empty value, so a placeholder is fine.
	manifestText := fmt.Sprintf("# parsed manifest for %s@%s\n", m.Metadata.Name, m.Metadata.Version)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort on error path

	count := m.EffectiveCount()
	configurable := 0
	if m.Instances.Configurable {
		configurable = 1
	}

	// Legacy mirror columns on `plugins` carry the primary (first)
	// service's artifact for display; plugin_services is authoritative.
	primary := &m.Services[0]
	legacyImage := ""
	if primary.Artifact.Type == ArtifactOCIImage {
		legacyImage = digestPinnedImageRef(primary.Artifact.Image, primary.Artifact.Digest)
	}

	res, err := tx.Exec(
		`INSERT INTO plugins
		 (name, version, state, manifest, artifact_type,
		  image_ref, distro_summary,
		  instance_count, instance_configurable)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Metadata.Name, m.Metadata.Version, StateInstalled, manifestText,
		primary.Artifact.Type, sqlNullable(legacyImage), sqlNullable(primary.DistroSummary()),
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

	ordinals := serviceOrdinals(m.Services)

	for si := range m.Services {
		svc := &m.Services[si]

		imageRef := ""
		if svc.Artifact.Type == ArtifactOCIImage {
			imageRef = digestPinnedImageRef(svc.Artifact.Image, svc.Artifact.Digest)
		}
		dependsJSON, err := marshalDependsOn(svc.DependsOn)
		if err != nil {
			return fmt.Errorf("encode dependsOn[%s]: %w", svc.Name, err)
		}
		healthJSON, err := marshalHealth(svc.Health)
		if err != nil {
			return fmt.Errorf("encode health[%s]: %w", svc.Name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO plugin_services
			 (plugin_name, service, artifact_type, image_ref,
			  distro_summary, depends_on, health, ordinal)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.Metadata.Name, svc.Name, svc.Artifact.Type, sqlNullable(imageRef),
			sqlNullable(svc.DistroSummary()), dependsJSON, healthJSON, ordinals[svc.Name],
		); err != nil {
			return fmt.Errorf("insert plugin_services[%s]: %w", svc.Name, err)
		}

		// One plugin_instances row per replica, per service.
		for i := 1; i <= count; i++ {
			if _, err := tx.Exec(
				`INSERT INTO plugin_instances
				 (plugin_name, service, instance, state)
				 VALUES (?, ?, ?, ?)`,
				m.Metadata.Name, svc.Name, i, StateInstalled,
			); err != nil {
				return fmt.Errorf("insert plugin_instances[%s/%d]: %w", svc.Name, i, err)
			}
		}

		// Volumes + their per-instance paths.
		for _, vol := range svc.Volumes {
			perInst := 0
			if vol.PerInstance {
				perInst = 1
			}
			var tierPool sql.NullString
			var slot sql.NullString
			if vol.Mode == VolumeModeTierBound {
				slot = sql.NullString{String: vol.Slot, Valid: vol.Slot != ""}
				if resolved, ok := p.Tiers[svc.Name][vol.Name]; ok && resolved != "" {
					tierPool = sql.NullString{String: resolved, Valid: true}
				} else {
					tierPool = sql.NullString{String: "<unresolved>", Valid: true}
				}
			}
			if _, err := tx.Exec(
				`INSERT INTO plugin_volumes
				 (plugin_name, service, volume_name, mode, slot, tier_pool,
				  per_instance, bind_path)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				m.Metadata.Name, svc.Name, vol.Name, vol.Mode, slot, tierPool,
				perInst, vol.Bind,
			); err != nil {
				return fmt.Errorf("insert plugin_volumes[%s/%s]: %w", svc.Name, vol.Name, err)
			}

			paths := p.Paths[svc.Name][vol.Name]
			if paths == nil {
				return fmt.Errorf("plugin.Insert: missing host paths for volume %q in service %q", vol.Name, svc.Name)
			}
			var expected int
			if vol.PerInstance {
				expected = count
			} else {
				expected = 1
			}
			if len(paths) != expected {
				return fmt.Errorf("plugin.Insert: volume %q/%q expected %d path(s), got %d", svc.Name, vol.Name, expected, len(paths))
			}
			for inst, host := range paths {
				if _, err := tx.Exec(
					`INSERT INTO plugin_volume_paths
					 (plugin_name, service, volume_name, instance, host_path)
					 VALUES (?, ?, ?, ?, ?)`,
					m.Metadata.Name, svc.Name, vol.Name, inst, host,
				); err != nil {
					return fmt.Errorf("insert plugin_volume_paths[%s/%s/%d]: %w", svc.Name, vol.Name, inst, err)
				}
			}
		}

		// Ports.
		for _, port := range svc.Ports {
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
				 (plugin_name, service, port_name, container_port, protocol,
				  expose, host_expose)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				m.Metadata.Name, svc.Name, port.Name, port.Port, port.Protocol,
				expose, hostExpose,
			); err != nil {
				return fmt.Errorf("insert plugin_ports[%s/%s]: %w", svc.Name, port.Name, err)
			}
		}

		// Config defaults plus any install-time operator overrides.
		for _, f := range svc.Config {
			value := f.Default
			if p.Config != nil {
				if override, ok := p.Config[f.Key]; ok {
					value = override
				}
			}
			if _, err := tx.Exec(
				`INSERT INTO plugin_config (plugin_name, service, key, value)
				 VALUES (?, ?, ?, ?)`,
				m.Metadata.Name, svc.Name, f.Key, value,
			); err != nil {
				return fmt.Errorf("insert plugin_config[%s/%s]: %w", svc.Name, f.Key, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// serviceOrdinals returns each service's start ordinal: dependencies
// get lower ordinals than the services that depend on them. The graph
// is acyclic (ValidateManifest guarantees it); a defensive fallback
// assigns any remaining services in declared order.
func serviceOrdinals(services []Service) map[string]int {
	known := make(map[string]bool, len(services))
	for i := range services {
		known[services[i].Name] = true
	}
	ord := make(map[string]int, len(services))
	placed := make(map[string]bool, len(services))
	n := 0
	for len(placed) < len(services) {
		progressed := false
		for i := range services {
			s := &services[i]
			if placed[s.Name] {
				continue
			}
			ready := true
			for dep := range s.DependsOn {
				if known[dep] && dep != s.Name && !placed[dep] {
					ready = false
					break
				}
			}
			if ready {
				ord[s.Name] = n
				n++
				placed[s.Name] = true
				progressed = true
			}
		}
		if !progressed {
			for i := range services {
				if !placed[services[i].Name] {
					ord[services[i].Name] = n
					n++
					placed[services[i].Name] = true
				}
			}
		}
	}
	return ord
}

func marshalDependsOn(deps map[string]DependsCondition) (sql.NullString, error) {
	if len(deps) == 0 {
		return sql.NullString{}, nil
	}
	flat := make(map[string]string, len(deps))
	for k, v := range deps {
		flat[k] = v.Condition
	}
	b, err := json.Marshal(flat)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func marshalHealth(h *Healthcheck) (sql.NullString, error) {
	if h == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// SetManifestYAML overwrites the stored raw manifest bytes for a plugin.
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
	if rec.Services, err = s.listServices(name); err != nil {
		return nil, err
	}
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
		        installed_at, updated_at, profiles_json
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
// ErrPluginNotFound when no row matches.
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

// GetBearerToken returns the per-plugin bearer token issued for the
// nginx auth-injection flow. Returns empty string + nil when the
// plugin has no token.
func (s *Store) GetBearerToken(name string) (string, error) {
	var token string
	err := s.db.QueryRow(
		`SELECT bearer_token FROM plugin_secrets WHERE plugin_name = ?`,
		name,
	).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get bearer token: %w", err)
	}
	return token, nil
}

// IssueBearerToken generates a new 256-bit token, persists it, and
// returns the value. The expected-bearer config key is written to the
// plugin's primary (highest-ordinal) service — the user-facing one that
// fronts the UI. Idempotent; calling twice rotates the token.
func (s *Store) IssueBearerToken(name string) (string, error) {
	if _, err := s.getPluginRow(name); err != nil {
		return "", err
	}
	token, err := newBearerToken()
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	svc, err := primaryServiceTx(tx, name)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(
		`INSERT INTO plugin_secrets (plugin_name, bearer_token)
		 VALUES (?, ?)
		 ON CONFLICT(plugin_name) DO UPDATE SET
		   bearer_token = excluded.bearer_token,
		   issued_at    = datetime('now')`,
		name, token,
	); err != nil {
		return "", fmt.Errorf("upsert plugin_secrets: %w", err)
	}
	if _, err = tx.Exec(
		`INSERT INTO plugin_config (plugin_name, service, key, value)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(plugin_name, service, key) DO UPDATE SET
		   value = excluded.value`,
		name, svc, bearerExpectedConfigKey, token,
	); err != nil {
		return "", fmt.Errorf("upsert bearer config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit bearer token: %w", err)
	}
	return token, nil
}

// DeleteBearerToken removes the per-plugin token row.
func (s *Store) DeleteBearerToken(name string) error {
	_, err := s.db.Exec(`DELETE FROM plugin_secrets WHERE plugin_name = ?`, name)
	return err
}

// newBearerToken returns 32 random bytes as a hex string (64 chars,
// 256 bits of entropy).
func newBearerToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// primaryServiceTx returns the plugin's primary service — the
// highest-ordinal one, which fronts the UI in a compose-style plugin
// (it depends on its backends, so it starts last). Falls back to the
// first service by name when ordinals tie.
func primaryServiceTx(tx *sql.Tx, name string) (string, error) {
	var svc string
	err := tx.QueryRow(
		`SELECT service FROM plugin_services
		 WHERE plugin_name = ?
		 ORDER BY ordinal DESC, service ASC
		 LIMIT 1`,
		name,
	).Scan(&svc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPluginNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve primary service: %w", err)
	}
	return svc, nil
}

// TierConsumers returns the names of every installed plugin that has
// at least one volume bound to the given tier pool.
func (s *Store) TierConsumers(poolName string) ([]string, error) {
	if poolName == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT plugin_name
		 FROM plugin_volumes
		 WHERE tier_pool = ?
		 ORDER BY plugin_name`,
		poolName,
	)
	if err != nil {
		return nil, fmt.Errorf("query plugin tier consumers: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// RawDB returns the underlying db.Store.
func (s *Store) RawDB() *db.Store { return s.store }

// ReplaceConfig wipes every existing plugin_config row for the named
// plugin's primary service and inserts the supplied key→value map in
// one transaction. Generated bearer auth config is preserved when
// callers omit it or submit it empty. Returns ErrPluginNotFound when
// the plugin does not exist.
//
// Multi-service config editing is a phase-06 concern; today operator
// config edits target the primary (user-facing) service, which is the
// only service for single-container plugins.
func (s *Store) ReplaceConfig(name string, cfg map[string]string) error {
	if _, err := s.getPluginRow(name); err != nil {
		return err
	}
	cfg = cloneConfig(cfg)
	if existing, err := s.listConfig(name); err == nil {
		for _, c := range existing {
			if c.Key == bearerExpectedConfigKey && c.Value != "" {
				if cfg[bearerExpectedConfigKey] == "" {
					cfg[bearerExpectedConfigKey] = c.Value
				}
				break
			}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	svc, err := primaryServiceTx(tx, name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plugin_config WHERE plugin_name = ? AND service = ?`, name, svc); err != nil {
		return fmt.Errorf("delete plugin_config: %w", err)
	}
	for k, v := range cfg {
		if _, err := tx.Exec(
			`INSERT INTO plugin_config (plugin_name, service, key, value) VALUES (?, ?, ?, ?)`,
			name, svc, k, v,
		); err != nil {
			return fmt.Errorf("insert plugin_config[%s]: %w", k, err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE plugins SET updated_at = datetime('now') WHERE name = ?`, name,
	); err != nil {
		return fmt.Errorf("touch plugin row: %w", err)
	}
	return tx.Commit()
}

func cloneConfig(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

// SetResolvedProfiles records the merged-profile-name list applied at
// install/materialise time as a JSON array.
func (s *Store) SetResolvedProfiles(name string, profileNames []string) error {
	if profileNames == nil {
		profileNames = []string{}
	}
	encoded, err := json.Marshal(profileNames)
	if err != nil {
		return fmt.Errorf("encode profiles: %w", err)
	}
	res, err := s.db.Exec(
		`UPDATE plugins SET profiles_json = ?, updated_at = datetime('now') WHERE name = ?`,
		string(encoded), name,
	)
	if err != nil {
		return fmt.Errorf("update plugins.profiles_json: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// SetInstanceContainerID records the runtime daemon's container ID for
// one (service, instance) unit.
func (s *Store) SetInstanceContainerID(name, service string, instance int, id string) error {
	res, err := s.db.Exec(
		`UPDATE plugin_instances
		 SET container_id = ?, last_change = datetime('now')
		 WHERE plugin_name = ? AND service = ? AND instance = ?`,
		sqlNullable(id), name, service, instance,
	)
	if err != nil {
		return fmt.Errorf("update plugin_instances.container_id: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// UpdateManifest replaces an installed plugin's manifest while preserving
// operator-owned state: volume placements, instance count, and existing
// config values for keys that still exist. Volume or service shape changes
// are rejected because they require fresh tier assignment / placement.
func (s *Store) UpdateManifest(name string, m *Manifest, yamlText string) error {
	if m == nil {
		return fmt.Errorf("plugin.UpdateManifest: nil manifest")
	}
	if yamlText == "" {
		return fmt.Errorf("plugin.UpdateManifest: empty yaml")
	}
	if m.Metadata.Name != name {
		return fmt.Errorf("plugin.UpdateManifest: manifest name %q does not match installed plugin %q", m.Metadata.Name, name)
	}

	rec, err := s.Get(name)
	if err != nil {
		return err
	}
	if err := compatibleVolumeSchema(rec.Volumes, m); err != nil {
		return err
	}
	if !m.Instances.Configurable && m.EffectiveCount() != rec.Plugin.InstanceCount {
		return fmt.Errorf("%w: instance count change (installed %d, manifest %d)", ErrPluginUpdateRequiresReinstall, rec.Plugin.InstanceCount, m.EffectiveCount())
	}

	// Preserve config values keyed by (service, key).
	existingConfig := make(map[string]map[string]string, len(rec.Services))
	for _, row := range rec.Config {
		if existingConfig[row.Service] == nil {
			existingConfig[row.Service] = map[string]string{}
		}
		existingConfig[row.Service][row.Key] = row.Value
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort on error path

	primary := &m.Services[0]
	legacyImage := ""
	if primary.Artifact.Type == ArtifactOCIImage {
		legacyImage = digestPinnedImageRef(primary.Artifact.Image, primary.Artifact.Digest)
	}
	configurable := 0
	if m.Instances.Configurable {
		configurable = 1
	}

	res, err := tx.Exec(
		`UPDATE plugins
		 SET version = ?, manifest = ?, artifact_type = ?,
		     image_ref = ?, distro_summary = ?,
		     instance_configurable = ?, updated_at = datetime('now')
		 WHERE name = ?`,
		m.Metadata.Version, yamlText, primary.Artifact.Type,
		sqlNullable(legacyImage), sqlNullable(primary.DistroSummary()),
		configurable, name,
	)
	if err != nil {
		return fmt.Errorf("update plugins: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPluginNotFound
	}

	// Refresh per-service artifact mirrors (depends_on/health/ordinal too).
	ordinals := serviceOrdinals(m.Services)
	for si := range m.Services {
		svc := &m.Services[si]
		imageRef := ""
		if svc.Artifact.Type == ArtifactOCIImage {
			imageRef = digestPinnedImageRef(svc.Artifact.Image, svc.Artifact.Digest)
		}
		dependsJSON, err := marshalDependsOn(svc.DependsOn)
		if err != nil {
			return fmt.Errorf("encode dependsOn[%s]: %w", svc.Name, err)
		}
		healthJSON, err := marshalHealth(svc.Health)
		if err != nil {
			return fmt.Errorf("encode health[%s]: %w", svc.Name, err)
		}
		if _, err := tx.Exec(
			`UPDATE plugin_services
			 SET artifact_type = ?, image_ref = ?, distro_summary = ?,
			     depends_on = ?, health = ?, ordinal = ?
			 WHERE plugin_name = ? AND service = ?`,
			svc.Artifact.Type, sqlNullable(imageRef), sqlNullable(svc.DistroSummary()),
			dependsJSON, healthJSON, ordinals[svc.Name], name, svc.Name,
		); err != nil {
			return fmt.Errorf("update plugin_services[%s]: %w", svc.Name, err)
		}
	}

	// Rebuild ports + config from the new manifest, per service.
	if _, err := tx.Exec(`DELETE FROM plugin_ports WHERE plugin_name = ?`, name); err != nil {
		return fmt.Errorf("delete plugin_ports: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM plugin_config WHERE plugin_name = ?`, name); err != nil {
		return fmt.Errorf("delete plugin_config: %w", err)
	}
	for si := range m.Services {
		svc := &m.Services[si]
		for _, port := range svc.Ports {
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
				 (plugin_name, service, port_name, container_port, protocol,
				  expose, host_expose)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				name, svc.Name, port.Name, port.Port, port.Protocol, expose, hostExpose,
			); err != nil {
				return fmt.Errorf("insert plugin_ports[%s/%s]: %w", svc.Name, port.Name, err)
			}
		}
		for _, field := range svc.Config {
			value, ok := existingConfig[svc.Name][field.Key]
			if !ok {
				value = field.Default
			}
			if _, err := tx.Exec(
				`INSERT INTO plugin_config (plugin_name, service, key, value)
				 VALUES (?, ?, ?, ?)`,
				name, svc.Name, field.Key, value,
			); err != nil {
				return fmt.Errorf("insert plugin_config[%s/%s]: %w", svc.Name, field.Key, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// compatibleVolumeSchema rejects volume shape changes across an update.
// Volumes are compared by (service, volume name); any add/remove/shape
// change requires a reinstall so tier placement can be re-chosen.
func compatibleVolumeSchema(installed []VolumeRow, m *Manifest) error {
	type key struct{ svc, vol string }
	byKey := make(map[key]VolumeRow, len(installed))
	for _, vol := range installed {
		byKey[key{vol.Service, vol.Name}] = vol
	}
	desiredCount := 0
	for si := range m.Services {
		svc := &m.Services[si]
		for _, vol := range svc.Volumes {
			desiredCount++
			got, ok := byKey[key{svc.Name, vol.Name}]
			if !ok {
				return fmt.Errorf("%w: volume %q/%q is new", ErrPluginUpdateRequiresReinstall, svc.Name, vol.Name)
			}
			if got.Mode != vol.Mode || got.Slot != vol.Slot || got.PerInstance != vol.PerInstance || got.BindPath != vol.Bind {
				return fmt.Errorf("%w: volume %q/%q changed shape", ErrPluginUpdateRequiresReinstall, svc.Name, vol.Name)
			}
		}
	}
	if desiredCount != len(installed) {
		return fmt.Errorf("%w: volume changes", ErrPluginUpdateRequiresReinstall)
	}
	return nil
}

// SetImageRef updates the resolved image ref for one service (and, when
// it is the primary service, the legacy mirror on the plugins row).
func (s *Store) SetImageRef(name, service, ref string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(
		`UPDATE plugin_services SET image_ref = ? WHERE plugin_name = ? AND service = ?`,
		sqlNullable(ref), name, service,
	)
	if err != nil {
		return fmt.Errorf("update plugin_services.image_ref: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPluginNotFound
	}
	primary, err := primaryServiceTx(tx, name)
	if err != nil {
		return err
	}
	if service == primary {
		if _, err := tx.Exec(
			`UPDATE plugins SET image_ref = ?, updated_at = datetime('now') WHERE name = ?`,
			sqlNullable(ref), name,
		); err != nil {
			return fmt.Errorf("update plugins.image_ref: %w", err)
		}
	}
	return tx.Commit()
}

// SetInstanceBridgeIP records the per-(service,instance) bridge IP.
func (s *Store) SetInstanceBridgeIP(name, service string, instance int, ip string) error {
	res, err := s.db.Exec(
		`UPDATE plugin_instances
		 SET bridge_ip = ?, last_change = datetime('now')
		 WHERE plugin_name = ? AND service = ? AND instance = ?`,
		sqlNullable(ip), name, service, instance,
	)
	if err != nil {
		return fmt.Errorf("update plugin_instances.bridge_ip: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// SetInstanceState updates one (service, instance) unit's state and
// recomputes the aggregate plugins.state column atomically. Returns
// ErrPluginNotFound if the row doesn't exist.
func (s *Store) SetInstanceState(name, service string, instance int, state, lastError string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(
		`UPDATE plugin_instances
		 SET state = ?, last_error = ?, last_change = datetime('now')
		 WHERE plugin_name = ? AND service = ? AND instance = ?`,
		state, sqlNullable(lastError), name, service, instance,
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

// recomputeAggregateStateTx runs the per-unit → aggregate state rollup.
// It spans every (service, instance) row of the plugin, so a plugin is
// "running" only when all of its services' instances are running, and
// "degraded" when some are up and others are down.
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

// AggregateInstanceStates rolls a service's per-instance state counts up
// into a single state, using the same rule as the plugin-wide aggregate.
// Exposed for the API layer's per-service breakdown. A service with no
// instances reports StateInstalled.
func AggregateInstanceStates(counts map[string]int, total int) string {
	if total == 0 {
		return StateInstalled
	}
	return aggregateState(counts, total)
}

// aggregateState applies the rollup table from the proposal:
//   - any in-flight transitional state (pulling/creating/starting) wins
//   - all units same → that state
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
		        installed_at, updated_at, profiles_json
		 FROM plugins WHERE name = ?`,
		name,
	)
	r, err := scanPluginRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PluginRow{}, ErrPluginNotFound
	}
	return r, err
}

// rowScanner is the common interface of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPluginRow(rs rowScanner) (PluginRow, error) {
	var r PluginRow
	var configurable int
	var profilesJSON string
	if err := rs.Scan(
		&r.Name, &r.Version, &r.State, &r.ManifestYAML, &r.ArtifactType,
		&r.ImageRef, &r.DistroSummary,
		&r.InstanceCount, &configurable,
		&r.InstalledAt, &r.UpdatedAt, &profilesJSON,
	); err != nil {
		return PluginRow{}, err
	}
	r.InstanceConfigurable = configurable != 0
	if profilesJSON != "" {
		_ = json.Unmarshal([]byte(profilesJSON), &r.ResolvedProfiles)
	}
	return r, nil
}

func (s *Store) listServices(name string) ([]ServiceRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, service, artifact_type,
		        COALESCE(image_ref, ''), COALESCE(distro_summary, ''),
		        COALESCE(depends_on, ''), COALESCE(health, ''), ordinal
		 FROM plugin_services WHERE plugin_name = ? ORDER BY ordinal, service`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRow
	for rows.Next() {
		var r ServiceRow
		var dependsJSON, healthJSON string
		if err := rows.Scan(
			&r.PluginName, &r.Service, &r.ArtifactType,
			&r.ImageRef, &r.DistroSummary,
			&dependsJSON, &healthJSON, &r.Ordinal,
		); err != nil {
			return nil, err
		}
		if dependsJSON != "" {
			_ = json.Unmarshal([]byte(dependsJSON), &r.DependsOn)
		}
		if healthJSON != "" {
			var h Healthcheck
			if err := json.Unmarshal([]byte(healthJSON), &h); err == nil {
				r.Health = &h
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) listInstances(name string) ([]InstanceRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, service, instance, COALESCE(container_id, ''), state,
		        COALESCE(bridge_ip, ''), last_change, COALESCE(last_error, '')
		 FROM plugin_instances WHERE plugin_name = ? ORDER BY service, instance`,
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
			&r.PluginName, &r.Service, &r.Instance, &r.ContainerID, &r.State,
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
		`SELECT plugin_name, service, volume_name, mode,
		        COALESCE(slot, ''), COALESCE(tier_pool, ''),
		        per_instance, bind_path
		 FROM plugin_volumes WHERE plugin_name = ? ORDER BY service, volume_name`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type vkey struct{ svc, vol string }
	vols := map[vkey]*VolumeRow{}
	var order []vkey
	for rows.Next() {
		var v VolumeRow
		var perInst int
		if err := rows.Scan(
			&v.PluginName, &v.Service, &v.Name, &v.Mode, &v.Slot, &v.TierPool,
			&perInst, &v.BindPath,
		); err != nil {
			return nil, err
		}
		v.PerInstance = perInst != 0
		v.Paths = map[int]string{}
		k := vkey{v.Service, v.Name}
		vols[k] = &v
		order = append(order, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pathRows, err := s.db.Query(
		`SELECT service, volume_name, instance, host_path
		 FROM plugin_volume_paths WHERE plugin_name = ?`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer pathRows.Close()
	for pathRows.Next() {
		var svc, vol string
		var inst int
		var host string
		if err := pathRows.Scan(&svc, &vol, &inst, &host); err != nil {
			return nil, err
		}
		if v, ok := vols[vkey{svc, vol}]; ok {
			v.Paths[inst] = host
		}
	}
	if err := pathRows.Err(); err != nil {
		return nil, err
	}

	out := make([]VolumeRow, 0, len(order))
	for _, k := range order {
		out = append(out, *vols[k])
	}
	return out, nil
}

func (s *Store) listPorts(name string) ([]PortRow, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name, service, port_name, container_port, protocol,
		        expose, host_expose
		 FROM plugin_ports WHERE plugin_name = ? ORDER BY service, port_name`,
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
			&r.PluginName, &r.Service, &r.Name, &r.ContainerPort, &r.Protocol,
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
		`SELECT plugin_name, service, key, value
		 FROM plugin_config WHERE plugin_name = ? ORDER BY service, key`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigRow
	for rows.Next() {
		var r ConfigRow
		if err := rows.Scan(&r.PluginName, &r.Service, &r.Key, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sqlNullable converts a Go string to a sql.NullString that is invalid
// (NULL) when empty.
func sqlNullable(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// isUniqueViolation matches the SQLite error text for a UNIQUE
// constraint failure on plugins.name.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed: plugins.name")
}
