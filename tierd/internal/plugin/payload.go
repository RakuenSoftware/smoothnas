package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// ContainerName returns the LXC2Docker container name for a given
// (plugin, instance) pair. Single-instance plugins use the bare name
// (matches operator expectations when they list with `lxc-ls`);
// multi-instance plugins suffix with -<n>.
func ContainerName(pluginName string, instance, instanceCount int) string {
	if instanceCount <= 1 {
		return pluginName
	}
	return fmt.Sprintf("%s-%d", pluginName, instance)
}

// SetupTemplateImage is the cached template image name for the
// committed result of an lxc-distro plugin's setup script. Stable
// across reinstalls of the same (manifest version, packages, setup)
// triple so the setup container only runs when one of those changes.
func SetupTemplateImage(pluginName, manifestVersion string) string {
	return fmt.Sprintf("smoothnas-plugin-%s:%s", pluginName, manifestVersion)
}

// SetupHash is a content hash over the lxc-distro setup inputs
// (packages + setup script lines). Stored alongside the plugin row
// (or kept in-memory in tests) so the lifecycle can decide "is the
// committed template still valid for this manifest?".
func SetupHash(packages, setup []string) string {
	h := sha256.New()
	// Sort packages so cosmetic ordering changes don't invalidate the
	// cache. Setup is order-sensitive (it's a script), so we leave it
	// alone.
	sortedPkgs := append([]string(nil), packages...)
	sort.Strings(sortedPkgs)
	for _, p := range sortedPkgs {
		h.Write([]byte(p))
		h.Write([]byte{'\n'})
	}
	h.Write([]byte("---\n"))
	for _, s := range setup {
		h.Write([]byte(s))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PayloadInputs is the data BuildCreatePayload needs. Bundling these
// into one struct keeps the call site readable; everything is read
// from the database except the ImageRef, which the lifecycle layer
// passes in after it resolves the pull.
type PayloadInputs struct {
	Plugin    *PluginRow
	Manifest  *Manifest
	Instance  int
	ImageRef  string         // resolved by Materialise; e.g. "ghcr.io/...@sha256:..."
	Volumes   []VolumeRow    // from the DB; expanded with per-instance host paths
	Config    []ConfigRow    // already merged with manifest defaults
}

// BuildCreatePayload renders the runtime.CreateContainerRequest for
// a single instance. Pure function — no DB access, no I/O — so unit
// tests are straightforward.
//
// Returns an error if a required volume's host path for this instance
// is missing or empty (which would mean phase 03 hasn't resolved a
// tier-bound volume yet, and the caller is racing the resolver).
func BuildCreatePayload(in PayloadInputs) (runtime.CreateContainerRequest, error) {
	if in.Plugin == nil || in.Manifest == nil {
		return runtime.CreateContainerRequest{}, fmt.Errorf("BuildCreatePayload: nil plugin or manifest")
	}
	if in.ImageRef == "" {
		return runtime.CreateContainerRequest{}, fmt.Errorf("BuildCreatePayload: empty image ref")
	}
	if in.Instance < 1 {
		return runtime.CreateContainerRequest{}, fmt.Errorf("BuildCreatePayload: instance=%d (must be ≥ 1)", in.Instance)
	}

	binds := make([]string, 0, len(in.Volumes))
	for _, vol := range in.Volumes {
		host := volumeHostPath(vol, in.Instance)
		if host == "" {
			return runtime.CreateContainerRequest{}, fmt.Errorf(
				"BuildCreatePayload: volume %q has no host_path for instance %d (tier-bound volume not yet resolved by phase 03?)",
				vol.Name, in.Instance,
			)
		}
		binds = append(binds, host+":"+vol.BindPath)
	}

	env := make([]string, 0, len(in.Config))
	// Stable env order so the rendered payload is deterministic in
	// tests and so a config-ordering reshuffle doesn't trigger a
	// container recreate diff.
	sort.Slice(in.Config, func(i, j int) bool { return in.Config[i].Key < in.Config[j].Key })
	for _, c := range in.Config {
		env = append(env, c.Key+"="+c.Value)
	}

	labels := map[string]string{
		runtime.PluginManagedLabel:  "true",
		runtime.PluginNameLabel:     in.Plugin.Name,
		runtime.PluginVersionLabel:  in.Plugin.Version,
		runtime.PluginInstanceLabel: strconv.Itoa(in.Instance),
	}

	return runtime.CreateContainerRequest{
		Image:      in.ImageRef,
		Cmd:        append([]string(nil), in.Manifest.Container.Command...),
		Env:        env,
		WorkingDir: in.Manifest.Container.WorkingDir,
		User:       in.Manifest.Container.User,
		Labels:     labels,
		HostConfig: runtime.HostConfig{
			Binds:       binds,
			NetworkMode: runtime.PluginBridgeName,
			RestartPolicy: runtime.RestartPolicy{
				Name: dockerRestartPolicyName(in.Manifest.EffectiveRestartPolicy()),
			},
			// Devices left empty: phase 05 (profiles) populates them.
		},
	}, nil
}

// volumeHostPath looks up the host path for a (volume, instance)
// pair. Shared volumes (per_instance=0) always live at instance=1
// regardless of which container is being created; per-instance
// volumes have one entry per replica.
func volumeHostPath(v VolumeRow, instance int) string {
	if !v.PerInstance {
		return v.Paths[1]
	}
	return v.Paths[instance]
}

// dockerRestartPolicyName maps the manifest's restart policy name to
// the wire-level value the daemon expects. They happen to be 1:1
// today but isolating the mapping keeps the manifest schema free to
// evolve.
func dockerRestartPolicyName(manifestPolicy string) string {
	switch manifestPolicy {
	case RestartUnlessStopped:
		return "unless-stopped"
	case RestartOnFailure:
		return "on-failure"
	case RestartNo:
		return "no"
	default:
		return "unless-stopped"
	}
}
