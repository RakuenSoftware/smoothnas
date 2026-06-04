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
// (plugin, service, instance) unit. A single-service plugin (service ==
// plugin name) keeps the bare plugin name as its base (matching operator
// expectations when they list with `lxc-ls`); extra services suffix the
// service name. Multi-instance plugins further suffix with -<n>.
func ContainerName(pluginName, service string, instance, instanceCount int) string {
	base := pluginName
	if service != "" && service != pluginName {
		base = pluginName + "-" + service
	}
	if instanceCount <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, instance)
}

// SetupTemplateImage is the cached template image name for the
// committed result of an lxc-distro service's setup script. Stable
// across reinstalls of the same (manifest version, packages, setup)
// triple so the setup container only runs when one of those changes.
// A service segment is appended only for extra services (service !=
// plugin name) so single-container plugins keep their original tag and
// multiple lxc-distro services in one plugin don't collide.
func SetupTemplateImage(pluginName, service, manifestVersion string) string {
	if service == "" || service == pluginName {
		return fmt.Sprintf("smoothnas-plugin-%s:%s", pluginName, manifestVersion)
	}
	return fmt.Sprintf("smoothnas-plugin-%s-%s:%s", pluginName, service, manifestVersion)
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
	Service  *Service // the manifest service this container realises
	Instance int
	ImageRef string      // resolved by Materialise; e.g. "ghcr.io/...@sha256:..."
	Volumes  []VolumeRow // this service's volumes, expanded with per-instance host paths
	Config   []ConfigRow // this service's config, already merged with manifest defaults
	// Discovery maps sibling service name → its reachable endpoint on the
	// plugin bridge. Used to render {{service.<name>.host}} and
	// {{service.<name>.port.<portName>}} tokens in the service's Env.
	Discovery map[string]ServiceEndpoint
	// Profiles is the merged profile fragments (devices, env, capAdd,
	// pidsLimit, oomScoreAdj, lxc raw config). When nil the renderer
	// adds no profile contribution; production callers pass a non-nil
	// *Resolved from plugin.Resolve.
	Profiles *Resolved
}

// ServiceEndpoint is a sibling service's reachable address on the plugin
// bridge (LXC2Docker has no embedded DNS, so discovery is by injected IP).
type ServiceEndpoint struct {
	Host  string
	Ports map[string]int // port name → container port
}

// renderDiscovery substitutes {{service.<name>.host}} and
// {{service.<name>.port.<portName>}} tokens against the discovery map.
// Unknown tokens are left verbatim so a misconfiguration is visible
// rather than silently blank.
func renderDiscovery(value string, disc map[string]ServiceEndpoint) string {
	if !strings.Contains(value, "{{") {
		return value
	}
	var b strings.Builder
	rest := value
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			b.WriteString(rest[start:])
			break
		}
		token := strings.TrimSpace(rest[start+2 : start+end])
		b.WriteString(resolveDiscoveryToken(token, disc))
		rest = rest[start+end+2:]
	}
	return b.String()
}

func resolveDiscoveryToken(token string, disc map[string]ServiceEndpoint) string {
	parts := strings.Split(token, ".")
	if len(parts) >= 3 && parts[0] == "service" {
		if ep, ok := disc[parts[1]]; ok {
			switch {
			case parts[2] == "host":
				return ep.Host
			case parts[2] == "port" && len(parts) >= 4:
				if p, ok := ep.Ports[parts[3]]; ok {
					return strconv.Itoa(p)
				}
			}
		}
	}
	return "{{" + token + "}}"
}

