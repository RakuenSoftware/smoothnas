package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPluginsRoot is the on-disk parent for flat-mode plugin
// volumes. Tier-bound volumes live under the tier mountpoint
// (resolved in phase 03), not here.
const DefaultPluginsRoot = "/var/lib/smoothnas/plugins"

// Demolisher is the subset of *Lifecycle the Installer needs at
// uninstall time: stop every container, remove them, drop the
// cached image. An interface (rather than a *Lifecycle field)
// keeps install.go free of any runtime-client imports and lets
// phase 1 tests run with no runtime configured.
type Demolisher interface {
	Demolish(ctx context.Context, name string) error
}

// Installer wires a plugin.Store to a filesystem layout. It owns
// the install / uninstall flow that operators reach through the
// CLI (and, in phase 06, the UI).
//
// The Demolisher is optional. When nil (phase 1 builds, tests),
// Uninstall skips container/image teardown and is otherwise
// unchanged.
//
// The TierProvider is optional. When nil, tier-bound volumes are
// recorded as "<unresolved>" (phase 1 behaviour). When set, the
// installer runs the phase 03 preflight + path resolution.
type Installer struct {
	store        *Store
	pluginsRoot  string
	demolisher   Demolisher
	tierProvider TierProvider
	statfs       Statfser

	// mkdirAll is overridable in tests. Defaults to os.MkdirAll.
	mkdirAll func(path string, perm os.FileMode) error
	// removeAll is overridable in tests. Defaults to os.RemoveAll.
	removeAll func(path string) error
}

// InstallOptions carries optional install-time inputs. The zero
// value preserves phase-1 behaviour.
type InstallOptions struct {
	// Tiers selects the tier pool for each tier-bound volume in the
	// manifest. Volumes without an entry fall back to Tiers.Default.
	// A tier-bound volume with no resolution at install time gets a
	// "<unresolved>" tier_pool sentinel and an empty host_path —
	// the operator must reinstall with assignments to fix.
	Tiers TierAssignments
}

// NewInstaller constructs an Installer rooted at DefaultPluginsRoot.
// The returned Installer has no Demolisher attached; callers that
// want runtime teardown on Uninstall must call SetDemolisher.
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

// SetDemolisher attaches a Demolisher (typically *Lifecycle) so
// Uninstall calls runtime stop/remove + image drop before deleting
// the DB rows. Pass nil to revert to the phase-1 behaviour.
func (i *Installer) SetDemolisher(d Demolisher) {
	i.demolisher = d
}

// SetTierProvider attaches the tier subsystem so Install can run
// preflight and resolve tier-bound paths. Pass nil to keep the
// phase-1 "<unresolved>" sentinel behaviour. Statfser is optional;
// nil means use the production StatAvail.
func (i *Installer) SetTierProvider(tp TierProvider, statfs Statfser) {
	i.tierProvider = tp
	i.statfs = statfs
}

// Install is the convenience form of InstallWithOptions with no
// options — kept for backward compatibility with phase 1 callers.
func (i *Installer) Install(yamlBytes []byte) (*PluginRecord, error) {
	return i.InstallWithOptions(yamlBytes, InstallOptions{})
}

