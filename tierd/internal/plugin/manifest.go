package plugin

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the only manifest apiVersion this build understands.
const APIVersion = "smoothnas.io/v1"

// Kind is the only manifest kind this build understands.
const Kind = "Plugin"

// Artifact type discriminators.
const (
	ArtifactOCIImage  = "oci-image"
	ArtifactLXCDistro = "lxc-distro"
)

// Volume modes.
const (
	VolumeModeTierBound = "tier-bound"
	VolumeModeFlat      = "flat"
)

// Restart-policy values.
const (
	RestartUnlessStopped = "unless-stopped"
	RestartOnFailure     = "on-failure"
	RestartNo            = "no"
)

// UI auth modes.
const (
	AuthNone           = "none"
	AuthBearerInjected = "bearer-injected"
)

// Manifest is the parsed in-memory form of smoothnas-plugin.yaml.
// Field-level validation lives in ValidateManifest.
type Manifest struct {
	APIVersion string        `json:"apiVersion" yaml:"apiVersion"`
	Kind       string        `json:"kind" yaml:"kind"`
	Metadata   Metadata      `json:"metadata" yaml:"metadata"`
	Artifact   Artifact      `json:"artifact" yaml:"artifact"`
	Container  Container     `json:"container" yaml:"container"`
	Instances  Instances     `json:"instances" yaml:"instances"`
	Volumes    []Volume      `json:"volumes,omitempty" yaml:"volumes"`
	Ports      []Port        `json:"ports,omitempty" yaml:"ports"`
	UI         *UI           `json:"ui,omitempty" yaml:"ui,omitempty"`
	Profiles   []string      `json:"profiles,omitempty" yaml:"profiles"`
	Config     []ConfigField `json:"config,omitempty" yaml:"config"`
}

// Metadata is the descriptive header.
type Metadata struct {
	Name        string `json:"name" yaml:"name"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description,omitempty" yaml:"description"`
	Vendor      string `json:"vendor,omitempty" yaml:"vendor"`
	Homepage    string `json:"homepage,omitempty" yaml:"homepage"`
}

// Artifact is a tagged union over Type. The unselected sub-struct is
// ignored at validation time, but yaml.v3 happily deserialises both
// since we use inline embedding with a discriminator.
type Artifact struct {
	Type string `json:"type" yaml:"type"`

	// oci-image fields
	Image  string `json:"image,omitempty" yaml:"image,omitempty"`
	Digest string `json:"digest,omitempty" yaml:"digest,omitempty"`

	// lxc-distro fields
	Distro   string   `json:"distro,omitempty" yaml:"distro,omitempty"`
	Release  string   `json:"release,omitempty" yaml:"release,omitempty"`
	Arch     string   `json:"arch,omitempty" yaml:"arch,omitempty"`
	Packages []string `json:"packages,omitempty" yaml:"packages,omitempty"`
	Setup    []string `json:"setup,omitempty" yaml:"setup,omitempty"`
}

// Container holds runtime knobs that apply to both artifact types.
type Container struct {
	Command       []string `json:"command,omitempty" yaml:"command,omitempty"`
	WorkingDir    string   `json:"workingDir,omitempty" yaml:"workingDir,omitempty"`
	User          string   `json:"user,omitempty" yaml:"user,omitempty"`
	RestartPolicy string   `json:"restartPolicy" yaml:"restartPolicy"`
}

// Instances controls replica fan-out. When omitted entirely the
// manifest is treated as { Count: 1, Configurable: false }.
type Instances struct {
	Count        int  `json:"count" yaml:"count"`
	Configurable bool `json:"configurable" yaml:"configurable"`
}

// Volume describes one persistent mount. PerInstance has no effect
// when Count == 1.
type Volume struct {
	Name        string `json:"name" yaml:"name"`
	Mode        string `json:"mode" yaml:"mode"`
	Slot        string `json:"slot,omitempty" yaml:"slot,omitempty"`
	MinSize     string `json:"minSize,omitempty" yaml:"minSize,omitempty"`
	Bind        string `json:"bind" yaml:"bind"`
	PerInstance bool   `json:"perInstance,omitempty" yaml:"perInstance,omitempty"`
}

// Port describes one port the container listens on. Phase 04 reads
// Expose to decide whether to render an nginx route. HostExpose is
// reserved for phase 09 and ignored in phase 1–4.
type Port struct {
	Name       string `json:"name" yaml:"name"`
	Port       int    `json:"port" yaml:"port"`
	Protocol   string `json:"protocol" yaml:"protocol"`
	Expose     bool   `json:"expose" yaml:"expose"`
	HostExpose bool   `json:"hostExpose,omitempty" yaml:"hostExpose,omitempty"`
}

// UI describes how the plugin's own HTTP UI should be embedded in
// the SmoothNAS browser. Phase 07 owns the embed page.
type UI struct {
	Embed UIEmbed `json:"embed" yaml:"embed"`
}

