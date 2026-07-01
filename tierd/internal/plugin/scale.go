package plugin

import (
	"context"
	"errors"
	"fmt"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Errors specific to instance scaling. Wired into the API layer so the
// HTTP handlers can map them to stable error codes for the UI.
var (
	// ErrPluginNotConfigurable rejects POST /instances when the
	// installed manifest declared instances.configurable: false. The
	// only path forward for the operator is reinstalling.
	ErrPluginNotConfigurable = errors.New("plugin instances are not configurable")

	// ErrScaleAcrossSingletonBoundary rejects scale operations that
	// cross 1↔N. Single-instance plugins use the bare plugin name as
	// their container name (ContainerName payload.go:17); multi-
	// instance plugins use <name>-<n>. Crossing the boundary would
	// require renaming the existing instance-1 container, which the
	// runtime daemon doesn't expose cleanly. v1 punts: scale among
	// counts ≥ 2, otherwise reinstall with a different count.
	ErrScaleAcrossSingletonBoundary = errors.New("cannot scale across the 1↔N boundary; reinstall with the desired count")

	// ErrScaleTargetInvalid rejects negative / zero counts.
	ErrScaleTargetInvalid = errors.New("scale target must be ≥ 1")
)

// ScaleResult describes the outcome of Lifecycle.Scale: whether the
// count actually changed, what the previous and new counts are, and
// (for scale-down) the per-instance numbers that were removed.
//
// The handler returns this verbatim so the UI's confirmation toast
// can list "removed instances 3, 4".
type ScaleResult struct {
	From    int   `json:"from"`
	To      int   `json:"to"`
	Added   []int `json:"added,omitempty"`
	Removed []int `json:"removed,omitempty"`
	NoOp    bool  `json:"noOp,omitempty"`
}

// Scale brings the named plugin to the target instance count. Returns
// ErrPluginNotFound if the plugin does not exist;
// ErrPluginNotConfigurable when the manifest disallows scaling; or
// ErrScaleAcrossSingletonBoundary / ErrScaleTargetInvalid for
// disallowed targets. Runtime errors during materialise / start
// surface verbatim with their natural wrap.
//
// Idempotent: target == current is reported as a no-op without any
// side effects.
//
// Failure mode: scale-up that fails partway through (mkdir, DB
// insert, container create, container start) reverts the runtime
// side-effects produced so far (best-effort) before returning the
// underlying error. The DB always ends up in a consistent state —
// either all new rows committed, or none. Container materialisation
// happens after the DB commit, so a failure there leaves rows
// pointing at non-existent containers; a follow-up Materialise
// re-creates them. This matches how install.go separates DB inserts
// from runtime materialisation.
func (l *Lifecycle) Scale(ctx context.Context, name string, target int) (*ScaleResult, error) {
	if target < 1 {
		return nil, ErrScaleTargetInvalid
	}

	rec, err := l.store.Get(name)
	if err != nil {
		return nil, err
	}
	if !rec.Plugin.InstanceConfigurable {
		return nil, ErrPluginNotConfigurable
	}
	if l.isCompose(rec) {
		return l.scaleCompose(ctx, rec, target)
	}
	current := rec.Plugin.InstanceCount

	switch {
	case target == current:
		return &ScaleResult{From: current, To: target, NoOp: true}, nil
	case (current == 1) != (target == 1):
		return nil, ErrScaleAcrossSingletonBoundary
	case target > current:
		return l.scaleUp(ctx, rec, target)
	default:
		return l.scaleDown(ctx, rec, target)
	}
}

// scaleUp adds (target - current) instances to the plugin: computes
// new per-instance volume paths, makes their host directories,
// inserts the DB rows in one transaction, and materialises +
// optionally starts the new containers. On a runtime-side failure
// after the DB commit, rolls back the DB rows + filesystem dirs so
// the caller's original count is preserved.
func (l *Lifecycle) scaleUp(ctx context.Context, rec *PluginRecord, target int) (*ScaleResult, error) {
	current := rec.Plugin.InstanceCount
	added := make([]int, 0, target-current)
	for i := current + 1; i <= target; i++ {
		added = append(added, i)
	}

	// Compute per-volume host paths for each new instance. For a
	// per-instance volume the path follows the existing instance-1
	// pattern with the segment swapped; shared volumes have no new
	// path to create — the existing path is reused.
	newPaths, mkdirs, err := computeNewInstancePaths(rec, added)
	if err != nil {
		return nil, err
	}

	// Make the new dirs first. Idempotent — a leftover from a
	// previous failed scale-up is fine.
	createdDirs := make([]string, 0, len(mkdirs))
	for _, dir := range mkdirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			// Best-effort cleanup of dirs we did create on this attempt.
			for _, c := range createdDirs {
				_ = os.Remove(c)
			}
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
		createdDirs = append(createdDirs, dir)
	}

	// DB transaction: insert new plugin_instances + plugin_volume_paths
	// rows and bump plugins.instance_count atomically.
	if err := l.store.AddInstanceRows(rec.Plugin.Name, target, added, newPaths); err != nil {
		// Roll back the dirs since the DB rejected the bump.
		for _, c := range createdDirs {
			_ = os.RemoveAll(c)
		}
		return nil, fmt.Errorf("add instance rows: %w", err)
	}

	// Materialise new containers. On failure, undo: remove any
	// containers we did create, drop the DB rows, rmdir the dirs.
	if err := l.materialiseNewInstances(ctx, rec.Plugin.Name, added); err != nil {
		_ = l.rollbackScaleUp(ctx, rec.Plugin.Name, current, added, createdDirs)
		return nil, fmt.Errorf("materialise new instances: %w", err)
	}

	// If the plugin was already running, start the new containers and
	// re-apply the nginx route. Other states (stopped, installed) are
	// honoured: we leave the new containers in their post-create state
	// and the next Start will pick them up alongside the old ones.
	if rec.Plugin.State == StateRunning {
		if err := l.startNewInstances(ctx, rec.Plugin.Name, added); err != nil {
			_ = l.rollbackScaleUp(ctx, rec.Plugin.Name, current, added, createdDirs)
			return nil, fmt.Errorf("start new instances: %w", err)
		}
		if l.proxy != nil {
			if err := l.ApplyRouteFor(ctx, rec.Plugin.Name); err != nil {
				return nil, fmt.Errorf("re-apply nginx route: %w", err)
			}
		}
	}

	return &ScaleResult{From: current, To: target, Added: added}, nil
}

