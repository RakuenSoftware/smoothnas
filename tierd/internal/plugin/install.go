package plugin

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPluginsRoot is the on-disk parent for flat-mode plugin
// volumes. Tier-bound volumes live under the tier mountpoint
// (resolved in phase 03), not here.
const DefaultPluginsRoot = "/var/lib/smoothnas/plugins"

// Installer wires a plugin.Store to a filesystem layout. It owns
// the install / uninstall flow that operators reach through the
// CLI (and, in phase 06, the UI).
type Installer struct {
	store        *Store
	pluginsRoot  string

	// mkdirAll is overridable in tests. Defaults to os.MkdirAll.
	mkdirAll func(path string, perm os.FileMode) error
	// removeAll is overridable in tests. Defaults to os.RemoveAll.
	removeAll func(path string) error
}

// NewInstaller constructs an Installer rooted at DefaultPluginsRoot.
func NewInstaller(store *Store) *Installer {
	return &Installer{
		store:       store,
		pluginsRoot: DefaultPluginsRoot,
		mkdirAll:    os.MkdirAll,
		removeAll:   os.RemoveAll,
	}
}

// SetPluginsRoot overrides the flat-volume parent directory.
// Used by tests; production callers stick with DefaultPluginsRoot.
func (i *Installer) SetPluginsRoot(path string) {
	i.pluginsRoot = path
}

// Install parses the YAML, validates it, fans out per-instance
// host paths for each volume, mkdirs the flat ones, and persists
// the result atomically. The original YAML bytes are stored
// verbatim so the UI can re-display the operator's input.
//
// Tier-bound volume host paths are left empty in the DB; phase 03
// resolves them. The directories under DefaultPluginsRoot are the
// only filesystem effect of phase 1.
//
// Returns ErrPluginExists if the plugin name is already taken.
// Returns *ValidationError on field-level manifest problems.
func (i *Installer) Install(yamlBytes []byte) (*PluginRecord, error) {
	m, err := ParseManifest(yamlBytes)
	if err != nil {
		return nil, err
	}
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}

	count := m.EffectiveCount()
	paths, mkdirs, err := i.resolveVolumePaths(m, count)
	if err != nil {
		return nil, err
	}

	// mkdir before insert. If the insert fails (e.g. duplicate
	// plugin name), the directories we created stay — which is
	// fine because the operator either retries (idempotent) or
	// uninstalls a stale half-state. install.go is the place we
	// trade strict atomicity for understandable failure modes.
	for _, dir := range mkdirs {
		if err := i.mkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	if err := i.store.Insert(InsertParams{Manifest: m, Paths: paths}); err != nil {
		return nil, err
	}
	if err := i.store.SetManifestYAML(m.Metadata.Name, string(yamlBytes)); err != nil {
		return nil, fmt.Errorf("store raw manifest: %w", err)
	}

	return i.store.Get(m.Metadata.Name)
}

// resolveVolumePaths computes the per-instance host_path map plus
// the list of flat-volume directories that need creating on disk.
// Tier-bound paths come back as "" — phase 03 fills them in.
func (i *Installer) resolveVolumePaths(m *Manifest, count int) (
	paths map[string]map[int]string,
	mkdirs []string,
	err error,
) {
	paths = make(map[string]map[int]string, len(m.Volumes))
	for _, vol := range m.Volumes {
		entries := make(map[int]string)
		switch vol.Mode {
		case VolumeModeFlat:
			if vol.PerInstance {
				for inst := 1; inst <= count; inst++ {
					p := filepath.Join(i.pluginsRoot, m.Metadata.Name,
						fmt.Sprintf("instance-%d", inst), vol.Name)
					entries[inst] = p
					mkdirs = append(mkdirs, p)
				}
			} else {
				p := filepath.Join(i.pluginsRoot, m.Metadata.Name, vol.Name)
				entries[1] = p
				mkdirs = append(mkdirs, p)
			}
		case VolumeModeTierBound:
			// Phase 03 resolves these. Phase 1 stores empty paths
			// at the right (instance) keys so the schema stays
			// consistent (every volume always has at least one row).
			if vol.PerInstance {
				for inst := 1; inst <= count; inst++ {
					entries[inst] = ""
				}
			} else {
				entries[1] = ""
			}
		default:
			return nil, nil, fmt.Errorf("resolveVolumePaths: unknown mode %q for volume %q", vol.Mode, vol.Name)
		}
		paths[vol.Name] = entries
	}
	return paths, mkdirs, nil
}

// Uninstall is the phase-1 stub: deletes the DB rows (cascading to
// every child table) and removes the plugin's flat-volume directory
// tree under DefaultPluginsRoot. Tier-bound volume teardown lives in
// phase 03; container/image/nginx teardown lives in phases 02 and 04.
//
// If the plugin doesn't exist, returns ErrPluginNotFound.
func (i *Installer) Uninstall(name string) error {
	// Snapshot which flat directories we own so we know what to remove
	// after the DB rows are gone.
	rec, err := i.store.Get(name)
	if err != nil {
		return err
	}

	if err := i.store.Delete(name); err != nil {
		return err
	}

	// Remove flat volume dirs. Tier-bound volumes have empty host paths
	// in phase 1 — skip them. Phase 03 owns tier-bound teardown.
	for _, vol := range rec.Volumes {
		if vol.Mode != VolumeModeFlat {
			continue
		}
		for _, host := range vol.Paths {
			if host == "" {
				continue
			}
			if err := i.removeAll(host); err != nil {
				return fmt.Errorf("remove %s: %w", host, err)
			}
		}
	}
	// Remove the per-plugin parent directory if empty (single-instance)
	// or both per-instance parents (multi-instance). Best effort —
	// failure is fine, the directory might already be gone or the
	// operator might have left junk in it.
	parent := filepath.Join(i.pluginsRoot, name)
	_ = os.Remove(parent) //nolint:errcheck // best-effort cleanup

	return nil
}
