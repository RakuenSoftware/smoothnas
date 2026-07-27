package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// RuntimeClient is the subset of *runtime.Client the lifecycle uses.
// Defined as an interface so unit tests can supply a fake without
// standing up a real socket-backed daemon.
type RuntimeClient interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (runtime.Info, error)

	PullImage(ctx context.Context, ref string, onProgress func(runtime.PullEvent)) (string, error)
	RemoveImage(ctx context.Context, ref string) error
	ListImages(ctx context.Context) ([]runtime.ImageSummary, error)

	CreateContainer(ctx context.Context, name string, req runtime.CreateContainerRequest) (runtime.CreateContainerResponse, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeoutSeconds int) error
	RestartContainer(ctx context.Context, id string, timeoutSeconds int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (runtime.ContainerInspect, error)
	ListManagedContainers(ctx context.Context) ([]runtime.ContainerSummary, error)
	WaitContainer(ctx context.Context, id string) (int, error)
	CommitContainer(ctx context.Context, containerID, repo, tag string) (string, error)

	StreamLogs(ctx context.Context, id string, opts runtime.LogsOptions) (io.ReadCloser, error)
	SubscribeEvents(ctx context.Context) (<-chan runtime.Event, <-chan error, error)

	// Phase 04: bridge network management.
	EnsurePluginBridge(ctx context.Context) (string, error)
	InspectContainerBridgeIP(ctx context.Context, id string) (string, error)
}

// ProxyManager is the subset of *Proxy the lifecycle uses. Defined
// as an interface so lifecycle tests can pass a recording fake
// instead of touching /etc/nginx.
type ProxyManager interface {
	Apply(ctx context.Context, route PluginRoute) error
	Remove(ctx context.Context, pluginName string) error
}

// Default container stop timeout (seconds). The runtime SIGTERMs and
// then SIGKILLs after this many seconds.
const DefaultStopTimeoutSeconds = 10

var (
	imagePullMaxAttempts = 3
	imagePullRetryDelay  = 5 * time.Second
	// serviceHealthMaxAttempts/Delay bound the best-effort readiness
	// wait for a dependency declared with condition: service_healthy.
	// LXC2Docker does not surface healthcheck-command results, so the
	// gate currently waits for the container to report Running; true
	// healthcheck polling is a follow-up once the runtime exposes it.
	serviceHealthMaxAttempts = 30
	serviceHealthRetryDelay  = 1 * time.Second
)

// Lifecycle owns the install/start/stop/uninstall flow that touches
// the runtime daemon. A plugin owns a set of services (compose-style);
// Lifecycle brings them up in dependency (ordinal) order, injecting
// service-discovery addresses as each backend comes online.
type Lifecycle struct {
	store   *Store
	rt      RuntimeClient
	lxcPath string
	proxy   ProxyManager
	catalog *Catalog
	backend *compose.Backend // plugins-11: compose-format plugins route here
	tier    TierProvider     // plugins-11 Phase 2: compose tiered-volume resolution
	// ready reports whether the underlying container runtime has been confirmed
	// reachable. Defaults true (a directly-constructed Lifecycle is assumed
	// usable); the daemon's startup path flips it false and back to true from a
	// background goroutine so waiting on the runtime never blocks HTTP readiness.
	ready atomic.Bool
}

// NewLifecycle constructs a Lifecycle around an existing Store and a
// runtime client.
func NewLifecycle(s *Store, rt RuntimeClient) *Lifecycle {
	l := &Lifecycle{store: s, rt: rt}
	l.ready.Store(true) // usable by default; the daemon defers this during boot
	return l
}

// SetRuntimeReady records whether the container runtime has been confirmed
// reachable. The daemon sets it false at construction and true once a
// background probe succeeds, so plugin verbs return 503 (not a runtime-call
// error) while the runtime is still warming up.
func (l *Lifecycle) SetRuntimeReady(ready bool) {
	if l != nil {
		l.ready.Store(ready)
	}
}

// RuntimeReady reports whether the container runtime is confirmed reachable.
// A nil Lifecycle is never ready.
func (l *Lifecycle) RuntimeReady() bool {
	return l != nil && l.ready.Load()
}

// SetProxy attaches the nginx route manager.
func (l *Lifecycle) SetProxy(p ProxyManager) { l.proxy = p }

// SetCatalog attaches the profile catalog.
func (l *Lifecycle) SetCatalog(c *Catalog) { l.catalog = c }

// SetComposeBackend attaches the compose backend (plugins-11). When set,
// compose-format plugins route their Materialise/Start/Demolish through it
// instead of the manifest BuildCreatePayload path.
func (l *Lifecycle) SetComposeBackend(b *compose.Backend) { l.backend = b }

// SetTierProvider attaches the tier subsystem so compose plugins' x-smoothnas
// tiered volumes resolve to smoothfs host paths (Phase 2).
func (l *Lifecycle) SetTierProvider(tp TierProvider) { l.tier = tp }

// ComposeServices returns the live per-service `compose ps` for a compose plugin
// (the project rollup plus each service's state/health/published ports) — the
// data a UI renders. Errors for a non-compose plugin (Phase 4).
func (l *Lifecycle) ComposeServices(ctx context.Context, name string) (compose.Status, error) {
	rec, err := l.store.Get(name)
	if err != nil {
		return compose.Status{}, err
	}
	if !l.isCompose(rec) {
		return compose.Status{}, fmt.Errorf("plugin %q is not a compose plugin", name)
	}
	return l.backend.Status(ctx, l.composeSpec(rec))
}

// ComposeLogs returns the tail of a compose plugin's aggregated logs (Phase 4).
func (l *Lifecycle) ComposeLogs(ctx context.Context, name string, tail int) ([]byte, error) {
	rec, err := l.store.Get(name)
	if err != nil {
		return nil, err
	}
	if !l.isCompose(rec) {
		return nil, fmt.Errorf("plugin %q is not a compose plugin", name)
	}
	return l.backend.Logs(ctx, l.composeSpec(rec), tail)
}