// scaleDown removes the top-numbered (current..target+1) instances,
// stopping + removing their containers and rmdir-ing their volume
// dirs. Best-effort on the runtime side — a missing container or
// already-removed volume dir is treated as already-gone, not an
// error. The DB delete + count update is transactional.
func (l *Lifecycle) scaleDown(ctx context.Context, rec *PluginRecord, target int) (*ScaleResult, error) {
	current := rec.Plugin.InstanceCount
	removed := make([]int, 0, current-target)
	for i := current; i > target; i-- {
		removed = append(removed, i)
	}

	// Stop + remove the doomed instances' containers across every
	// service. Best-effort — we continue past missing containers,
	// otherwise a half-failed scale-down could strand the operator.
	removedSet := map[int]struct{}{}
	for _, n := range removed {
		removedSet[n] = struct{}{}
	}
	for _, inst := range rec.Instances {
		if _, ok := removedSet[inst.Instance]; !ok || inst.ContainerID == "" {
			continue
		}
		_ = l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds)
		_ = l.removeContainerWithCleanup(ctx, inst.ContainerID, true)
	}

	// Snapshot volume dirs before the DB rows go (they're our only
	// record of the per-instance host paths).
	dirsToRemove := perInstanceDirs(rec, removed)

	if err := l.store.RemoveInstanceRows(rec.Plugin.Name, target, removed); err != nil {
		return nil, fmt.Errorf("remove instance rows: %w", err)
	}

	for _, d := range dirsToRemove {
		_ = os.RemoveAll(d)
	}

	if rec.Plugin.State == StateRunning && l.proxy != nil {
		// Re-apply the nginx route so it routes only to the surviving
		// instances. v1 only routes to instance 1 anyway, so this is a
		// no-op unless instance 1 was somehow removed (which it can't
		// be — scale-down peels off the top).
		if err := l.ApplyRouteFor(ctx, rec.Plugin.Name); err != nil {
			return nil, fmt.Errorf("re-apply nginx route: %w", err)
		}
	}

	return &ScaleResult{From: current, To: target, Removed: removed}, nil
}