// InstallWithOptions parses the YAML, validates it, runs preflight
// for tier-bound volumes (when a TierProvider is attached), fans out
// per-instance host paths, mkdirs both flat and tier-bound dirs,
// and persists the result atomically. The original YAML bytes are
// stored verbatim so the UI can re-display the operator's input.
//
// When no TierProvider is set, tier-bound volumes get the phase-1
// behaviour: empty host_path and a "<unresolved>" tier_pool
// sentinel. With a TierProvider attached, every tier-bound volume
// must resolve through opts.Tiers; preflight failures abort the
// install before any filesystem work happens.
//
// Returns ErrPluginExists if the plugin name is already taken.
// Returns *ValidationError on field-level manifest problems.
// Returns *PreflightError when phase-03 preflight rejects a volume.
func (i *Installer) InstallWithOptions(yamlBytes []byte, opts InstallOptions) (*PluginRecord, error) {
	m, err := ParseManifest(yamlBytes)
	if err != nil {
		return nil, err
	}
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}

	count := m.EffectiveCount()

	// Tier resolution + preflight, only when a provider is wired.
	// resolveVolumePaths owns the actual per-instance path + mkdir
	// generation; preflight gives us the resolved pool name (for
	// the DB row) and the canonical single-instance host path
	// (which we use as the parent for per-instance fan-out).
	tierPools := map[string]string{}
	tierBases := map[string]string{}
	if i.tierProvider != nil {
		preflight, err := PreflightTierAssignments(i.tierProvider, i.statfs, m, opts.Tiers, i.pluginsRoot)
		if err != nil {
			return nil, fmt.Errorf("preflight: %w", err)
		}
		if !preflight.OK {
			return nil, &PreflightError{Result: preflight}
		}
		for _, p := range preflight.Placements {
			if p.Pool != "" {
				tierPools[p.Volume] = p.Pool
				// p.HostPath is the canonical single-instance form
				// (.../<volume>); for per-instance fan-out we need
				// the parent (.../<plugin-name>/) so we can splice
				// in instance-N. Strip the volume name suffix.
				tierBases[p.Volume] = stripVolumeSuffix(p.HostPath, p.Volume)
			}
		}
	}

	paths, mkdirs, err := i.resolveVolumePaths(m, count, tierBases)
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

	if err := i.store.Insert(InsertParams{Manifest: m, Paths: paths, Tiers: tierPools}); err != nil {
		return nil, err
	}
	if err := i.store.SetManifestYAML(m.Metadata.Name, string(yamlBytes)); err != nil {
		return nil, fmt.Errorf("store raw manifest: %w", err)
	}

	// Phase 07: when the manifest opts into bearer-injected auth,
	// generate the per-plugin token now so the nginx route written
	// by Lifecycle.Start (phase 04) has a token to inject. The
	// token rides as the Authorization header on every proxied
	// request to the plugin.
	if m.UI != nil && m.UI.Embed.Auth == AuthBearerInjected {
		if _, err := i.store.IssueBearerToken(m.Metadata.Name); err != nil {
			return nil, fmt.Errorf("issue bearer token: %w", err)
		}
	}

	return i.store.Get(m.Metadata.Name)
}

// PreflightError wraps a failed PreflightResult so callers can
// errors.As() it and surface the per-volume placements (with
// errors and warnings) to the UI.
type PreflightError struct {
	Result *PreflightResult
}

func (e *PreflightError) Error() string {
	var msg string
	for _, p := range e.Result.Placements {
		for _, e := range p.Errors {
			if msg != "" {
				msg += "; "
			}
			msg += p.Volume + ": " + e
		}
	}
	if msg == "" {
		return "preflight failed"
	}
	return "preflight failed: " + msg
}

// resolveVolumePaths computes the per-instance host_path map plus
// the list of directories that need creating on disk.
//
// tierBases maps volume name → the per-plugin parent under that
// volume's tier mount (e.g. "/mnt/media/.plugins/llama-cpp"). The
// per-instance fan-out splices an "instance-N" segment between
// that parent and the volume name. Tier-bound volumes NOT in
// tierBases get empty paths (no tier provider wired, phase-1
// fallback) and need a reinstall to fix.
func (i *Installer) resolveVolumePaths(m *Manifest, count int, tierBases map[string]string) (
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
			base := tierBases[vol.Name]
			if base == "" {
				// No tier provider configured (phase-1 caller) —
				// keep the empty-path sentinel so the schema row
				// still satisfies (plugin, volume, instance).
				if vol.PerInstance {
					for inst := 1; inst <= count; inst++ {
						entries[inst] = ""
					}
				} else {
					entries[1] = ""
				}
				break
			}
			// Resolved: real per-instance paths under the tier mount.
			if vol.PerInstance {
				for inst := 1; inst <= count; inst++ {
					p := filepath.Join(base, fmt.Sprintf("instance-%d", inst), vol.Name)
					entries[inst] = p
					mkdirs = append(mkdirs, p)
				}
			} else {
				p := filepath.Join(base, vol.Name)
				entries[1] = p
				mkdirs = append(mkdirs, p)
			}
		default:
			return nil, nil, fmt.Errorf("resolveVolumePaths: unknown mode %q for volume %q", vol.Mode, vol.Name)
		}
		paths[vol.Name] = entries
	}
	return paths, mkdirs, nil
}