// BuildCreatePayload renders the runtime.CreateContainerRequest for
// a single instance. Pure function — no DB access, no I/O — so unit
// tests are straightforward.
//
// Returns an error if a required volume's host path for this instance
// is missing or empty (which would mean phase 03 hasn't resolved a
// tier-bound volume yet, and the caller is racing the resolver).
func BuildCreatePayload(in PayloadInputs) (runtime.CreateContainerRequest, error) {
	if in.Plugin == nil || in.Service == nil {
		return runtime.CreateContainerRequest{}, fmt.Errorf("BuildCreatePayload: nil plugin or service")
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

	// Build the env list. Precedence (low→high): profile-contributed
	// env, the service's static Env (where discovery tokens live), then
	// plugin_config entries — so operator-tuned plugin_config wins on
	// key collisions. Discovery tokens ({{service.X.host}}) are rendered
	// against sibling endpoints as values are added.
	envMap := map[string]string{}
	if in.Profiles != nil {
		for k, v := range in.Profiles.Env {
			envMap[k] = v
		}
	}
	for k, v := range in.Service.Env {
		envMap[k] = renderDiscovery(v, in.Discovery)
	}
	for _, c := range in.Config {
		envMap[c.Key] = renderDiscovery(c.Value, in.Discovery)
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
	gpu, err := selectedGPU(in.Service.Config, envMap)
	if err != nil {
		return runtime.CreateContainerRequest{}, err
	}

	labels := map[string]string{
		runtime.PluginManagedLabel:           "true",
		runtime.PluginNameLabel:              in.Plugin.Name,
		runtime.PluginServiceLabel:           in.Service.Name,
		runtime.PluginVersionLabel:           in.Plugin.Version,
		runtime.PluginInstanceLabel:          strconv.Itoa(in.Instance),
		runtime.LXC2DockerBindMountInitLabel: "image",
	}

	host := runtime.HostConfig{
		Binds:       binds,
		NetworkMode: runtime.PluginBridgeName,
		RestartPolicy: runtime.RestartPolicy{
			Name: dockerRestartPolicyName(in.Service.EffectiveRestartPolicy()),
		},
	}
	exposedPorts := map[string]struct{}{}
	portBindings := map[string][]runtime.PortBinding{}
	for _, p := range in.Service.Ports {
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
		seenDevices := map[string]bool{}
		for _, d := range in.Profiles.Devices {
			d = gpu.rewriteDevice(d)
			deviceKey := d.Path + "\x00" + cgroupPerms(d.CgroupPermissions)
			if seenDevices[deviceKey] {
				continue
			}
			seenDevices[deviceKey] = true
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
		rawConfig := gpu.rewriteRawConfig(in.Profiles.LXCRaw)
		for i, raw := range rawConfig {
			labels[fmt.Sprintf("io.smoothnas.lxc.raw.%d", i)] = raw
		}
	}
	if in.Service.Container.Resources.Memory != "" {
		raw := expandArg(in.Service.Container.Resources.Memory, envMap)
		memory, err := parseByteSize(raw)
		if err != nil {
			return runtime.CreateContainerRequest{}, fmt.Errorf("container.resources.memory: %w", err)
		}
		host.Memory = memory
	}
	if in.Service.Container.Resources.CPU != "" {
		raw := expandArg(in.Service.Container.Resources.CPU, envMap)
		nanoCPUs, err := parseCPUCount(raw)
		if err != nil {
			return runtime.CreateContainerRequest{}, fmt.Errorf("container.resources.cpu: %w", err)
		}
		host.NanoCPUs = nanoCPUs
	}

	req := runtime.CreateContainerRequest{
		Image:      in.ImageRef,
		Cmd:        expandCommand(in.Service.Container.Command, envMap),
		Env:        env,
		WorkingDir: in.Service.Container.WorkingDir,
		User:       in.Service.Container.User,
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

type gpuSelection struct {
	Vendor string
	Path   string
}

func selectedGPU(config []ConfigField, env map[string]string) (gpuSelection, error) {
	for _, f := range config {
		if f.Type != ConfigTypeGPU {
			continue
		}
		path := strings.TrimSpace(env[f.Key])
		if path == "" {
			continue
		}
		gpu := gpuSelection{Vendor: f.GPUVendor, Path: path}
		if gpu.Vendor == "" {
			gpu.Vendor = inferGPUVendor(path)
		}
		if !gpu.valid() {
			return gpuSelection{}, fmt.Errorf("config %s: invalid GPU device %q for vendor %q", f.Key, path, f.GPUVendor)
		}
		return gpu, nil
	}
	return gpuSelection{}, nil
}

func inferGPUVendor(path string) string {
	switch {
	case isNVIDIAGPUPath(path):
		return GPUVendorNVIDIA
	case isDRIRenderPath(path):
		return GPUVendorAMD
	default:
		return ""
	}
}

func (g gpuSelection) valid() bool {
	switch g.Vendor {
	case "":
		return g.Path == ""
	case GPUVendorNVIDIA:
		return isNVIDIAGPUPath(g.Path)
	case GPUVendorAMD, GPUVendorIntel:
		return isDRIRenderPath(g.Path)
	default:
		return false
	}
}

func (g gpuSelection) rewriteDevice(d ProfileDevice) ProfileDevice {
	if g.Path == "" {
		return d
	}
	switch g.Vendor {
	case GPUVendorNVIDIA:
		if isNVIDIAGPUPath(d.Path) {
			d.Path = g.Path
		}
	case GPUVendorAMD, GPUVendorIntel:
		if d.Path == "/dev/dri" || isDRIRenderPath(d.Path) {
			d.Path = g.Path
		}
	}
	return d
}

func (g gpuSelection) rewriteRawConfig(raw []string) []string {
	if g.Path == "" {
		return raw
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, line := range raw {
		next := g.rewriteRawLine(line)
		if seen[next] {
			continue
		}
		seen[next] = true
		out = append(out, next)
	}
	return out
}

func (g gpuSelection) rewriteRawLine(line string) string {
	if g.Path == "" || !strings.HasPrefix(line, "lxc.mount.entry = ") {
		return line
	}
	switch g.Vendor {
	case GPUVendorNVIDIA:
		fields := strings.Fields(line)
		if len(fields) >= 4 && isNVIDIAGPUPath(fields[2]) {
			return lxcDeviceMountLine(g.Path)
		}
	case GPUVendorAMD, GPUVendorIntel:
		fields := strings.Fields(line)
		if len(fields) >= 4 && (fields[2] == "/dev/dri" || isDRIRenderPath(fields[2])) {
			return lxcDeviceMountLine(g.Path)
		}
	}
	return line
}

func lxcDeviceMountLine(path string) string {
	return fmt.Sprintf("lxc.mount.entry = %s %s none bind,optional,create=file 0 0", path, strings.TrimPrefix(path, "/"))
}

func isNVIDIAGPUPath(path string) bool {
	const prefix = "/dev/nvidia"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return allDigits(path[len(prefix):])
}

func isDRIRenderPath(path string) bool {
	const prefix = "/dev/dri/renderD"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return allDigits(path[len(prefix):])
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