// rollbackScaleUp undoes a partially-applied scale-up: removes any
// containers that did get created, deletes the DB rows for the
// `added` instances, and rmdirs the per-instance dirs we made on
// this attempt. Used by scaleUp when materialisation or start
// fails. Caller already logged the underlying error.
func (l *Lifecycle) rollbackScaleUp(ctx context.Context, name string, originalCount int, added []int, createdDirs []string) error {
	// Remove containers (no-op if none were created yet).
	rec, err := l.store.Get(name)
	if err == nil {
		for _, instNum := range added {
			for _, inst := range rec.Instances {
				if inst.Instance != instNum || inst.ContainerID == "" {
					continue
				}
				_ = l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds)
				_ = l.removeContainerWithCleanup(ctx, inst.ContainerID, true)
			}
		}
	}
	// DB rows.
	if err := l.store.RemoveInstanceRows(name, originalCount, added); err != nil {
		return fmt.Errorf("rollback remove instance rows: %w", err)
	}
	for _, d := range createdDirs {
		_ = os.RemoveAll(d)
	}
	return nil
}

// materialiseNewInstances creates containers for each of the newly-
// inserted instance rows. Re-uses Lifecycle.Materialise's machinery
// by routing through it: Materialise iterates plugin_instances and
// skips ones that already have a container_id, so calling it after
// AddInstanceRows naturally creates only the new instances.
func (l *Lifecycle) materialiseNewInstances(ctx context.Context, name string, _ []int) error {
	return l.Materialise(ctx, name)
}

// startNewInstances Start()s only the per-instance containers for
// the supplied instance numbers. Distinct from Lifecycle.Start (which
// touches every instance + re-applies the nginx route as one
// transaction) so a scale-up of an already-running plugin doesn't
// stop-and-restart the existing instances.
func (l *Lifecycle) startNewInstances(ctx context.Context, name string, added []int) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	addedSet := map[int]struct{}{}
	for _, n := range added {
		addedSet[n] = struct{}{}
	}
	// Start new replicas of every service. They were created by
	// Materialise with discovery env pointed at instance-1 of each
	// dependency, so a direct start is sufficient.
	for _, inst := range rec.Instances {
		if _, ok := addedSet[inst.Instance]; !ok {
			continue
		}
		if inst.ContainerID == "" {
			return fmt.Errorf("service %s instance %d has no container — materialise first", inst.Service, inst.Instance)
		}
		_ = l.setInstanceState(name, inst.Service, inst.Instance, StateStarting, "")
		if err := l.rt.StartContainer(ctx, inst.ContainerID); err != nil {
			_ = l.setInstanceState(name, inst.Service, inst.Instance, StateFailed, err.Error())
			return fmt.Errorf("start %s/%d: %w", inst.Service, inst.Instance, err)
		}
		if ip, err := l.captureBridgeIP(ctx, inst.ContainerID); err != nil {
			_ = l.setInstanceState(name, inst.Service, inst.Instance, StateFailed, fmt.Sprintf("bridge IP: %v", err))
			return fmt.Errorf("capture bridge IP for %s/%d: %w", inst.Service, inst.Instance, err)
		} else if ip != "" {
			_ = l.store.SetInstanceBridgeIP(name, inst.Service, inst.Instance, ip)
		}
		_ = l.setInstanceState(name, inst.Service, inst.Instance, StateRunning, "")
	}
	return nil
}

// computeNewInstancePaths derives per-volume host paths for each new
// instance number from the existing record's volume rows. For
// per-instance volumes the path follows the established
// "<base>/instance-N/<volume-name>" pattern with the instance segment
// swapped. Shared volumes (per_instance: false) emit no entry — they
// keep their existing single-path row.
//
// Returns:
//   - paths[volumeName][instanceNum] = host path  (only per-instance volumes)
//   - mkdirs: ordered list of new dirs to create on disk
func computeNewInstancePaths(rec *PluginRecord, addedInstances []int) (
	paths map[string]map[string]map[int]string,
	mkdirs []string,
	err error,
) {
	paths = map[string]map[string]map[int]string{}
	for _, vol := range rec.Volumes {
		if !vol.PerInstance {
			continue
		}
		// Find an existing instance's path on this volume to use as
		// the template. Instance 1 is always present for installed
		// plugins.
		var template string
		var templateInst int
		for inst, p := range vol.Paths {
			if p == "" {
				continue
			}
			template = p
			templateInst = inst
			break
		}
		if template == "" {
			// Tier-bound volume that was never resolved (phase-1
			// install with no tier provider). The operator must
			// reinstall to fix; we can't fan out to new instances
			// without a base path.
			return nil, nil, fmt.Errorf("volume %q/%q has no resolved host path; reinstall the plugin with a tier assignment before scaling", vol.Service, vol.Name)
		}
		if paths[vol.Service] == nil {
			paths[vol.Service] = map[string]map[int]string{}
		}
		paths[vol.Service][vol.Name] = map[int]string{}
		for _, instNum := range addedInstances {
			p, err := swapInstanceSegment(template, templateInst, instNum)
			if err != nil {
				return nil, nil, fmt.Errorf("derive path for volume %q/%q instance %d: %w", vol.Service, vol.Name, instNum, err)
			}
			paths[vol.Service][vol.Name][instNum] = p
			mkdirs = append(mkdirs, p)
		}
	}
	return paths, mkdirs, nil
}