// composeSpecResolved builds the compose ProjectSpec with x-smoothnas tiered
// volumes resolved to smoothfs host binds (mechanism B). Used at Materialise
// (write time); the rewritten compose is what gets written to disk, so
// Start/Stop/Status run against the bound project. Resolution errors (missing/
// unhealthy tier) block Materialise before any container is created.
func (l *Lifecycle) composeSpecResolved(rec *PluginRecord) (compose.ProjectSpec, error) {
	yamlBytes := []byte(rec.Plugin.ManifestYAML)
	// Phase-5 (gh-runner): expand x-smoothnas.instances services into N discrete
	// per-instance services BEFORE tiered-volume resolution, so each instance's
	// _work volume resolves + pins independently (data-safe across scale).
	specs, err := compose.ScalableServices(yamlBytes)
	if err != nil {
		return compose.ProjectSpec{}, err
	}
	if len(specs) > 0 {
		counts := map[string]int{}
		for _, s := range specs {
			counts[s.Service] = rec.Plugin.InstanceCount
		}
		if yamlBytes, err = compose.ExpandInstances(yamlBytes, counts); err != nil {
			return compose.ProjectSpec{}, err
		}
	}
	tvols, err := compose.TieredVolumes(yamlBytes)
	if err != nil {
		return compose.ProjectSpec{}, err
	}
	if len(tvols) > 0 {
		binds, err := l.resolveAndPinComposeVolumes(rec.Plugin.Name, tvols)
		if err != nil {
			return compose.ProjectSpec{}, err
		}
		// Create each tiered bind-source dir before it is mounted. The native
		// install path mkdirs its volume dirs (see Installer.resolveVolumePaths);
		// the compose flow only REWRITES mounts to these host paths, so without
		// this nothing creates the source and lxc-start aborts with ENOENT on the
		// bind mount ("Failed to mount .../compose/<vol>"). Idempotent; matches the
		// native 0o750.
		for _, hostPath := range binds {
			if err := os.MkdirAll(hostPath, 0o750); err != nil {
				return compose.ProjectSpec{}, fmt.Errorf("create tiered volume dir %s: %w", hostPath, err)
			}
		}
		if yamlBytes, err = compose.RewriteTieredBinds(yamlBytes, binds); err != nil {
			return compose.ProjectSpec{}, err
		}
	}
	spec := compose.SpecFromSingle(rec.Plugin.Name, string(yamlBytes), nil)
	// Render operator config into the compose .env: every non-secret
	// x-smoothnas.config key gets the operator's answer or the schema default,
	// so a ${KEY} reference never resolves empty. Secrets are NOT here — they go
	// to the up-subprocess env via GetComposeSecrets at Start (never on disk).
	env, err := l.composeConfigEnv(rec, yamlBytes)
	if err != nil {
		return compose.ProjectSpec{}, err
	}
	spec.Env = env
	return spec, nil
}

// composeConfigEnv resolves the non-secret operator config for a compose plugin
// into the .env map: the declared x-smoothnas.config schema, filled from the
// stored answers (plugin_compose_config) with schema defaults for unset keys,
// type-validated. Returns nil when the plugin declares no config.
func (l *Lifecycle) composeConfigEnv(rec *PluginRecord, yamlBytes []byte) (map[string]string, error) {
	schema, err := compose.ConfigSchema(yamlBytes)
	if err != nil {
		return nil, err
	}
	if len(schema) == 0 {
		return nil, nil
	}
	answers, err := l.store.GetComposeConfig(rec.Plugin.Name)
	if err != nil {
		return nil, err
	}
	env, _, err := compose.ResolveConfigEnv(schema, answers)
	return env, err
}

// isCompose reports whether a stored plugin is compose-format and a backend is
// wired to handle it. It trusts the ArtifactCompose stamp set at install time
// (every plugin gets a definitive artifact type), so it does no per-call YAML
// re-parse and can never misroute a manifest plugin whose YAML looks compose-y.
func (l *Lifecycle) isCompose(rec *PluginRecord) bool {
	return l.backend != nil && rec.Plugin.ArtifactType == ArtifactCompose
}

// composeSpec builds the compose ProjectSpec from a stored compose plugin. The
// stored "manifest" is the raw compose project; the tierd plugin name is the
// compose -p project name. (Operator config -> .env is a follow-up slice.)
func (l *Lifecycle) composeSpec(rec *PluginRecord) compose.ProjectSpec {
	return compose.SpecFromSingle(rec.Plugin.Name, rec.Plugin.ManifestYAML, nil)
}

// syncComposeState refreshes a compose plugin's CACHED state from compose ps (the
// source of truth) after an op — best-effort. `compose up`/`stop` returning nil
// doesn't prove the rollup, so we read the real state rather than assume
// running/stopped. Status reads this cache (a per-read `compose ps` would spawn an
// unbounded subprocess per UI poll); a periodic reconcile sweep to catch
// out-of-band drift is a follow-up.
func (l *Lifecycle) syncComposeState(ctx context.Context, name string, rec *PluginRecord) {
	if st, err := l.backend.Status(ctx, l.composeSpec(rec)); err == nil {
		_ = l.store.SetPluginState(name, composeOverallToState(st.Overall))
	}
}

// ReconcileComposeStates refreshes every compose plugin's cached state from
// compose ps. The event-based reconciler only tracks io.smoothnas-labelled
// containers; compose plugins carry com.docker.compose labels, so a periodic
// sweep keeps their cached state fresh against out-of-band changes (a container
// crash/restart outside a tierd op). No-op when no backend is wired.
func (l *Lifecycle) ReconcileComposeStates(ctx context.Context) error {
	if l.backend == nil {
		return nil
	}
	rows, err := l.store.List()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ArtifactType != ArtifactCompose {
			continue
		}
		rec, err := l.store.Get(row.Name)
		if err != nil {
			continue
		}
		l.syncComposeState(ctx, row.Name, rec)
	}
	return nil
}

// BackfillComposeImageRefs records service images for compose plugins that were
// installed before image-ref tracking existed (so they carry no container refs
// and never surfaced an Update button). Idempotent: it only touches compose
// plugins with zero refs, parsing images from the stored compose project. Runs
// once at startup after an upgrade; a per-plugin parse/record failure is logged
// and skipped so one bad project can't block the sweep.
func (l *Lifecycle) BackfillComposeImageRefs(_ context.Context) error {
	rows, err := l.store.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if row.ArtifactType != ArtifactCompose {
			continue
		}
		rec, err := l.store.Get(row.Name)
		if err != nil || len(rec.ContainerRefs) > 0 {
			continue // absent, unreadable, or already tracked
		}
		images, err := compose.ServiceImages([]byte(rec.Plugin.ManifestYAML))
		if err != nil || len(images) == 0 {
			continue
		}
		if err := l.store.RecordComposeImages(row.Name, images); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", row.Name, err))
		}
	}
	return errors.Join(errs...)
}

// resolveAndPinComposeVolumes resolves each tiered volume to its host path and
// PINS the placement on first materialise. On a later materialise it reuses the
// pinned path and REFUSES a silent retier (the compose now points at a different
// tier) — a compose edit never relocates tiered data; an explicit REPIN (a
// follow-up operator verb) is required.
func (l *Lifecycle) resolveAndPinComposeVolumes(plugin string, tvols []compose.TieredVolume) (map[string]string, error) {
	pins, err := l.store.GetComposeVolumePins(plugin)
	if err != nil {
		return nil, err
	}
	binds := map[string]string{}
	for _, tv := range tvols {
		resolved, err := ResolveComposeTierVolumes(l.tier, plugin, []compose.TieredVolume{tv})
		if err != nil {
			return nil, err
		}
		cur := resolved[tv.Name]
		if pin, ok := pins[tv.Name]; ok {
			if pin.HostPath != cur {
				return nil, fmt.Errorf("volume %q is pinned to %q (tier %q); the compose now requests tier %q -> %q. "+
					"A compose edit will not silently relocate tiered data — an explicit retier is required",
					tv.Name, pin.HostPath, pin.Pool, tv.Tier, cur)
			}
			binds[tv.Name] = pin.HostPath
			continue
		}
		if err := l.store.PinComposeVolume(plugin, tv.Name, tv.Tier, cur, tv.MinSize); err != nil {
			return nil, err
		}
		binds[tv.Name] = cur
	}
	return binds, nil
}

// composeOverallToState maps a compose-project rollup to a tierd plugin state.
func composeOverallToState(o compose.Overall) string {
	switch o {
	case compose.StateRunning:
		return StateRunning
	case compose.StateDegraded:
		return StateDegraded
	case compose.StateFailed:
		return StateFailed
	default:
		return StateStopped
	}
}

