package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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

// Lifecycle owns the install/start/stop/uninstall flow that touches
// the runtime daemon. It does not own the database schema (that's
// Store) or the manifest format (that's manifest.go); it sits on
// top of both and adds the runtime calls.
type Lifecycle struct {
	store *Store
	rt    RuntimeClient
	// proxy is optional: when nil (legacy / test setups), Lifecycle
	// skips nginx route generation. Production wires *Proxy via
	// SetProxy at startup.
	proxy ProxyManager
	// catalog is optional: when nil (phase 1-4 tests), Materialise
	// skips profile resolution and the create payload contains no
	// profile-contributed fragments. Production wires a *Catalog
	// via SetCatalog at startup.
	catalog *Catalog
}

// NewLifecycle constructs a Lifecycle around an existing Store and a
// runtime client. Pass *runtime.Client in production; tests pass a
// fake.
func NewLifecycle(s *Store, rt RuntimeClient) *Lifecycle {
	return &Lifecycle{store: s, rt: rt}
}

// SetProxy attaches the nginx route manager. With a Proxy attached,
// Start writes the per-plugin route after capturing the bridge IP
// and Demolish removes it on uninstall. Pass nil to disable nginx
// integration (tests, environments without nginx).
func (l *Lifecycle) SetProxy(p ProxyManager) {
	l.proxy = p
}

// SetCatalog attaches the profile catalog. With a Catalog attached,
// Materialise resolves profiles, runs preflight, persists the
// applied profile names, and threads the merged fragments into the
// container create payload. Pass nil to disable profile resolution
// (phase 1-4 behaviour).
func (l *Lifecycle) SetCatalog(c *Catalog) {
	l.catalog = c
}

// Materialise pulls the image (and runs the lxc-distro setup flow if
// needed), creates the per-instance containers, and stamps the
// resulting container IDs into plugin_instances. After Materialise,
// the plugin can be Started.
//
// Idempotent: if a container already exists for an instance,
// Materialise re-uses it. If the image is already cached, the pull
// is fast.
func (l *Lifecycle) Materialise(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}

	manifest, err := ParseManifest([]byte(rec.Plugin.ManifestYAML))
	if err != nil {
		return fmt.Errorf("re-parse stored manifest: %w", err)
	}

	// Phase 04: ensure the smoothnas-plugins bridge exists before
	// container creation. Idempotent; safe to call every Materialise.
	if _, err := l.rt.EnsurePluginBridge(ctx); err != nil {
		return fmt.Errorf("ensure plugin bridge: %w", err)
	}

	// Phase 05: resolve profiles. nil catalog = no profile fragments
	// (preserves phase 1-4 test behaviour). When set, this validates
	// every referenced profile exists, runs preflight gates, and
	// returns the merged fragments to thread into the create payload.
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

	imageRef, err := l.materialiseImage(ctx, &rec.Plugin, manifest)
	if err != nil {
		return err
	}

	count := rec.Plugin.InstanceCount
	for _, inst := range rec.Instances {
		if inst.ContainerID != "" {
			// Already materialised. Verify the container still exists;
			// if not, recreate it.
			if _, err := l.rt.InspectContainer(ctx, inst.ContainerID); err == nil {
				continue
			} else if !runtime.IsNotFound(err) {
				return fmt.Errorf("inspect existing container for instance %d: %w", inst.Instance, err)
			}
			// Fall through to recreate.
		}

		payload, err := BuildCreatePayload(PayloadInputs{
			Plugin:   &rec.Plugin,
			Manifest: manifest,
			Instance: inst.Instance,
			ImageRef: imageRef,
			Volumes:  rec.Volumes,
			Config:   rec.Config,
			Profiles: resolved,
		})
		if err != nil {
			return fmt.Errorf("build payload for instance %d: %w", inst.Instance, err)
		}

		containerName := ContainerName(name, inst.Instance, count)
		_ = l.setInstanceState(name, inst.Instance, StateCreating, "")
		resp, err := l.rt.CreateContainer(ctx, containerName, payload)
		if err != nil {
			_ = l.setInstanceState(name, inst.Instance, StateFailed, err.Error())
			return fmt.Errorf("create container for instance %d: %w", inst.Instance, err)
		}
		if err := l.store.SetInstanceContainerID(name, inst.Instance, resp.ID); err != nil {
			return fmt.Errorf("record container id: %w", err)
		}
		_ = l.setInstanceState(name, inst.Instance, StateStopped, "")
	}

	return nil
}