// UIEmbed is the embed sub-block.
type UIEmbed struct {
	Path string `json:"path" yaml:"path"`
	Auth string `json:"auth" yaml:"auth"`
}

// ConfigField declares an operator-tunable parameter. The value
// chosen at install time is recorded in plugin_config and passed
// to the container as an environment variable named Key.
type ConfigField struct {
	Key         string `json:"key" yaml:"key"`
	Type        string `json:"type" yaml:"type"`
	Default     string `json:"default,omitempty" yaml:"default,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Secret      bool   `json:"secret,omitempty" yaml:"secret,omitempty"`
}

// ParseManifest parses a manifest YAML document. Strict decoding is
// enabled so unknown fields produce a clear error instead of silently
// being dropped — operators sideloading bad manifests should learn
// about typos at parse time.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Field-level regexes. Public so tests and other packages can re-use
// them when constructing fixtures.
var (
	reName        = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$`)
	reSemver      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	reHexDigest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	reUnit        = regexp.MustCompile(`^[A-Za-z0-9_@.-]+\.service$`)
	reVolumeName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reConfigKey   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	reDistroToken = regexp.MustCompile(`^[a-z0-9._-]+$`)
)

// Recognised LXC2Docker arches. Empty arch defaults to the host arch
// at install time; no need to include "" here.
var validArches = map[string]struct{}{
	"amd64": {}, "arm64": {}, "armhf": {}, "i386": {},
}

// ValidateManifest checks every field-level rule. It collects every
// failure into the returned *ValidationError so operators see the
// full list at once. Returns nil when the manifest is valid.
func ValidateManifest(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("validate: nil manifest")
	}
	v := &ValidationError{}

	if m.APIVersion != APIVersion {
		v.add("apiVersion", "must be %q (got %q)", APIVersion, m.APIVersion)
	}
	if m.Kind != Kind {
		v.add("kind", "must be %q (got %q)", Kind, m.Kind)
	}

	validateMetadata(v, &m.Metadata)
	validateArtifact(v, &m.Artifact)
	validateContainer(v, &m.Container, &m.Artifact)
	validateInstances(v, &m.Instances, m.Volumes)
	validateVolumes(v, m.Volumes)
	validatePorts(v, m.Ports)
	validateUI(v, m.UI)
	validateConfig(v, m.Config)

	return v.asError()
}

func validateMetadata(v *ValidationError, m *Metadata) {
	if !reName.MatchString(m.Name) {
		v.add("metadata.name", "must match %s (DNS-1123 label, ≤40 chars)", reName)
	}
	if !reSemver.MatchString(m.Version) {
		v.add("metadata.version", "must be semver MAJOR.MINOR.PATCH (got %q)", m.Version)
	}
}

func validateArtifact(v *ValidationError, a *Artifact) {
	switch a.Type {
	case ArtifactOCIImage:
		if a.Image == "" {
			v.add("artifact.image", "is required for type %q", ArtifactOCIImage)
		}
		if a.Digest != "" && !reHexDigest.MatchString(a.Digest) {
			v.add("artifact.digest", "must match %s when present", reHexDigest)
		}
		// Reject lxc-distro fields populated by mistake.
		if a.Distro != "" || a.Release != "" || len(a.Packages) > 0 || len(a.Setup) > 0 {
			v.add("artifact", "lxc-distro fields (distro/release/packages/setup) must be empty when type is %q", ArtifactOCIImage)
		}
	case ArtifactLXCDistro:
		if !reDistroToken.MatchString(a.Distro) {
			v.add("artifact.distro", "is required and must match %s", reDistroToken)
		}
		if !reDistroToken.MatchString(a.Release) {
			v.add("artifact.release", "is required and must match %s", reDistroToken)
		}
		if a.Arch != "" {
			if _, ok := validArches[a.Arch]; !ok {
				v.add("artifact.arch", "must be one of amd64/arm64/armhf/i386 (got %q)", a.Arch)
			}
		}
		// Reject oci-image fields populated by mistake.
		if a.Image != "" || a.Digest != "" {
			v.add("artifact", "oci-image fields (image/digest) must be empty when type is %q", ArtifactLXCDistro)
		}
	case "":
		v.add("artifact.type", "is required (must be %q or %q)", ArtifactOCIImage, ArtifactLXCDistro)
	default:
		v.add("artifact.type", "must be %q or %q (got %q)", ArtifactOCIImage, ArtifactLXCDistro, a.Type)
	}
}

func validateContainer(v *ValidationError, c *Container, a *Artifact) {
	switch c.RestartPolicy {
	case "", RestartUnlessStopped, RestartOnFailure, RestartNo:
		// "" means "use default" (unless-stopped) — install.go fills it in.
	default:
		v.add("container.restartPolicy", "must be one of %q/%q/%q (got %q)",
			RestartUnlessStopped, RestartOnFailure, RestartNo, c.RestartPolicy)
	}
	// lxc-distro plugins must declare a command — distro templates have
	// no default CMD.
	if a.Type == ArtifactLXCDistro && len(c.Command) == 0 {
		v.add("container.command", "is required when artifact.type is %q", ArtifactLXCDistro)
	}
}