// stripVolumeSuffix returns the parent path of a tier-bound volume's
// canonical (single-instance) host path, used as the splicing base
// for per-instance fan-out. Just removes the trailing "/<volume>"
// segment.
func stripVolumeSuffix(hostPath, volumeName string) string {
	suffix := "/" + volumeName
	if len(hostPath) > len(suffix) && hostPath[len(hostPath)-len(suffix):] == suffix {
		return hostPath[:len(hostPath)-len(suffix)]
	}
	return hostPath
}

// Uninstall removes the plugin and every cascading row + directory
// it owns. When a Demolisher has been attached (phase 02+), it is
// called first to stop/remove containers and drop the cached image
// — that step needs the plugin_instances rows that the DB delete
// would otherwise wipe out, so the order is:
//
//  1. Demolisher.Demolish — runtime teardown (skipped if nil)
//  2. removeAll volume dirs — filesystem teardown
//  3. best-effort parent dir cleanup
//  4. store.Delete          — DB rows (cascades all child tables)
//
// Tier-bound volume teardown still lives in phase 03; nginx + bridge
// teardown lives in phase 04. Returns ErrPluginNotFound when no
// plugin matches.
func (i *Installer) Uninstall(name string) error {
	// Snapshot which directories we own before teardown. Keeping the
	// DB row until filesystem deletion succeeds makes uninstall
	// retryable if a volume cannot be removed.
	rec, err := i.store.Get(name)
	if err != nil {
		return err
	}

	// Phase 02: runtime teardown happens before the DB delete because
	// Demolish needs the per-instance container_id rows. If the runtime
	// daemon is unreachable Demolish returns an error; the operator can
	// retry uninstall after the daemon is back.
	if i.demolisher != nil {
		if err := i.demolisher.Demolish(context.Background(), name); err != nil {
			return fmt.Errorf("runtime teardown: %w", err)
		}
	}

	// Remove every volume's per-instance host path — flat under
	// /var/lib/smoothnas/plugins/, tier-bound under
	// /mnt/<pool>/.plugins/<name>/. Empty paths (a tier-bound
	// volume that was never resolved) are skipped.
	// Collect the per-plugin parent dirs under each tier mount by
	// walking back from any resolved host path. This avoids hard-
	// coding "/mnt/<pool>/..." since the actual tier mountpoint is
	// whatever the tier subsystem chose.
	tierParents := map[string]struct{}{}
	for _, vol := range rec.Volumes {
		for _, host := range vol.Paths {
			if host == "" {
				continue
			}
			if err := i.removeAll(host); err != nil {
				return fmt.Errorf("remove %s: %w", host, err)
			}
			if vol.Mode == VolumeModeTierBound {
				if parent := tierPluginParent(host, vol.PerInstance, vol.Name); parent != "" {
					tierParents[parent] = struct{}{}
				}
			}
		}
	}

	// Remove per-plugin parent directories. Best effort — leftover
	// content (operator junk, race with another writer) is fine.
	flatParent := filepath.Join(i.pluginsRoot, name)
	_ = os.Remove(flatParent) //nolint:errcheck // best-effort cleanup
	for tierParent := range tierParents {
		_ = os.RemoveAll(tierParent) //nolint:errcheck // best-effort cleanup
	}

	if err := i.store.Delete(name); err != nil {
		return err
	}

	return nil
}

// tierPluginParent walks back from a resolved volume host path to
// the per-plugin parent under the tier mount, so Uninstall can rm
// it without needing to know the tier subsystem's mountpoint
// convention. Walks back two segments for perInstance volumes
// (.../instance-N/<vol>) and one for shared (.../<vol>).
func tierPluginParent(hostPath string, perInstance bool, volumeName string) string {
	cleaned := filepath.Clean(hostPath)
	parent := filepath.Dir(cleaned) // strips <vol>
	if perInstance {
		parent = filepath.Dir(parent) // strips instance-N
	}
	if parent == "/" || parent == "." {
		return ""
	}
	return parent
}