// materialiseImage handles the artifact-type-specific pull (and, for
// lxc-distro plugins with a setup script, the one-shot setup
// container + commit flow). Returns the resolved image ref to use
// in the per-instance create payloads.
func (l *Lifecycle) materialiseImage(ctx context.Context, p *PluginRow, m *Manifest) (string, error) {
	switch m.Artifact.Type {
	case ArtifactOCIImage:
		// Update transient state on the first instance — the UI uses
		// any non-installed state on plugin_instances to render
		// "pulling…" progress.
		_ = l.setInstanceState(p.Name, 1, StatePulling, "")
		ref := m.Artifact.Image
		if m.Artifact.Digest != "" {
			ref = m.Artifact.Image + "@" + m.Artifact.Digest
		}
		resolved, err := l.rt.PullImage(ctx, ref, nil)
		if err != nil {
			_ = l.setInstanceState(p.Name, 1, StateFailed, err.Error())
			return "", fmt.Errorf("pull image %s: %w", ref, err)
		}
		// Verify the resolved digest matches the manifest's pin (if any).
		if m.Artifact.Digest != "" && !strings.Contains(resolved, m.Artifact.Digest) {
			_ = l.setInstanceState(p.Name, 1, StateFailed, "image digest mismatch")
			return "", fmt.Errorf("image digest mismatch: pulled %s, manifest pinned %s", resolved, m.Artifact.Digest)
		}
		// Persist the resolved ref so future operations don't re-pull.
		_ = l.store.SetImageRef(p.Name, resolved)
		return resolved, nil

	case ArtifactLXCDistro:
		baseRef := m.Artifact.Distro + ":" + m.Artifact.Release
		_ = l.setInstanceState(p.Name, 1, StatePulling, "")
		if _, err := l.rt.PullImage(ctx, baseRef, nil); err != nil {
			_ = l.setInstanceState(p.Name, 1, StateFailed, err.Error())
			return "", fmt.Errorf("pull distro template %s: %w", baseRef, err)
		}

		// No setup script → use the base directly.
		if len(m.Artifact.Packages) == 0 && len(m.Artifact.Setup) == 0 {
			return baseRef, nil
		}

		// Setup script flow: build a one-shot Cmd, run it, commit.
		commitTag := SetupTemplateImage(p.Name, p.Version)
		if err := l.runSetupScript(ctx, p, m, baseRef, commitTag); err != nil {
			_ = l.setInstanceState(p.Name, 1, StateFailed, err.Error())
			return "", err
		}
		return commitTag, nil

	default:
		return "", fmt.Errorf("unknown artifact type %q", m.Artifact.Type)
	}
}