func validateInstances(v *ValidationError, in *Instances, vols []Volume) {
	if in.Count == 0 {
		// Treated as default 1 by callers; not an error.
		return
	}
	if in.Count < 1 {
		v.add("instances.count", "must be ≥ 1 (got %d)", in.Count)
	}
	if in.Count > 1 {
		anyPerInstance := false
		for _, vol := range vols {
			if vol.PerInstance {
				anyPerInstance = true
				break
			}
		}
		if !anyPerInstance && len(vols) > 0 {
			// Warning, not error. Recorded as an issue with a clear
			// message; install.go currently treats every issue as
			// blocking. A separate Warnings collection is a future
			// enhancement.
			v.add("instances", "count > 1 with no perInstance volumes — all replicas will share state (set perInstance: true on at least one volume, or ignore this if shared state is intended)")
		}
	}
}

func validateVolumes(v *ValidationError, vols []Volume) {
	seen := map[string]bool{}
	for i, vol := range vols {
		field := fmt.Sprintf("volumes[%d]", i)
		if !reVolumeName.MatchString(vol.Name) {
			v.add(field+".name", "must match %s", reVolumeName)
		}
		if seen[vol.Name] {
			v.add(field+".name", "duplicate volume name %q", vol.Name)
		}
		seen[vol.Name] = true

		switch vol.Mode {
		case VolumeModeTierBound:
			if vol.Slot == "" {
				v.add(field+".slot", "is required when mode is %q", VolumeModeTierBound)
			}
			// Slot value enumeration against tier_levels is phase 03;
			// here we just require non-empty.
		case VolumeModeFlat:
			if vol.Slot != "" {
				v.add(field+".slot", "must be empty when mode is %q", VolumeModeFlat)
			}
		case "":
			v.add(field+".mode", "is required (%q or %q)", VolumeModeTierBound, VolumeModeFlat)
		default:
			v.add(field+".mode", "must be %q or %q (got %q)", VolumeModeTierBound, VolumeModeFlat, vol.Mode)
		}

		if vol.Bind == "" || !strings.HasPrefix(vol.Bind, "/") {
			v.add(field+".bind", "must be an absolute path (got %q)", vol.Bind)
		}
	}
}

func validatePorts(v *ValidationError, ports []Port) {
	seen := map[string]bool{}
	for i, p := range ports {
		field := fmt.Sprintf("ports[%d]", i)
		if p.Name == "" {
			v.add(field+".name", "is required")
		}
		if seen[p.Name] {
			v.add(field+".name", "duplicate port name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Port < 1 || p.Port > 65535 {
			v.add(field+".port", "must be 1..65535 (got %d)", p.Port)
		}
		switch p.Protocol {
		case "tcp", "udp":
		default:
			v.add(field+".protocol", "must be \"tcp\" or \"udp\" (got %q)", p.Protocol)
		}
	}
}

func validateUI(v *ValidationError, ui *UI) {
	if ui == nil {
		return
	}
	switch ui.Embed.Auth {
	case "", AuthNone, AuthBearerInjected:
	default:
		v.add("ui.embed.auth", "must be %q or %q (got %q)", AuthNone, AuthBearerInjected, ui.Embed.Auth)
	}
}

func validateConfig(v *ValidationError, fields []ConfigField) {
	seen := map[string]bool{}
	for i, f := range fields {
		field := fmt.Sprintf("config[%d]", i)
		if !reConfigKey.MatchString(f.Key) {
			v.add(field+".key", "must match %s", reConfigKey)
		}
		if seen[f.Key] {
			v.add(field+".key", "duplicate key %q", f.Key)
		}
		seen[f.Key] = true
	}
}

// EffectiveCount returns the number of instances after defaulting.
// A manifest with no instances block is treated as 1.
func (m *Manifest) EffectiveCount() int {
	if m.Instances.Count <= 0 {
		return 1
	}
	return m.Instances.Count
}

// EffectiveRestartPolicy returns the restart policy after defaulting.
func (m *Manifest) EffectiveRestartPolicy() string {
	if m.Container.RestartPolicy == "" {
		return RestartUnlessStopped
	}
	return m.Container.RestartPolicy
}

// DistroSummary returns a single-line description of an lxc-distro
// artifact for display ("ubuntu/jammy/amd64"). Empty for oci-image.
func (m *Manifest) DistroSummary() string {
	if m.Artifact.Type != ArtifactLXCDistro {
		return ""
	}
	arch := m.Artifact.Arch
	if arch == "" {
		arch = "host"
	}
	return strings.Join([]string{m.Artifact.Distro, m.Artifact.Release, arch}, "/")
}