// swapInstanceSegment replaces the "instance-<old>" path segment with
// "instance-<new>" in a per-instance volume host path. Returns an
// error if the segment isn't present (which would mean the path was
// computed by a stale pre-scaling code path that didn't fan out per
// instance — shouldn't happen).
func swapInstanceSegment(path string, oldN, newN int) (string, error) {
	oldSeg := fmt.Sprintf("instance-%d", oldN)
	newSeg := fmt.Sprintf("instance-%d", newN)
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := range parts {
		if parts[i] == oldSeg {
			parts[i] = newSeg
			return strings.Join(parts, string(filepath.Separator)), nil
		}
	}
	return "", fmt.Errorf("path %q has no %q segment", path, oldSeg)
}

// perInstanceDirs returns every per-instance volume host path
// belonging to the supplied instance numbers — Scale-down's
// rmdir list.
func perInstanceDirs(rec *PluginRecord, removed []int) []string {
	removedSet := map[int]struct{}{}
	for _, n := range removed {
		removedSet[n] = struct{}{}
	}
	var out []string
	for _, vol := range rec.Volumes {
		if !vol.PerInstance {
			continue
		}
		for inst, p := range vol.Paths {
			if _, ok := removedSet[inst]; !ok {
				continue
			}
			if p == "" {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// AddInstanceRows extends an existing plugin's instance count by
// inserting the supplied per-instance plugin_instances and
// plugin_volume_paths rows in one transaction. addedInstances and
// volumePaths must be consistent with each other; volumePaths is
// keyed (volume name → instance num → host path) and only includes
// per-instance volumes (shared volumes have no new path).
//
// Returns ErrPluginNotFound when the named plugin does not exist. New
// instance rows are created for every service; volumePaths is keyed
// service → volume name → instance num → host path (per-instance volumes
// only — shared volumes have no new path).
func (s *Store) AddInstanceRows(name string, newCount int, addedInstances []int, volumePaths map[string]map[string]map[int]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Verify the plugin exists before doing anything.
	var existed int
	if err := tx.QueryRow(`SELECT 1 FROM plugins WHERE name = ?`, name).Scan(&existed); err != nil {
		return ErrPluginNotFound
	}

	// Every service gains the new replica instances.
	svcRows, err := tx.Query(`SELECT service FROM plugin_services WHERE plugin_name = ?`, name)
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	var services []string
	for svcRows.Next() {
		var svc string
		if err := svcRows.Scan(&svc); err != nil {
			svcRows.Close()
			return err
		}
		services = append(services, svc)
	}
	svcRows.Close()
	if err := svcRows.Err(); err != nil {
		return err
	}

	for _, svc := range services {
		for _, instNum := range addedInstances {
			if _, err := tx.Exec(
				`INSERT INTO plugin_instances (plugin_name, service, instance, state)
				 VALUES (?, ?, ?, ?)`,
				name, svc, instNum, StateInstalled,
			); err != nil {
				return fmt.Errorf("insert plugin_instances[%s/%d]: %w", svc, instNum, err)
			}
		}
	}

	for svc, vols := range volumePaths {
		for volName, perInst := range vols {
			for instNum, hostPath := range perInst {
				if _, err := tx.Exec(
					`INSERT INTO plugin_volume_paths
					 (plugin_name, service, volume_name, instance, host_path)
					 VALUES (?, ?, ?, ?, ?)`,
					name, svc, volName, instNum, hostPath,
				); err != nil {
					return fmt.Errorf("insert plugin_volume_paths[%s/%s/%d]: %w", svc, volName, instNum, err)
				}
			}
		}
	}

	if _, err := tx.Exec(
		`UPDATE plugins SET instance_count = ?, updated_at = datetime('now') WHERE name = ?`,
		newCount, name,
	); err != nil {
		return fmt.Errorf("update plugins.instance_count: %w", err)
	}

	if err := recomputeAggregateStateTx(tx, name); err != nil {
		return fmt.Errorf("recompute state: %w", err)
	}

	return tx.Commit()
}

// RemoveInstanceRows deletes the supplied instance numbers'
// plugin_instances and plugin_volume_paths rows in one transaction
// and updates plugins.instance_count to newCount.
//
// Returns ErrPluginNotFound when the named plugin does not exist.
func (s *Store) RemoveInstanceRows(name string, newCount int, removedInstances []int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var existed int
	if err := tx.QueryRow(`SELECT 1 FROM plugins WHERE name = ?`, name).Scan(&existed); err != nil {
		return ErrPluginNotFound
	}

	for _, instNum := range removedInstances {
		if _, err := tx.Exec(
			`DELETE FROM plugin_instances WHERE plugin_name = ? AND instance = ?`,
			name, instNum,
		); err != nil {
			return fmt.Errorf("delete plugin_instances[%d]: %w", instNum, err)
		}
		if _, err := tx.Exec(
			`DELETE FROM plugin_volume_paths WHERE plugin_name = ? AND instance = ?`,
			name, instNum,
		); err != nil {
			return fmt.Errorf("delete plugin_volume_paths[%d]: %w", instNum, err)
		}
	}

	if _, err := tx.Exec(
		`UPDATE plugins SET instance_count = ?, updated_at = datetime('now') WHERE name = ?`,
		newCount, name,
	); err != nil {
		return fmt.Errorf("update plugins.instance_count: %w", err)
	}

	if err := recomputeAggregateStateTx(tx, name); err != nil {
		return fmt.Errorf("recompute state: %w", err)
	}

	return tx.Commit()
}

// composeScaleLocks serializes scale (regenerate compose + reconcile up) per
// plugin, so two concurrent scales can't half-write the project (roundtable).
var composeScaleLocks sync.Map // plugin name -> *sync.Mutex

func pluginScaleLock(name string) *sync.Mutex {
	mu, _ := composeScaleLocks.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// scaleCompose changes a compose plugin's instance count: validate against the
// scalable service's min/max, update the count, re-materialise (which regenerates
// the expanded N-service compose), then `up --remove-orphans` to reconcile —
// starting new per-instance services and removing scaled-down ones. Scaled-down
// instances' tier-bound _work binds persist (pins retained), so scaling back up
// reuses the same data. Serialized per-plugin.
func (l *Lifecycle) scaleCompose(ctx context.Context, rec *PluginRecord, target int) (*ScaleResult, error) {
	name := rec.Plugin.Name
	mu := pluginScaleLock(name)
	mu.Lock()
	defer mu.Unlock()

	// Re-read under the lock.
	rec, err := l.store.Get(name)
	if err != nil {
		return nil, err
	}
	current := rec.Plugin.InstanceCount
	if target == current {
		return &ScaleResult{From: current, To: target, NoOp: true}, nil
	}
	specs, err := compose.ScalableServices([]byte(rec.Plugin.ManifestYAML))
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("plugin %q has no scalable (x-smoothnas.instances) service", name)
	}
	sp := specs[0]
	if sp.Min > 0 && target < sp.Min {
		return nil, fmt.Errorf("scale target %d is below the declared min %d", target, sp.Min)
	}
	if sp.Max > 0 && target > sp.Max {
		return nil, fmt.Errorf("scale target %d is above the declared max %d", target, sp.Max)
	}

	if err := l.store.SetComposeInstances(name, target, true); err != nil {
		return nil, err
	}
	if err := l.Materialise(ctx, name); err != nil {
		_ = l.store.SetComposeInstances(name, current, true) // roll the count back
		return nil, fmt.Errorf("scale materialise: %w", err)
	}
	rec2, err := l.store.Get(name)
	if err != nil {
		return nil, err
	}
	spec := l.composeSpec(rec2)
	if secrets, err := l.store.GetComposeSecrets(name); err == nil && len(secrets) > 0 {
		spec.SecretEnv = secrets
	}
	if err := l.backend.StartScaled(ctx, spec); err != nil {
		return nil, fmt.Errorf("scale reconcile up: %w", err)
	}
	l.syncComposeState(ctx, name, rec2)

	res := &ScaleResult{From: current, To: target}
	if target > current {
		for i := current + 1; i <= target; i++ {
			res.Added = append(res.Added, i)
		}
	} else {
		for i := target + 1; i <= current; i++ {
			res.Removed = append(res.Removed, i)
		}
	}
	return res, nil
}