// runSetupScript runs the apt-install + setup commands inside a
// one-shot container, waits for clean exit, commits the rootfs as
// commitTag, and removes the one-shot container. The setup
// container's logs are NOT captured here — phase 02 keeps this code
// simple and assumes operator inspection via `docker logs` if the
// setup fails. Phase 06 (UI) can layer log capture on top.
func (l *Lifecycle) runSetupScript(ctx context.Context, p *PluginRow, m *Manifest, baseRef, commitTag string) error {
	cmd := buildSetupCmd(m.Artifact.Distro, m.Artifact.Packages, m.Artifact.Setup)

	createReq := runtime.CreateContainerRequest{
		Image: baseRef,
		Cmd:   []string{"/bin/sh", "-c", cmd},
		Labels: map[string]string{
			runtime.PluginManagedLabel: "true",
			runtime.PluginNameLabel:    p.Name,
			"io.smoothnas.role":        "setup",
		},
		HostConfig: runtime.HostConfig{
			RestartPolicy: runtime.RestartPolicy{Name: "no"},
		},
	}
	setupName := "smoothnas-plugin-setup-" + p.Name
	resp, err := l.rt.CreateContainer(ctx, setupName, createReq)
	if err != nil {
		return fmt.Errorf("create setup container: %w", err)
	}
	// Best-effort cleanup whatever happens.
	defer func() {
		// Use a fresh context so cleanup runs even if the parent
		// context was cancelled mid-flow.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = l.rt.RemoveContainer(cleanupCtx, resp.ID, true)
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

	// Commit. The repo argument is the image name without the tag;
	// our SetupTemplateImage helper returns "<repo>:<tag>" so split
	// here.
	repo, tag := splitImageTag(commitTag)
	if _, err := l.rt.CommitContainer(ctx, resp.ID, repo, tag); err != nil {
		return fmt.Errorf("commit setup container: %w", err)
	}
	return nil
}

// buildSetupCmd composes the sh -c argument that installs packages and
// then runs each setup line. Distro selects the package manager; we
// support apt (debian/ubuntu) and apk (alpine) in v1. Other distros
// pass through as-is and rely on the setup script to handle install.
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
			// Best effort; let the setup script handle it.
			b.WriteString("# unknown distro " + distro + ": skipping package install\n")
		}
	}
	for _, line := range setup {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// splitImageTag splits "repo:tag" into ("repo", "tag").
func splitImageTag(ref string) (string, string) {
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// Start brings every instance of a plugin to the running state,
// captures each instance's bridge IP, and (when a Proxy is wired)
// writes the per-plugin nginx route. Materialise must have been
// called first; Start returns an error if any instance has no
// container_id recorded.
func (l *Lifecycle) Start(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	for _, inst := range rec.Instances {
		if inst.ContainerID == "" {
			return fmt.Errorf("instance %d has no container — call Materialise first", inst.Instance)
		}
		_ = l.setInstanceState(name, inst.Instance, StateStarting, "")
		if err := l.rt.StartContainer(ctx, inst.ContainerID); err != nil {
			_ = l.setInstanceState(name, inst.Instance, StateFailed, err.Error())
			return fmt.Errorf("start instance %d: %w", inst.Instance, err)
		}
		// Capture the bridge IP. The container is on the bridge
		// from create time, but the IPAM-assigned address only
		// becomes visible once the container's network namespace
		// is up — which is post-start. Retry briefly to handle the
		// brief window where the daemon is still wiring it.
		if ip, err := l.captureBridgeIP(ctx, inst.ContainerID); err != nil {
			_ = l.setInstanceState(name, inst.Instance, StateFailed, fmt.Sprintf("bridge IP: %v", err))
			return fmt.Errorf("capture bridge IP for instance %d: %w", inst.Instance, err)
		} else if ip != "" {
			_ = l.store.SetInstanceBridgeIP(name, inst.Instance, ip)
		}
		_ = l.setInstanceState(name, inst.Instance, StateRunning, "")
	}

	// Re-fetch so the route picks up the freshly-written bridge IPs.
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

// captureBridgeIP polls InspectContainerBridgeIP up to 10 times
// (~1 s) waiting for IPAM to populate the address. This window is
// usually <100ms but can stretch on a busy daemon.
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

// buildPluginRoute renders the plugin record into a PluginRoute the
// Proxy can apply. Multiple exposed ports per plugin produce
// multiple ProxyRoute entries — first port (declaration order) gets
// /plugins/<name>/, additional ports get /plugins/<name>/<port-name>/.
//
// For multi-instance plugins (count > 1), v1 routes to the first
// instance's bridge IP only — load balancing is a future enhancement
// and the proposal explicitly defers it. Operators wanting LB across
// runners should run plugin-side load balancing (e.g. socat).
func (l *Lifecycle) buildPluginRoute(rec *PluginRecord) (PluginRoute, error) {
	if len(rec.Instances) == 0 {
		return PluginRoute{}, fmt.Errorf("no instances")
	}
	upstreamIP := rec.Instances[0].BridgeIP
	if upstreamIP == "" {
		return PluginRoute{}, fmt.Errorf("instance 1 has no bridge IP")
	}

	route := PluginRoute{
		PluginName: rec.Plugin.Name,
		Version:    rec.Plugin.Version,
	}
	first := true
	for _, p := range rec.Ports {
		if !p.Expose {
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
		})
	}
	return route, nil
}

// Stop signals every running instance, waiting up to
// DefaultStopTimeoutSeconds before SIGKILL. Stopped instances stay
// stopped; the next Start will resume them.
func (l *Lifecycle) Stop(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	var firstErr error
	for _, inst := range rec.Instances {
		if inst.ContainerID == "" {
			continue
		}
		if err := l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds); err != nil {
			_ = l.setInstanceState(name, inst.Instance, StateFailed, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_ = l.setInstanceState(name, inst.Instance, StateStopped, "")
	}
	return firstErr
}

// Restart stops then starts every instance.
func (l *Lifecycle) Restart(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		return err
	}
	for _, inst := range rec.Instances {
		if inst.ContainerID == "" {
			continue
		}
		if err := l.rt.RestartContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds); err != nil {
			_ = l.setInstanceState(name, inst.Instance, StateFailed, err.Error())
			return fmt.Errorf("restart instance %d: %w", inst.Instance, err)
		}
		_ = l.setInstanceState(name, inst.Instance, StateRunning, "")
	}
	return nil
}

// Demolish stops, removes, and drops the cached image for every
// instance, and (when a Proxy is wired) removes the per-plugin
// nginx route. Used by Uninstall. Idempotent — gone-already
// containers, images, and nginx confs do not error.
func (l *Lifecycle) Demolish(ctx context.Context, name string) error {
	rec, err := l.store.Get(name)
	if err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			return nil
		}
		return err
	}

	// Remove the nginx route first, before the containers it points
	// at go away. nginx will return 502 to any in-flight request
	// either way; removing the route first means a fresh request
	// gets a clean 404 from SmoothNAS rather than 502.
	if l.proxy != nil {
		if err := l.proxy.Remove(ctx, name); err != nil {
			return fmt.Errorf("remove nginx route: %w", err)
		}
	}

	for _, inst := range rec.Instances {
		if inst.ContainerID == "" {
			continue
		}
		_ = l.rt.StopContainer(ctx, inst.ContainerID, DefaultStopTimeoutSeconds)
		if err := l.rt.RemoveContainer(ctx, inst.ContainerID, true); err != nil {
			return fmt.Errorf("remove instance %d container: %w", inst.Instance, err)
		}
		_ = l.store.SetInstanceContainerID(name, inst.Instance, "")
	}

	// Drop image. For lxc-distro plugins, drop both the committed
	// template AND don't bother with the upstream distro template
	// — that's shared across plugins, eviction is not our call.
	manifest, err := ParseManifest([]byte(rec.Plugin.ManifestYAML))
	if err == nil {
		switch manifest.Artifact.Type {
		case ArtifactOCIImage:
			if rec.Plugin.ImageRef != "" {
				_ = l.rt.RemoveImage(ctx, rec.Plugin.ImageRef)
			}
		case ArtifactLXCDistro:
			tmpl := SetupTemplateImage(rec.Plugin.Name, rec.Plugin.Version)
			_ = l.rt.RemoveImage(ctx, tmpl)
		}
	}
	return nil
}

// Status returns the aggregate plugin state plus per-instance state
// for the named plugin. Reads the DB; the event subscriber keeps the
// DB in sync with the runtime, so this is fast and consistent.
func (l *Lifecycle) Status(ctx context.Context, name string) (*PluginRecord, error) {
	return l.store.Get(name)
}

// StreamContainerLogs opens a follow-mode logs stream from the
// runtime daemon for the given container. The api layer wraps the
// returned reader as Server-Sent Events for the UI's logs view.
// Caller is responsible for closing the returned reader.
func (l *Lifecycle) StreamContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	return l.rt.StreamLogs(ctx, containerID, runtime.LogsOptions{
		Follow: true, Stdout: true, Stderr: true, Tail: "200",
	})
}

// setInstanceState is a thin wrapper that swallows DB errors after
// logging — lifecycle code already returns on the underlying
// runtime error; failing to update the DB on top of that is worth
// noting but not worth replacing the more useful error.
func (l *Lifecycle) setInstanceState(name string, instance int, state, lastError string) error {
	return l.store.SetInstanceState(name, instance, state, lastError)
}
