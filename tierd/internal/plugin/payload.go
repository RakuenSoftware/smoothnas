package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	Plugin   *PluginRow
	Manifest *Manifest
	Instance int
	ImageRef string      // resolved by Materialise; e.g. "ghcr.io/...@sha256:..."
	Volumes  []VolumeRow // from the DB; expanded with per-instance host paths
	Config   []ConfigRow // already merged with manifest defaults
	// Profiles is the merged profile fragments (devices, env, capAdd,
	// pidsLimit, oomScoreAdj, lxc raw config). When nil the renderer
	// behaves as phase 1–4 (no profile contribution); production
	// callers always pass a non-nil *Resolved from plugin.Resolve.
	Profiles *Resolved
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

	// Build the env list. Profile-contributed env entries land first,
	// the manifest's plugin_config entries after — so plugin_config
	// wins on key collisions (operators tuning runtime values
	// override profile defaults).
	envMap := map[string]string{}
	if in.Profiles != nil {
		for k, v := range in.Profiles.Env {
			envMap[k] = v
		}
	}
	for _, c := range in.Config {
		envMap[c.Key] = c.Value
	}
	envKeys := make([]string, 0, len(envMap))
	for k := range envMap {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	env := make([]string, 0, len(envKeys))
	for _, k := range envKeys {
		env = append(env, k+"="+envMap[k])
	}

	labels := map[string]string{
		runtime.PluginManagedLabel:           "true",
		runtime.PluginNameLabel:              in.Plugin.Name,
		runtime.PluginVersionLabel:           in.Plugin.Version,
		runtime.PluginInstanceLabel:          strconv.Itoa(in.Instance),
		runtime.LXC2DockerBindMountInitLabel: "image",
	}

	host := runtime.HostConfig{
		Binds:       binds,
		NetworkMode: runtime.PluginBridgeName,
		RestartPolicy: runtime.RestartPolicy{
			Name: dockerRestartPolicyName(in.Manifest.EffectiveRestartPolicy()),
		},
	}
	exposedPorts := map[string]struct{}{}
	portBindings := map[string][]runtime.PortBinding{}
	for _, p := range in.Manifest.Ports {
		if !p.HostExpose {
			continue
		}
		key := fmt.Sprintf("%d/%s", p.Port, strings.ToLower(p.Protocol))
		exposedPorts[key] = struct{}{}
		portBindings[key] = []runtime.PortBinding{{HostPort: strconv.Itoa(p.Port)}}
	}
	if len(portBindings) > 0 {
		host.PortBindings = portBindings
	}
	if in.Profiles != nil {
		// Devices: profile-contributed mappings.
		for _, d := range in.Profiles.Devices {
			host.Devices = append(host.Devices, runtime.DeviceMapping{
				PathOnHost:        d.Path,
				PathInContainer:   d.Path,
				CgroupPermissions: cgroupPerms(d.CgroupPermissions),
			})
		}
		host.CapAdd = append([]string(nil), in.Profiles.CapAdd...)
		if in.Profiles.PidsLimit != 0 {
			host.PidsLimit = in.Profiles.PidsLimit
		}
		if in.Profiles.Memory != 0 {
			host.Memory = in.Profiles.Memory
		}
		if in.Profiles.OomScoreAdj != nil {
			host.OomScoreAdj = *in.Profiles.OomScoreAdj
		}

		// LXC raw-config directives ride as labels for LXC2Docker
		// to apply at container start. Indexed so the ordering is
		// preserved (the directives are order-sensitive).
		for i, raw := range in.Profiles.LXCRaw {
			labels[fmt.Sprintf("io.smoothnas.lxc.raw.%d", i)] = raw
		}
	}
	if in.Manifest.Container.Resources.Memory != "" {
		raw := expandArg(in.Manifest.Container.Resources.Memory, envMap)
		memory, err := parseByteSize(raw)
		if err != nil {
			return runtime.CreateContainerRequest{}, fmt.Errorf("container.resources.memory: %w", err)
		}
		host.Memory = memory
	}

	req := runtime.CreateContainerRequest{
		Image:      in.ImageRef,
		Cmd:        expandCommand(in.Manifest.Container.Command, envMap),
		Env:        env,
		WorkingDir: in.Manifest.Container.WorkingDir,
		User:       in.Manifest.Container.User,
		Labels:     labels,
		HostConfig: host,
	}
	if len(exposedPorts) > 0 {
		req.ExposedPorts = exposedPorts
	}
	return req, nil
}

func expandCommand(cmd []string, env map[string]string) []string {
	out := make([]string, 0, len(cmd))
	for _, arg := range cmd {
		out = append(out, expandArg(arg, env))
	}
	return out
}

func expandArg(arg string, env map[string]string) string {
	if !strings.Contains(arg, "$") {
		return arg
	}
	var b strings.Builder
	for i := 0; i < len(arg); i++ {
		if arg[i] != '$' {
			b.WriteByte(arg[i])
			continue
		}
		if i+1 >= len(arg) {
			b.WriteByte(arg[i])
			continue
		}
		if arg[i+1] == '{' {
			end := strings.IndexByte(arg[i+2:], '}')
			if end < 0 {
				b.WriteByte(arg[i])
				continue
			}
			key := arg[i+2 : i+2+end]
			if val, ok := env[key]; ok {
				b.WriteString(val)
			} else {
				b.WriteString("${")
				b.WriteString(key)
				b.WriteByte('}')
			}
			i += end + 2
			continue
		}
		j := i + 1
		for j < len(arg) && isEnvNameChar(arg[j]) {
			j++
		}
		if j == i+1 {
			b.WriteByte(arg[i])
			continue
		}
		key := arg[i+1 : j]
		if val, ok := env[key]; ok {
			b.WriteString(val)
		} else {
			b.WriteString(arg[i:j])
		}
		i = j - 1
	}
	return b.String()
}

func isEnvNameChar(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// cgroupPerms defaults empty permissions to "rwm". A profile that
// declares a device without explicit permissions almost always
// wants full access — explicit "" usually means "I forgot to set
// it" rather than "I want zero permissions" (which would make the
// device useless).
func cgroupPerms(p string) string {
	if p == "" {
		return "rwm"
	}
	return p
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