// SetLXCPath attaches the smoothnas-runtime LXC root used for container
// backing directories. Pass an empty string to disable filesystem cleanup.
func (l *Lifecycle) SetLXCPath(path string) { l.lxcPath = strings.TrimSpace(path) }

// --- per-service helpers -------------------------------------------------

// manifestServiceMap indexes a manifest's services by name.
func manifestServiceMap(m *Manifest) map[string]*Service {
	out := make(map[string]*Service, len(m.Services))
	for i := range m.Services {
		out[m.Services[i].Name] = &m.Services[i]
	}
	return out
}

// orderedServices returns the plugin's services in start order (ascending
// ordinal; ties broken by name for determinism).
func orderedServices(rec *PluginRecord) []ServiceRow {
	out := append([]ServiceRow(nil), rec.Services...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// primaryService returns the plugin's user-facing service: the
// highest-ordinal one (it depends on its backends, so it starts last),
// ties broken by smallest name — matching Store.primaryServiceTx.
func primaryService(rec *PluginRecord) string {
	best := ServiceRow{Ordinal: -1}
	for _, s := range rec.Services {
		if s.Ordinal > best.Ordinal || (s.Ordinal == best.Ordinal && (best.Service == "" || s.Service < best.Service)) {
			best = s
		}
	}
	return best.Service
}

func filterInstances(rec *PluginRecord, service string) []InstanceRow {
	var out []InstanceRow
	for _, r := range rec.Instances {
		if r.Service == service {
			out = append(out, r)
		}
	}
	return out
}

func filterVolumes(rec *PluginRecord, service string) []VolumeRow {
	var out []VolumeRow
	for _, r := range rec.Volumes {
		if r.Service == service {
			out = append(out, r)
		}
	}
	return out
}

func filterConfig(rec *PluginRecord, service string) []ConfigRow {
	var out []ConfigRow
	for _, r := range rec.Config {
		if r.Service == service {
			out = append(out, r)
		}
	}
	return out
}

// serviceImageRef returns the resolved image for a service, preferring the
// stored ref and falling back to the manifest's pre-resolution form.
func serviceImageRef(rec *PluginRecord, svc *Service) string {
	for _, sr := range rec.Services {
		if sr.Service == svc.Name && sr.ImageRef != "" {
			return sr.ImageRef
		}
	}
	if svc.Artifact.Type == ArtifactOCIImage {
		return digestPinnedImageRef(svc.Artifact.Image, svc.Artifact.Digest)
	}
	return svc.Artifact.Distro + ":" + svc.Artifact.Release
}

// currentDiscovery seeds the discovery map with each service's stable
// bridge hostname (the container name) and declared ports. The host is
// the container name rather than an IP: LXC2Docker has no embedded DNS,
// so tierd writes /etc/hosts records (name→current IP) into every
// managed container and lets the kernel resolver close the gap. Using
// the name keeps a dependent's injected env (e.g. AIMEE_DB2_URL) valid
// across a backend's IP drift without recreating the dependent.
func currentDiscovery(rec *PluginRecord, svcMap map[string]*Service) map[string]ServiceEndpoint {
	disc := map[string]ServiceEndpoint{}
	for _, sr := range rec.Services {
		svc := svcMap[sr.Service]
		if svc == nil {
			continue
		}
		ep := ServiceEndpoint{
			Host:  ContainerName(rec.Plugin.Name, sr.Service, 1, rec.Plugin.InstanceCount),
			Ports: map[string]int{},
		}
		for _, p := range svc.Ports {
			ep.Ports[p.Name] = p.Port
		}
		disc[sr.Service] = ep
	}
	return disc
}

// --- Materialise ---------------------------------------------------------

// Materialise pulls every service's image (running the lxc-distro setup
// flow where needed) and creates the per-(service,instance) containers,
// stamping the resulting container IDs into plugin_instances. After
// Materialise, the plugin can be Started.
//
// Idempotent: an existing container that still matches the desired
// payload is reused.
func (l *Lifecycle) Materialise(ctx context.Context, name string) error {
	// Serialize Materialise per plugin. It is invoked from many concurrent sites
	// (the plugin API handlers, AutostartAll at daemon startup, scale, and resume),
	// and the create loop below only skips creation when the DB already records a
	// ContainerID for the instance. Two applies that both observe an empty
	// ContainerID each proceed to createInstance; on a runtime that does not reject
	// a duplicate container name (the LXC2Docker shim, unlike real Docker) that
	// yields several containers for one instance — observed as 4x
	// aimee-llm-gpu-mid-llm created in the same second, thrashing the GPU. The lock
	// makes the first apply record the ID so the rest take the idempotent reuse path.
	mu := pluginMaterialiseLock(name)
	mu.Lock()
	defer mu.Unlock()

	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	if l.isCompose(rec) {
		if err := l.checkComposeHostPortConflicts(name, rec.Plugin.ManifestYAML); err != nil {
			return err
		}
		spec, err := l.composeSpecResolved(rec)
		if err != nil {
			return err
		}
		return l.backend.Materialise(ctx, spec)
	}
	manifest, err := ParseManifest([]byte(rec.Plugin.ManifestYAML))
	if err != nil {
		return fmt.Errorf("re-parse stored manifest: %w", err)
	}
	svcMap := manifestServiceMap(manifest)

	// Fail fast (before pulling images) if a host-published port collides with
	// another installed plugin: a hostExpose port is DNAT'd onto the host, so two
	// plugins claiming the same host port silently shadow each other — the runtime
	// publishes both, one wins, and the loser is unreachable though it reports
	// "running". Surface it as a clear lifecycle error instead.
	if err := l.checkHostPortConflicts(name, rec); err != nil {
		return err
	}

	if _, err := l.rt.EnsurePluginBridge(ctx); err != nil {
		return fmt.Errorf("ensure plugin bridge: %w", err)
	}

	var resolved *Resolved
	if l.catalog != nil {
		r, err := Resolve(l.catalog, manifest, nil)
		if err != nil {
			return fmt.Errorf("resolve profiles: %w", err)
		}
		if err := l.store.SetResolvedProfiles(name, r.Names); err != nil {
			return fmt.Errorf("persist resolved profiles: %w", err)
		}
		resolved = r
	}

	count := rec.Plugin.InstanceCount
	disc := currentDiscovery(rec, svcMap)

	for _, sr := range orderedServices(rec) {
		svc := svcMap[sr.Service]
		if svc == nil {
			return fmt.Errorf("stored manifest is missing service %q", sr.Service)
		}

		vols := filterVolumes(rec, sr.Service)
		cfg := filterConfig(rec, sr.Service)
		resolvedRefs, err := l.resolveContainerRefs(ctx, &rec.Plugin, svc, cfg)
		if err != nil {
			return err
		}
		imageRef, err := l.materialiseImage(ctx, &rec.Plugin, svc, resolvedRefs)
		if err != nil {
			return err
		}
		for _, inst := range filterInstances(rec, sr.Service) {
			payload, err := BuildCreatePayload(PayloadInputs{
				Plugin:                  &rec.Plugin,
				Service:                 svc,
				Instance:                inst.Instance,
				ImageRef:                imageRef,
				ContainerRefsGeneration: resolvedRefs.Generation,
				Volumes:                 vols,
				Config:                  cfg,
				Discovery:               disc,
				Profiles:                resolved,
			})
			if err != nil {
				return fmt.Errorf("build payload for %s/%d: %w", sr.Service, inst.Instance, err)
			}

			if inst.ContainerID != "" {
				existing, err := l.rt.InspectContainer(ctx, inst.ContainerID)
				if err == nil && containerMatchesDesired(existing, payload) {
					continue
				}
				if err != nil && !runtime.IsNotFound(err) {
					return fmt.Errorf("inspect existing container for %s/%d: %w", sr.Service, inst.Instance, err)
				}
				if err == nil {
					_ = l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds)
					if err := l.removeContainerWithCleanup(ctx, inst.ContainerID, true); err != nil {
						return fmt.Errorf("remove stale container for %s/%d: %w", sr.Service, inst.Instance, err)
					}
					_ = l.store.SetInstanceContainerID(name, sr.Service, inst.Instance, "")
				}
			}

			if err := l.createInstance(ctx, name, &rec.Plugin, svc, inst.Instance, count, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

// hostPortKey normalises a (port, protocol) pair to the "port/proto" form used
// to publish it on the host (matching BuildCreatePayload). An empty protocol
// defaults to tcp.
func hostPortKey(port int, proto string) string {
	p := strings.ToLower(proto)
	if p == "" {
		p = "tcp"
	}
	return fmt.Sprintf("%d/%s", port, p)
}

// checkHostPortConflicts rejects materialise if any of this plugin's
// host-published (hostExpose) ports is already host-published by a DIFFERENT
// installed plugin. hostExpose publishes the container port on the host (the
// runtime DNATs it), so two plugins on the same host port collide silently —
// both get published, one wins, the other is unreachable while still reporting
// "running". Catching it here turns a baffling "deployed but dead" plugin into a
// clear install/update error.
func (l *Lifecycle) checkHostPortConflicts(name string, rec *PluginRecord) error {
	want := map[string]string{} // host-port key -> service that wants it
	for _, p := range rec.Ports {
		if p.HostExpose {
			want[hostPortKey(p.ContainerPort, p.Protocol)] = p.Service
		}
	}
	if len(want) == 0 {
		return nil
	}
	others, err := l.store.List()
	if err != nil {
		return fmt.Errorf("list plugins for host-port conflict check: %w", err)
	}
	for _, o := range others {
		if o.Name == name {
			continue
		}
		orec, err := l.store.Get(o.Name)
		if err != nil {
			return fmt.Errorf("load %q for host-port conflict check: %w", o.Name, err)
		}
		for key, svc := range otherHostPortKeys(orec) {
			if _, clash := want[key]; clash {
				return fmt.Errorf("host port %s is already published by plugin %q (%s); "+
					"two plugins cannot host-expose the same port — change one of them",
					key, o.Name, svc)
			}
		}
	}
	return nil
}

// otherHostPortKeys returns the host-published port keys (port/proto -> a service
// or "compose" descriptor) for an installed plugin, unified across the manifest
// (hostExpose) and compose (ports:) forms so the guard is cross-type.
func otherHostPortKeys(orec *PluginRecord) map[string]string {
	keys := map[string]string{}
	if orec.Plugin.ArtifactType == ArtifactCompose {
		ports, _ := compose.HostPorts([]byte(orec.Plugin.ManifestYAML))
		for _, h := range ports {
			keys[h.Key()] = "compose"
		}
		return keys
	}
	for _, p := range orec.Ports {
		if p.HostExpose {
			keys[hostPortKey(p.ContainerPort, p.Protocol)] = "service " + p.Service
		}
	}
	return keys
}

// checkComposeHostPortConflicts is the compose-plugin analogue of
// checkHostPortConflicts: it parses the candidate's fixed published ports and
// rejects a collision with any other installed plugin (manifest or compose)
// before `compose up`, turning a silent DNAT shadow into a clear error.
func (l *Lifecycle) checkComposeHostPortConflicts(name, composeYAML string) error {
	mine, err := compose.HostPorts([]byte(composeYAML))
	if err != nil {
		return fmt.Errorf("parse candidate ports: %w", err)
	}
	if len(mine) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, h := range mine {
		want[h.Key()] = true
	}
	others, err := l.store.List()
	if err != nil {
		return fmt.Errorf("list plugins for host-port conflict check: %w", err)
	}
	for _, o := range others {
		if o.Name == name {
			continue
		}
		orec, err := l.store.Get(o.Name)
		if err != nil {
			return fmt.Errorf("load %q for host-port conflict check: %w", o.Name, err)
		}
		for key, svc := range otherHostPortKeys(orec) {
			if want[key] {
				return fmt.Errorf("host port %s is already published by plugin %q (%s); "+
					"change one of them", key, o.Name, svc)
			}
		}
	}
	return nil
}

// createInstance creates one container from a pre-built payload, records
// its ID, and marks the unit stopped (ready to start).
func (l *Lifecycle) createInstance(ctx context.Context, name string, p *PluginRow, svc *Service, instance, count int, payload runtime.CreateContainerRequest) error {
	containerName := ContainerName(name, svc.Name, instance, count)

	// Idempotency by name (defense-in-depth beyond the Materialise lock). A managed
	// container with this deterministic name may already exist in the runtime with
	// no ID recorded in our DB — a create that landed before SetInstanceContainerID
	// persisted, or a concurrent apply on a runtime that (unlike real Docker) does
	// not reject a duplicate name. Adopt one that matches the desired payload rather
	// than creating a second; replace a stale one so the create below is clean.
	if existingID, ferr := l.findManagedContainerByName(ctx, containerName); ferr == nil && existingID != "" {
		if insp, ierr := l.rt.InspectContainer(ctx, existingID); ierr == nil {
			if containerMatchesDesired(insp, payload) {
				if err := l.store.SetInstanceContainerID(name, svc.Name, instance, existingID); err != nil {
					return fmt.Errorf("adopt existing container %q for %s/%d: %w", containerName, svc.Name, instance, err)
				}
				_ = l.setInstanceState(name, svc.Name, instance, mapDockerState(insp.State.Status), "")
				return nil
			}
			_ = l.rt.StopContainer(ctx, existingID, DefaultStopTimeoutSeconds)
			_ = l.removeContainerWithCleanup(ctx, existingID, true)
		}
	}

	_ = l.setInstanceState(name, svc.Name, instance, StateCreating, "")
	resp, err := l.rt.CreateContainer(ctx, containerName, payload)
	if err != nil && runtime.IsConflict(err) {
		// A create canceled mid-flight (e.g. a slow first image pull whose
		// context deadline expired) can leave a container under this name
		// that we never recorded an ID for. The stale-container branch in
		// Materialise keys off the recorded ContainerID, so it never runs
		// for such an orphan and every retry 409s — wedging the plugin
		// until the container is removed by hand. Force-remove the
		// name-conflicting container and retry the create once.
		if rmErr := l.rt.RemoveContainer(ctx, containerName, true); rmErr != nil {
			_ = l.setInstanceState(name, svc.Name, instance, StateFailed, err.Error())
			return fmt.Errorf("remove name-conflicting container %q for %s/%d: %w", containerName, svc.Name, instance, rmErr)
		}
		resp, err = l.rt.CreateContainer(ctx, containerName, payload)
	}
	if err != nil {
		_ = l.setInstanceState(name, svc.Name, instance, StateFailed, err.Error())
		return fmt.Errorf("create container for %s/%d: %w", svc.Name, instance, err)
	}
	if err := l.store.SetInstanceContainerID(name, svc.Name, instance, resp.ID); err != nil {
		return fmt.Errorf("record container id: %w", err)
	}
	_ = l.setInstanceState(name, svc.Name, instance, StateStopped, "")
	return nil
}

// findManagedContainerByName returns the ID of a tierd-managed container whose
// name matches (the runtime reports names with a leading "/"). Empty ID, nil
// error when none exists.
func (l *Lifecycle) findManagedContainerByName(ctx context.Context, name string) (string, error) {
	managed, err := l.rt.ListManagedContainers(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range managed {
		for _, n := range c.Names {
			if n == name || n == "/"+name {
				return c.ID, nil
			}
		}
	}
	return "", nil
}

// pluginMaterialiseLocks serializes Materialise per plugin so concurrent applies
// cannot each create a container for the same instance (see Materialise). A
// separate lock from composeScaleLocks: scale calls Materialise while holding the
// scale lock, so reusing it would self-deadlock.
var pluginMaterialiseLocks sync.Map // plugin name -> *sync.Mutex

func pluginMaterialiseLock(name string) *sync.Mutex {
	mu, _ := pluginMaterialiseLocks.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// AutostartAll materialises and starts every installed plugin that is not
// already running. It continues across per-plugin failures.
func (l *Lifecycle) AutostartAll(ctx context.Context) error {
	plugins, err := l.store.List()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	var errs []error
	for _, p := range plugins {
		if p.State == StateRunning {
			continue
		}
		if err := l.Materialise(ctx, p.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: materialise: %w", p.Name, err))
			continue
		}
		rec, err := l.store.Get(p.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: refresh: %w", p.Name, err))
			continue
		}
		if rec.Plugin.State == StateRunning {
			continue
		}
		if err := l.Start(ctx, p.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: start: %w", p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// RefreshContainers pulls the plugin's declared container refs and recreates
// runtime containers whose resolved refs changed. It intentionally does not
// replace the plugin manifest or bump the plugin version.
func (l *Lifecycle) RefreshContainers(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	if l.isCompose(rec) {
		// compose owns container discovery; a refresh is just a re-up.
		if err := l.Materialise(ctx, name); err != nil {
			return err
		}
		return l.Start(ctx, name)
	}
	wasRunning := rec.Plugin.State == StateRunning
	if err := l.Materialise(ctx, name); err != nil {
		return err
	}
	if wasRunning {
		if err := l.Start(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func containerMatchesDesired(existing runtime.ContainerInspect, desired runtime.CreateContainerRequest) bool {
	if desired.Image != "" && existing.Config.Image != "" && existing.Config.Image != desired.Image {
		return false
	}
	if desired.Image != "" && existing.Config.Image == "" && existing.Image != "" && existing.Image != desired.Image {
		return false
	}
	if len(desired.Cmd) > 0 && !reflect.DeepEqual(existing.Config.Cmd, desired.Cmd) {
		return false
	}
	if desired.WorkingDir != "" && existing.Config.WorkingDir != desired.WorkingDir {
		return false
	}
	if desired.User != "" && existing.Config.User != desired.User {
		return false
	}
	if !envContainsDesired(existing.Config.Env, desired.Env) {
		return false
	}
	if !labelsContainDesired(existing.Config.Labels, desired.Labels) {
		return false
	}
	if !exposedPortsMatchDesired(existing.Config.ExposedPorts, desired.ExposedPorts) {
		return false
	}
	return hostConfigMatchesDesired(existing.HostConfig, desired.HostConfig)
}

func hostConfigMatchesDesired(existing, desired runtime.HostConfig) bool {
	if desired.NetworkMode != "" && existing.NetworkMode != desired.NetworkMode {
		return false
	}
	if !reflect.DeepEqual(existing.Binds, desired.Binds) {
		return false
	}
	if !reflect.DeepEqual(existing.Devices, desired.Devices) {
		return false
	}
	if !reflect.DeepEqual(existing.CapAdd, desired.CapAdd) {
		return false
	}
	if desired.Memory != existing.Memory {
		return false
	}
	if desired.NanoCPUs != existing.NanoCPUs {
		return false
	}
	if desired.PidsLimit != existing.PidsLimit {
		return false
	}
	if desired.OomScoreAdj != existing.OomScoreAdj {
		return false
	}
	if desired.RestartPolicy.Name != "" && existing.RestartPolicy.Name != desired.RestartPolicy.Name {
		return false
	}
	if !portBindingsMatchDesired(existing.PortBindings, desired.PortBindings) {
		return false
	}
	return true
}

func exposedPortsMatchDesired(existing, desired map[string]struct{}) bool {
	if len(existing) == 0 && len(desired) == 0 {
		return true
	}
	return reflect.DeepEqual(existing, desired)
}

func portBindingsMatchDesired(existing, desired map[string][]runtime.PortBinding) bool {
	if len(existing) == 0 && len(desired) == 0 {
		return true
	}
	return reflect.DeepEqual(existing, desired)
}

func envContainsDesired(existing, desired []string) bool {
	have := envSliceMap(existing)
	want := envSliceMap(desired)
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func envSliceMap(values []string) map[string]string {
	out := map[string]string{}
	for _, item := range values {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

func labelsContainDesired(existing, desired map[string]string) bool {
	for k, v := range desired {
		if existing[k] != v {
			return false
		}
	}
	return true
}

// materialiseImage handles the artifact-type-specific pull (and, for an
// lxc-distro service with a setup script, the one-shot setup container +
// commit flow). Returns the resolved image ref for the create payloads.
type resolvedContainerRefs struct {
	PrimaryImageRef string
	Generation      string
}

func (l *Lifecycle) resolveContainerRefs(ctx context.Context, p *PluginRow, svc *Service, config []ConfigRow) (resolvedContainerRefs, error) {
	refs := svc.EffectiveContainerRefs()
	if len(refs) == 0 {
		return resolvedContainerRefs{}, nil
	}
	_ = l.setInstanceState(p.Name, svc.Name, 1, StatePulling, "")

	env := map[string]string{}
	for _, c := range config {
		env[c.Key] = c.Value
	}
	// Operator image pin for the primary service (survives updates -- re-applied here
	// on every materialise). Empty when none is set.
	pinned, err := l.store.PinnedImage(p.Name, svc.Name)
	if err != nil {
		return resolvedContainerRefs{}, err
	}
	resolvedByName := make(map[string]string, len(refs))
	out := resolvedContainerRefs{}
	for _, ref := range refs {
		image := expandArg(ref.Image, env)
		manifestDigest := ref.Digest
		// A pinned image replaces the primary ref's manifest image; it carries its own
		// tag/digest, so the manifest's digest pin no longer applies.
		if ref.Name == "primary" && pinned != "" {
			image = pinned
			manifestDigest = ""
		}
		pullRef := digestPinnedImageRef(image, manifestDigest)
		resolved, err := l.pullImageWithRetry(ctx, pullRef, nil)
		if err != nil {
			_ = l.setInstanceState(p.Name, svc.Name, 1, StateFailed, err.Error())
			return out, fmt.Errorf("pull container ref %s/%s (%s): %w", svc.Name, ref.Name, pullRef, err)
		}
		if manifestDigest != "" && !strings.Contains(resolved, manifestDigest) {
			_ = l.setInstanceState(p.Name, svc.Name, 1, StateFailed, "container ref digest mismatch")
			return out, fmt.Errorf("container ref %s/%s digest mismatch: pulled %s, manifest pinned %s", svc.Name, ref.Name, resolved, manifestDigest)
		}
		digest := digestFromImageRef(resolved)
		if digest == "" {
			digest = manifestDigest
		}
		if err := l.store.SetContainerRefResolved(p.Name, svc.Name, ref.Name, pullRef, digest, resolved); err != nil {
			return out, err
		}
		if ref.Name == "primary" {
			out.PrimaryImageRef = resolved
			_ = l.store.SetImageRef(p.Name, svc.Name, resolved)
		}
		resolvedByName[ref.Name] = resolved
	}
	out.Generation = containerRefsGeneration(resolvedByName)
	return out, nil
}

func (l *Lifecycle) materialiseImage(ctx context.Context, p *PluginRow, svc *Service, refs resolvedContainerRefs) (string, error) {
	switch svc.Artifact.Type {
	case ArtifactOCIImage:
		if refs.PrimaryImageRef == "" {
			return "", fmt.Errorf("primary container ref for service %q did not resolve", svc.Name)
		}
		return refs.PrimaryImageRef, nil

	case ArtifactLXCDistro:
		// LXC2Docker reserves bare distro references for faithful OCI pulls.
		// This artifact explicitly requests an LXC download-template distro,
		// so use the opt-in linuxcontainers host rather than relying on the old
		// bare-name shortcut.
		baseRef := "images.linuxcontainers.org/" + svc.Artifact.Distro + ":" + svc.Artifact.Release
		_ = l.setInstanceState(p.Name, svc.Name, 1, StatePulling, "")
		resolvedBaseRef, err := l.pullImageWithRetry(ctx, baseRef, nil)
		if err != nil {
			_ = l.setInstanceState(p.Name, svc.Name, 1, StateFailed, err.Error())
			return "", fmt.Errorf("pull distro template %s: %w", baseRef, err)
		}
		if len(svc.Artifact.Packages) == 0 && len(svc.Artifact.Setup) == 0 {
			_ = l.store.SetImageRef(p.Name, svc.Name, resolvedBaseRef)
			return resolvedBaseRef, nil
		}
		commitTag := SetupTemplateImageForBase(p.Name, svc.Name, p.Version, resolvedBaseRef, svc.Artifact.Packages, svc.Artifact.Setup)
		if err := l.runSetupScript(ctx, p, svc, resolvedBaseRef, commitTag); err != nil {
			_ = l.setInstanceState(p.Name, svc.Name, 1, StateFailed, err.Error())
			return "", err
		}
		_ = l.store.SetImageRef(p.Name, svc.Name, commitTag)
		return commitTag, nil

	default:
		return "", fmt.Errorf("unknown artifact type %q", svc.Artifact.Type)
	}
}

func (l *Lifecycle) pullImageWithRetry(ctx context.Context, ref string, onProgress func(runtime.PullEvent)) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= imagePullMaxAttempts; attempt++ {
		resolved, err := l.rt.PullImage(ctx, ref, onProgress)
		if err == nil {
			return resolved, nil
		}
		lastErr = err
		if attempt == imagePullMaxAttempts {
			break
		}
		timer := time.NewTimer(imagePullRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", fmt.Errorf("failed after %d attempts: %w", imagePullMaxAttempts, lastErr)
}

func (l *Lifecycle) runSetupScript(ctx context.Context, p *PluginRow, svc *Service, baseRef, commitTag string) error {
	cmd := buildSetupCmd(svc.Artifact.Distro, svc.Artifact.Packages, svc.Artifact.Setup)

	createReq := runtime.CreateContainerRequest{
		Image: baseRef,
		Cmd:   []string{"/bin/sh", "-c", cmd},
		Labels: map[string]string{
			runtime.PluginManagedLabel: "true",
			runtime.PluginNameLabel:    p.Name,
			runtime.PluginServiceLabel: svc.Name,
			"io.smoothnas.role":        "setup",
		},
		HostConfig: runtime.HostConfig{
			RestartPolicy: runtime.RestartPolicy{Name: "no"},
		},
	}
	setupName := "smoothnas-plugin-setup-" + p.Name + "-" + svc.Name
	resp, err := l.rt.CreateContainer(ctx, setupName, createReq)
	if err != nil {
		return fmt.Errorf("create setup container: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = l.removeContainerWithCleanup(cleanupCtx, resp.ID, true)
	}()

	if err := l.rt.StartContainer(ctx, resp.ID); err != nil {
		return fmt.Errorf("start setup container: %w", err)
	}
	exit, err := l.rt.WaitContainer(ctx, resp.ID)
	if err != nil {
		return fmt.Errorf("wait setup container: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("setup script exited %d", exit)
	}
	repo, tag := splitImageTag(commitTag)
	if _, err := l.rt.CommitContainer(ctx, resp.ID, repo, tag); err != nil {
		return fmt.Errorf("commit setup container: %w", err)
	}
	return nil
}

func buildSetupCmd(distro string, packages, setup []string) string {
	var b strings.Builder
	b.WriteString("set -ex\n")
	if len(packages) > 0 {
		switch distro {
		case "ubuntu", "debian":
			b.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
			b.WriteString("apt-get update\n")
			b.WriteString("apt-get install -y --no-install-recommends ")
			b.WriteString(strings.Join(packages, " "))
			b.WriteString("\n")
		case "alpine":
			b.WriteString("apk add --no-cache ")
			b.WriteString(strings.Join(packages, " "))
			b.WriteString("\n")
		default:
			b.WriteString("# unknown distro " + distro + ": skipping package install\n")
		}
	}
	for _, line := range setup {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func splitImageTag(ref string) (string, string) {
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func digestPinnedImageRef(image, digest string) string {
	if image == "" {
		return ""
	}
	base, embeddedDigest, hasEmbeddedDigest := strings.Cut(image, "@")
	if digest == "" && hasEmbeddedDigest {
		digest = embeddedDigest
	}
	if digest == "" {
		return image
	}
	repo, _ := splitImageTag(base)
	return repo + "@" + digest
}

func digestFromImageRef(ref string) string {
	_, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ""
	}
	if strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return ""
}

func containerRefsGeneration(refs map[string]string) string {
	if len(refs) == 0 {
		return ""
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(refs[name]))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func containerRefsGenerationForRows(rows []ContainerRefRow, service string) string {
	refs := map[string]string{}
	for _, row := range rows {
		if row.Service != service {
			continue
		}
		ref := row.ResolvedRef
		if ref == "" {
			ref = row.ImageRef
		}
		if ref != "" {
			refs[row.Name] = ref
		}
	}
	return containerRefsGeneration(refs)
}

// --- Start / Stop --------------------------------------------------------

// Start brings every service of a plugin to running, in dependency
// (ordinal) order. Backends start first; a dependent service is recreated
// with its discovery env resolved (sibling bridge IPs) before it starts,
// and a dependency declared service_healthy is waited on before its
// dependents come up. Once all services run, the nginx route (when wired)
// is applied against the primary service.
func (l *Lifecycle) Start(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	if l.isCompose(rec) {
		spec := l.composeSpec(rec)
		secrets, err := l.store.GetComposeSecrets(name)
		if err != nil {
			return err
		}
		if len(secrets) > 0 {
			spec.SecretEnv = secrets // injected into the `up` subprocess env for ${KEY}
		}
		if err := l.backend.Start(ctx, spec); err != nil {
			return err
		}
		l.syncComposeState(ctx, name, rec) // cache the ACTUAL rollup, not optimistic running
		return nil
	}
	manifest, err := ParseManifest([]byte(rec.Plugin.ManifestYAML))
	if err != nil {
		return fmt.Errorf("re-parse stored manifest: %w", err)
	}
	svcMap := manifestServiceMap(manifest)

	var resolved *Resolved
	if l.catalog != nil {
		if resolved, err = Resolve(l.catalog, manifest, nil); err != nil {
			return fmt.Errorf("resolve profiles: %w", err)
		}
	}

	count := rec.Plugin.InstanceCount
	order := orderedServices(rec)
	disc := currentDiscovery(rec, svcMap)

	for _, sr := range order {
		svc := svcMap[sr.Service]
		if svc == nil {
			return fmt.Errorf("stored manifest is missing service %q", sr.Service)
		}

		// Re-read so we see container IDs written by earlier iterations
		// (and any recreate below).
		fresh, err := l.store.Get(name)
		if err != nil {
			return err
		}
		instances := filterInstances(fresh, sr.Service)
		vols := filterVolumes(fresh, sr.Service)
		cfg := filterConfig(fresh, sr.Service)

		// Dependents carry {{service.X.host}} tokens that could only be
		// resolved once their dependencies started, so recreate them now
		// with the discovery map populated by earlier services.
		if len(svc.DependsOn) > 0 {
			imageRef := serviceImageRef(fresh, svc)
			refsGeneration := containerRefsGenerationForRows(fresh.ContainerRefs, sr.Service)
			for _, inst := range instances {
				if inst.ContainerID != "" {
					_ = l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds)
					_ = l.removeContainerWithCleanup(ctx, inst.ContainerID, true)
					_ = l.store.SetInstanceContainerID(name, sr.Service, inst.Instance, "")
				}
				payload, err := BuildCreatePayload(PayloadInputs{
					Plugin:                  &fresh.Plugin,
					Service:                 svc,
					Instance:                inst.Instance,
					ImageRef:                imageRef,
					ContainerRefsGeneration: refsGeneration,
					Volumes:                 vols,
					Config:                  cfg,
					Discovery:               disc,
					Profiles:                resolved,
				})
				if err != nil {
					return fmt.Errorf("rebuild payload for %s/%d: %w", sr.Service, inst.Instance, err)
				}
				if err := l.createInstance(ctx, name, &fresh.Plugin, svc, inst.Instance, count, payload); err != nil {
					return err
				}
			}
			// Reload the freshly-created container IDs.
			fresh, err = l.store.Get(name)
			if err != nil {
				return err
			}
			instances = filterInstances(fresh, sr.Service)
		}

		for _, inst := range instances {
			if inst.ContainerID == "" {
				return fmt.Errorf("service %s instance %d has no container — call Materialise first", sr.Service, inst.Instance)
			}
			_ = l.setInstanceState(name, sr.Service, inst.Instance, StateStarting, "")
			if err := l.rt.StartContainer(ctx, inst.ContainerID); err != nil {
				_ = l.setInstanceState(name, sr.Service, inst.Instance, StateFailed, err.Error())
				return fmt.Errorf("start %s/%d: %w", sr.Service, inst.Instance, err)
			}
			if ip, err := l.captureBridgeIP(ctx, inst.ContainerID); err != nil {
				_ = l.setInstanceState(name, sr.Service, inst.Instance, StateFailed, fmt.Sprintf("bridge IP: %v", err))
				return fmt.Errorf("capture bridge IP for %s/%d: %w", sr.Service, inst.Instance, err)
			} else if ip != "" {
				_ = l.store.SetInstanceBridgeIP(name, sr.Service, inst.Instance, ip)
			}
			_ = l.setInstanceState(name, sr.Service, inst.Instance, StateRunning, "")
		}

		// disc already carries this service's stable bridge name (set by
		// currentDiscovery); downstream dependents resolve it via the
		// /etc/hosts records tierd maintains, so there's no per-start IP
		// to republish here.

		// Health gate: if any later service depends on this one with
		// service_healthy, wait for readiness before proceeding.
		if dependedHealthy(manifest, sr.Service) {
			if err := l.waitServiceReady(ctx, instances); err != nil {
				return fmt.Errorf("service %s not ready: %w", sr.Service, err)
			}
		}
	}

	if l.proxy != nil {
		updated, err := l.store.Get(name)
		if err != nil {
			return err
		}
		route, err := l.buildPluginRoute(updated)
		if err != nil {
			return fmt.Errorf("build nginx route: %w", err)
		}
		if err := l.proxy.Apply(ctx, route); err != nil {
			return fmt.Errorf("apply nginx route: %w", err)
		}
	}
	return nil
}

// dependedHealthy reports whether any service declares a service_healthy
// dependency on target.
func dependedHealthy(m *Manifest, target string) bool {
	for i := range m.Services {
		if c, ok := m.Services[i].DependsOn[target]; ok && c.Condition == DependsServiceHealthy {
			return true
		}
	}
	return false
}

// waitServiceReady waits, best-effort, for every instance of a service to
// report Running. LXC2Docker does not surface healthcheck-command results
// yet, so this is a readiness gate, not a true health gate.
func (l *Lifecycle) waitServiceReady(ctx context.Context, instances []InstanceRow) error {
	for _, inst := range instances {
		if inst.ContainerID == "" {
			continue
		}
		ready := false
		for attempt := 0; attempt < serviceHealthMaxAttempts; attempt++ {
			insp, err := l.rt.InspectContainer(ctx, inst.ContainerID)
			if err == nil && insp.State.Running {
				ready = true
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(serviceHealthRetryDelay):
			}
		}
		if !ready {
			return fmt.Errorf("instance %d did not become ready", inst.Instance)
		}
	}
	return nil
}

func (l *Lifecycle) captureBridgeIP(ctx context.Context, containerID string) (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		ip, err := l.rt.InspectContainerBridgeIP(ctx, containerID)
		if err == nil {
			return ip, nil
		}
		if !errors.Is(err, runtime.ErrBridgeIPNotReady) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("bridge IP not assigned after 10 retries")
}

// buildPluginRoute renders the plugin's primary (user-facing) service into
// a PluginRoute. The primary service's exposed ports become the routes;
// its instance-1 bridge IP is the upstream. Multi-instance load balancing
// is deferred per the proposal.
func (l *Lifecycle) buildPluginRoute(rec *PluginRecord) (PluginRoute, error) {
	if len(rec.Instances) == 0 {
		return PluginRoute{}, fmt.Errorf("no instances")
	}
	primary := primaryService(rec)
	upstreamIP := ""
	for _, in := range rec.Instances {
		if in.Service == primary && in.Instance == 1 {
			upstreamIP = in.BridgeIP
		}
	}
	if upstreamIP == "" {
		return PluginRoute{}, fmt.Errorf("primary service %q instance 1 has no bridge IP", primary)
	}

	var token string
	if l.store != nil {
		t, err := l.store.GetBearerToken(rec.Plugin.Name)
		if err != nil {
			return PluginRoute{}, fmt.Errorf("get bearer token: %w", err)
		}
		token = t
	}

	route := PluginRoute{
		PluginName: rec.Plugin.Name,
		Version:    rec.Plugin.Version,
	}
	first := true
	for _, p := range rec.Ports {
		if p.Service != primary || !p.Expose {
			continue
		}
		var locationPath string
		if first {
			locationPath = "/plugins/" + rec.Plugin.Name + "/"
			first = false
		} else {
			locationPath = "/plugins/" + rec.Plugin.Name + "/" + p.Name + "/"
		}
		route.Routes = append(route.Routes, ProxyRoute{
			LocationPath: locationPath,
			UpstreamURL:  fmt.Sprintf("http://%s:%d/", upstreamIP, p.ContainerPort),
			AuthBearer:   token,
		})
	}
	return route, nil
}

// Stop signals every running instance, in reverse dependency order
// (dependents before their backends), waiting up to
// DefaultStopTimeoutSeconds before SIGKILL.
func (l *Lifecycle) Stop(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	if l.isCompose(rec) {
		if err := l.backend.Stop(ctx, l.composeSpec(rec)); err != nil {
			return err
		}
		l.syncComposeState(ctx, name, rec) // real rollup (a partial stop isn't fully stopped)
		return nil
	}
	order := orderedServices(rec)
	var firstErr error
	for i := len(order) - 1; i >= 0; i-- {
		sr := order[i]
		for _, inst := range filterInstances(rec, sr.Service) {
			if inst.ContainerID == "" {
				continue
			}
			if err := l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds); err != nil {
				_ = l.setInstanceState(name, sr.Service, inst.Instance, StateFailed, err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			_ = l.setInstanceState(name, sr.Service, inst.Instance, StateStopped, "")
		}
	}
	return firstErr
}

// Restart stops every service then starts the set again — going through
// Start so discovery env and ordering are re-established.
func (l *Lifecycle) Restart(ctx context.Context, name string) error {
	if err := l.Stop(ctx, name); err != nil {
		return err
	}
	return l.Start(ctx, name)
}

// Demolish stops, removes, and drops the cached image for every
// (service,instance) container, and (when a Proxy is wired) removes the
// per-plugin nginx route. Used by Uninstall. Idempotent.
func (l *Lifecycle) Demolish(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			return nil
		}
		return err
	}
	if l.isCompose(rec) {
		if err := l.backend.Teardown(ctx, l.composeSpec(rec), false); err != nil {
			return err
		}
		// `compose down` removes containers, networks and (with -v) anonymous
		// volumes, but NEVER the images it pulled — so a compose plugin's image
		// templates leak on the tier after uninstall. Reclaim them here, mirroring
		// the manifest path below. Guarded so an image another running plugin still
		// references is left intact.
		l.pruneUnusedImages(ctx, pluginImageRefs(rec))
		return nil
	}

	if l.proxy != nil {
		if err := l.proxy.Remove(ctx, name); err != nil {
			return fmt.Errorf("remove nginx route: %w", err)
		}
	}

	order := orderedServices(rec)
	for i := len(order) - 1; i >= 0; i-- {
		sr := order[i]
		for _, inst := range filterInstances(rec, sr.Service) {
			if inst.ContainerID == "" {
				// No recorded ID, but a create canceled mid-flight may have
				// left an orphan under the deterministic name. Best-effort
				// remove it by name so uninstall/reinstall isn't wedged by a
				// later name conflict. RemoveContainer is idempotent on 404.
				orphan := ContainerName(rec.Plugin.Name, sr.Service, inst.Instance, rec.Plugin.InstanceCount)
				_ = l.rt.RemoveContainer(ctx, orphan, true)
				continue
			}
			_ = l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds)
			if err := l.removeContainerWithCleanup(ctx, inst.ContainerID, true); err != nil {
				return fmt.Errorf("remove %s/%d container: %w", sr.Service, inst.Instance, err)
			}
			_ = l.store.SetInstanceContainerID(name, sr.Service, inst.Instance, "")
		}
	}

	// Drop per-service images: OCI refs and committed lxc-distro templates.
	for _, sr := range rec.Services {
		switch sr.ArtifactType {
		case ArtifactOCIImage:
			seen := map[string]bool{}
			for _, ref := range rec.ContainerRefs {
				if ref.Service != sr.Service {
					continue
				}
				removeRef := ref.ResolvedRef
				if removeRef == "" {
					removeRef = ref.ImageRef
				}
				if removeRef == "" || seen[removeRef] {
					continue
				}
				seen[removeRef] = true
				_ = l.rt.RemoveImage(ctx, removeRef)
			}
			if sr.ImageRef != "" && !seen[sr.ImageRef] {
				_ = l.rt.RemoveImage(ctx, sr.ImageRef)
			}
		case ArtifactLXCDistro:
			if sr.ImageRef != "" && strings.HasPrefix(sr.ImageRef, "smoothnas-plugin-") {
				_ = l.rt.RemoveImage(ctx, sr.ImageRef)
			} else {
				_ = l.rt.RemoveImage(ctx, SetupTemplateImage(rec.Plugin.Name, sr.Service, rec.Plugin.Version))
			}
		}
	}
	return nil
}

// Status returns the aggregate plugin state plus per-(service,instance)
// state for the named plugin.
func (l *Lifecycle) Status(ctx context.Context, name string) (*PluginRecord, error) {
	// Reads the cached state, which is kept in sync from compose ps at each op
	// (Start/Stop) for compose plugins. A per-read `compose ps` would spawn an
	// unbounded subprocess per UI poll; a periodic reconcile sweep for out-of-band
	// drift is a follow-up. (Manifest plugins are unchanged.)
	return l.store.Get(name)
}

// ApplyRouteFor re-renders and applies the nginx route for one plugin.
// No-op when no Proxy is wired.
func (l *Lifecycle) ApplyRouteFor(ctx context.Context, name string) error {
	if l.proxy == nil {
		return nil
	}
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	if l.isCompose(rec) {
		return nil // compose plugins don't register an nginx UI route
	}
	route, err := l.buildPluginRoute(rec)
	if err != nil {
		return fmt.Errorf("build route: %w", err)
	}
	return l.proxy.Apply(ctx, route)
}

// StreamContainerLogs opens a follow-mode logs stream from the runtime
// daemon for the given container.
func (l *Lifecycle) StreamContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	return l.rt.StreamLogs(ctx, containerID, runtime.LogsOptions{
		Follow: true, Stdout: true, Stderr: true, Tail: "200",
	})
}

// setInstanceState updates one (service,instance) unit's state.
func (l *Lifecycle) setInstanceState(name, service string, instance int, state, lastError string) error {
	return l.store.SetInstanceState(name, service, instance, state, lastError)
}
