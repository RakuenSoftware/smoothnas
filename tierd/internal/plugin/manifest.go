package plugin

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
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
	// ArtifactCompose marks a compose-format plugin (plugins-11): its
	// "manifest" is a docker-compose project, its services/volumes/lifecycle
	// are owned by docker compose, and it has no plugin_services rows.
	ArtifactCompose = "compose"
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

// dependsOn start conditions.
const (
	DependsServiceStarted = "service_started"
	DependsServiceHealthy = "service_healthy"
)

// Config field types.
const (
	ConfigTypeString  = "string"
	ConfigTypeNumber  = "number"
	ConfigTypeSelect  = "select"
	ConfigTypeBoolean = "boolean"
	ConfigTypeGPU     = "gpu"
)

// GPU vendor selectors accepted by config fields with type=gpu.
const (
	GPUVendorNVIDIA = "nvidia"
	GPUVendorAMD    = "amd"
	GPUVendorIntel  = "intel"
)

// Manifest is the parsed in-memory form of smoothnas-plugin.yaml.
// Field-level validation lives in ValidateManifest.
//
// metadata, instances, ui, and profiles are plugin-level. Everything
// that was per-image — artifact, container, volumes, ports, config —
// lives under a named Service: a plugin owns its services as a single
// managed set (compose-style). See plugins-10-compose-services.
type Manifest struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind" yaml:"kind"`
	Metadata   Metadata  `json:"metadata" yaml:"metadata"`
	Instances  Instances `json:"instances" yaml:"instances"`
	UI         *UI       `json:"ui,omitempty" yaml:"ui,omitempty"`
	Profiles   []string  `json:"profiles,omitempty" yaml:"profiles"`

	// Config is the plugin-level operator-tunable schema for a plain-compose
	// plugin (from x-smoothnas.config), surfaced so the install wizard renders a
	// form. Native manifests carry config per-Service instead and leave this nil.
	Config []ConfigField `json:"config,omitempty" yaml:"-"`

	// Services is the set of containers this plugin owns. A single-
	// container plugin is one service. New manifests should use this
	// shape; pre-plugins-10 manifests using the top-level fields below
	// are auto-wrapped into a single service by ParseManifest.
	Services []Service `json:"services" yaml:"services,omitempty"`

	// Legacy single-image top-level fields (pre-plugins-10). When present
	// and Services is empty, ParseManifest folds them into one service
	// named after the plugin — so already-installed plugins (whose stored
	// manifest is re-parsed on every Materialise) and third-party
	// single-image manifests keep working. Hidden from JSON: the API and
	// the rest of the runtime only ever see the normalized Services view.
	LegacyArtifact      *Artifact      `json:"-" yaml:"artifact,omitempty"`
	LegacyContainer     *Container     `json:"-" yaml:"container,omitempty"`
	LegacyContainerRefs []ContainerRef `json:"-" yaml:"containerRefs,omitempty"`
	LegacyVolumes       []Volume       `json:"-" yaml:"volumes,omitempty"`
	LegacyPorts         []Port         `json:"-" yaml:"ports,omitempty"`
	LegacyConfig        []ConfigField  `json:"-" yaml:"config,omitempty"`

	// isCompose marks a manifest that ParseManifest built from a plain
	// docker-compose file (top-level services: map, no smoothnas.io/*
	// apiVersion) rather than the native format. Only Metadata + UI are
	// populated (for catalog display + console embed); the compose YAML
	// itself is the runtime source of truth and drives the ArtifactCompose
	// lifecycle. Unexported so it neither serializes nor affects the strict
	// decode of native manifests. ValidateManifest branches on it.
	isCompose bool
}

// IsCompose reports whether this manifest was parsed from a plain compose file.
func (m *Manifest) IsCompose() bool { return m != nil && m.isCompose }

// Service is one container within a plugin. Artifact, container knobs,
// volumes, ports, and config are all per-service; the plugin owns the
// services as one lifecycle unit. Replica fan-out (instances) is
// plugin-level and applies to every service uniformly.
type Service struct {
	Name          string                      `json:"name" yaml:"name"`
	Artifact      Artifact                    `json:"artifact" yaml:"artifact"`
	ContainerRefs []ContainerRef              `json:"containerRefs,omitempty" yaml:"containerRefs,omitempty"`
	Container     Container                   `json:"container,omitempty" yaml:"container,omitempty"`
	Env           map[string]string           `json:"env,omitempty" yaml:"env,omitempty"`
	Volumes       []Volume                    `json:"volumes,omitempty" yaml:"volumes"`
	Ports         []Port                      `json:"ports,omitempty" yaml:"ports"`
	Config        []ConfigField               `json:"config,omitempty" yaml:"config"`
	DependsOn     map[string]DependsCondition `json:"dependsOn,omitempty" yaml:"dependsOn,omitempty"`
	Health        *Healthcheck                `json:"health,omitempty" yaml:"health,omitempty"`
}

// ContainerRef declares one OCI ref a service depends on. The service's
// artifact.image is always available as the implicit "primary" ref.
type ContainerRef struct {
	Name   string `json:"name" yaml:"name"`
	Image  string `json:"image" yaml:"image"`
	Digest string `json:"digest,omitempty" yaml:"digest,omitempty"`
}

// DependsCondition is the start gate for one dependsOn edge. The map
// key (in Service.DependsOn) is the sibling service name.
type DependsCondition struct {
	Condition string `json:"condition" yaml:"condition"`
}

// Healthcheck mirrors the compose / LXC2Docker healthcheck surface. A
// dependent declaring a service_healthy dependency requires its target
// to carry one.
type Healthcheck struct {
	Test        []string `json:"test" yaml:"test"`
	Interval    string   `json:"interval,omitempty" yaml:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Retries     int      `json:"retries,omitempty" yaml:"retries,omitempty"`
	StartPeriod string   `json:"startPeriod,omitempty" yaml:"startPeriod,omitempty"`
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
	Command       []string  `json:"command,omitempty" yaml:"command,omitempty"`
	WorkingDir    string    `json:"workingDir,omitempty" yaml:"workingDir,omitempty"`
	User          string    `json:"user,omitempty" yaml:"user,omitempty"`
	RestartPolicy string    `json:"restartPolicy" yaml:"restartPolicy"`
	Resources     Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
}

// Resources holds runtime resource limits that can be literal values or
// interpolated from config keys. Values are rendered into HostConfig by the
// payload builder rather than passed only as container environment.
type Resources struct {
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
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

// Port describes one port the container listens on. Expose renders
// an nginx route; HostExpose publishes the same container port on the
// SmoothNAS host through the runtime daemon.
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
	// Service and Port are populated for plain-compose plugins (from
	// x-smoothnas.ui): Service names the compose service that hosts the UI
	// and Port is that service's CONTAINER-side port. Native single-service
	// plugins leave them empty (the embed target is unambiguous). The console
	// builds the iframe src from the discovered service endpoint at runtime.
	Service string `json:"service,omitempty" yaml:"service,omitempty"`
	Port    int    `json:"port,omitempty" yaml:"port,omitempty"`
}

// ConfigField declares an operator-tunable parameter. The value
// chosen at install time is recorded in plugin_config and passed
// to the container as an environment variable named Key.
type ConfigField struct {
	Key         string         `json:"key" yaml:"key"`
	Type        string         `json:"type" yaml:"type"`
	Label       string         `json:"label,omitempty" yaml:"label,omitempty"`
	Default     string         `json:"default,omitempty" yaml:"default,omitempty"`
	Description string         `json:"description,omitempty" yaml:"description,omitempty"`
	Secret      bool           `json:"secret,omitempty" yaml:"secret,omitempty"`
	GPUVendor   string         `json:"gpuVendor,omitempty" yaml:"gpuVendor,omitempty"`
	Options     []ConfigOption `json:"options,omitempty" yaml:"options,omitempty"`
	Min         string         `json:"min,omitempty" yaml:"min,omitempty"`
	Max         string         `json:"max,omitempty" yaml:"max,omitempty"`
	Step        string         `json:"step,omitempty" yaml:"step,omitempty"`
	Unit        string         `json:"unit,omitempty" yaml:"unit,omitempty"`
}

// ConfigOption is one selectable value for a config field with
// type=select. Values are still persisted as strings in plugin_config.
type ConfigOption struct {
	Value string `json:"value" yaml:"value"`
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
}

// ParseManifest parses a manifest YAML document. Strict decoding is
// enabled so unknown fields produce a clear error instead of silently
// being dropped — operators sideloading bad manifests should learn
// about typos at parse time.
func ParseManifest(data []byte) (*Manifest, error) {
	// A plain docker-compose plugin (top-level services: map, no
	// smoothnas.io/* apiVersion) carries no native metadata block; build a
	// display-only Manifest from the compose name + the x-smoothnas extras so
	// every ParseManifest caller (catalog, install, validate) gets a usable
	// Manifest without re-parsing. The compose YAML remains the runtime source.
	if compose.IsComposeFormat(data) {
		return manifestFromCompose(data)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	m.normalizeLegacy()
	return &m, nil
}

// manifestFromCompose builds a display-only *Manifest from a plain compose
// plugin: Name from the compose `name:` project, and Description/Vendor/
// Homepage/UI lifted from the top-level x-smoothnas block. Version is left
// blank here — a compose plugin's version comes from its catalog release tag,
// which ParseManifest doesn't see; the catalog stamps it. Marked isCompose so
// ValidateManifest applies compose (not native) rules.
func manifestFromCompose(data []byte) (*Manifest, error) {
	if err := compose.RejectMultiDoc(data); err != nil {
		return nil, err
	}
	name := compose.ProjectName(data)
	if !reName.MatchString(name) {
		return nil, fmt.Errorf("parse compose manifest: name %q must match %s (DNS-1123 label, ≤40 chars)", name, reName)
	}
	meta, err := compose.ParseMeta(data)
	if err != nil {
		return nil, err
	}
	// Validate the x-smoothnas.ui embed target against the real services now,
	// at ingest — compose ignores x-* entirely, so a mis-spelled ui.service
	// would otherwise fail silently at runtime instead of loudly at publish.
	svcNames, err := compose.ServiceNames(data)
	if err != nil {
		return nil, err
	}
	if err := compose.ValidateMeta(meta, svcNames); err != nil {
		return nil, fmt.Errorf("parse compose manifest: %w", err)
	}
	m := &Manifest{
		isCompose: true,
		Metadata: Metadata{
			Name:        name,
			Description: meta.Description,
			Vendor:      meta.Vendor,
			Homepage:    meta.Homepage,
		},
	}
	if meta.UI != nil {
		m.UI = &UI{Embed: UIEmbed{
			Path:    meta.UI.Path,
			Service: meta.UI.Service,
			Port:    meta.UI.Port,
		}}
	}
	// Surface the operator-config schema so the install wizard can render a form.
	schema, err := compose.ConfigSchema(data)
	if err != nil {
		return nil, err
	}
	for _, d := range schema {
		m.Config = append(m.Config, ConfigField{
			Key: d.Key, Label: d.Label, Type: d.Type,
			Default: d.Default, Description: d.Description, Secret: d.Secret,
		})
	}
	return m, nil
}

// normalizeLegacy folds a pre-plugins-10 single-image manifest (top-level
// artifact/container/volumes/ports/config) into one service named after
// the plugin, so older and third-party manifests keep parsing. The
// service name is the plugin name, which yields the same bare container
// name the plugin already runs under — so an installed plugin survives
// the upgrade without a recreate.
//
// A no-op when the manifest already uses services: or carries no legacy
// artifact. The both-shapes-present conflict is reported by ValidateManifest.
func (m *Manifest) normalizeLegacy() {
	if len(m.Services) > 0 || m.LegacyArtifact == nil {
		return
	}
	svc := Service{
		Name:          m.Metadata.Name,
		Artifact:      *m.LegacyArtifact,
		ContainerRefs: m.LegacyContainerRefs,
		Volumes:       m.LegacyVolumes,
		Ports:         m.LegacyPorts,
		Config:        m.LegacyConfig,
	}
	if m.LegacyContainer != nil {
		svc.Container = *m.LegacyContainer
	}
	m.Services = []Service{svc}
	m.LegacyArtifact = nil
	m.LegacyContainer = nil
	m.LegacyContainerRefs = nil
	m.LegacyVolumes = nil
	m.LegacyPorts = nil
	m.LegacyConfig = nil
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
	// A plain-compose plugin has no native apiVersion/kind/services to check —
	// the compose file is validated as compose (and `docker compose config`
	// runs at install). Here we only assert the catalog-display invariant: a
	// usable plugin name. Version is stamped from the release tag, and the
	// ui.service embed target was already checked at parse (manifestFromCompose).
	if m.isCompose {
		v := &ValidationError{}
		if !reName.MatchString(m.Metadata.Name) {
			v.add("name", "must match %s (DNS-1123 label, ≤40 chars)", reName)
		}
		return v.asError()
	}
	v := &ValidationError{}

	if m.APIVersion != APIVersion {
		v.add("apiVersion", "must be %q (got %q)", APIVersion, m.APIVersion)
	}
	if m.Kind != Kind {
		v.add("kind", "must be %q (got %q)", Kind, m.Kind)
	}

	// A normalized manifest never has both; this only fires when a YAML
	// mixed the legacy top-level artifact with an explicit services: block.
	if m.LegacyArtifact != nil && len(m.Services) > 0 {
		v.add("services", "set either the top-level artifact (legacy) or services, not both")
	}

	validateMetadata(v, &m.Metadata)
	validateInstances(v, &m.Instances, m.allVolumes())
	validateUI(v, m.UI)
	validateServices(v, m.Services)

	return v.asError()
}

// allVolumes flattens every service's volumes. The instances check only
// asks whether any volume opts into perInstance, so a union is enough.
func (m *Manifest) allVolumes() []Volume {
	var out []Volume
	for i := range m.Services {
		out = append(out, m.Services[i].Volumes...)
	}
	return out
}

// validateServices checks the service set: non-empty, unique DNS-label
// names, per-service artifact/container/volume/port/config rules, no
// host-port collisions across services, and an acyclic dependsOn graph.
func validateServices(v *ValidationError, services []Service) {
	if len(services) == 0 {
		v.add("services", "at least one service is required")
		return
	}

	names := make(map[string]bool, len(services))
	for i := range services {
		names[services[i].Name] = true
	}

	seenName := map[string]bool{}
	hostPorts := map[int]string{} // host-published container port -> owning service
	for i := range services {
		s := &services[i]
		prefix := fmt.Sprintf("services[%d]", i)

		if !reName.MatchString(s.Name) {
			v.add(prefix+".name", "must match %s (DNS-1123 label, ≤40 chars)", reName)
		}
		if seenName[s.Name] {
			v.add(prefix+".name", "duplicate service name %q", s.Name)
		}
		seenName[s.Name] = true

		validateArtifact(v, prefix, &s.Artifact)
		validateContainerRefs(v, prefix, s.ContainerRefs)
		validateContainer(v, prefix, &s.Container, &s.Artifact, s.Config)
		validateVolumes(v, prefix, s.Volumes)
		validatePorts(v, prefix, s.Ports)
		validateConfig(v, prefix, s.Config)

		// Host-published ports map to the same port on the SmoothNAS
		// host, so two services of one plugin can't claim the same one.
		for _, p := range s.Ports {
			if !p.HostExpose {
				continue
			}
			if owner, ok := hostPorts[p.Port]; ok {
				v.add(prefix+".ports", "host-published port %d already published by service %q", p.Port, owner)
			} else {
				hostPorts[p.Port] = s.Name
			}
		}

		validateDependsOn(v, prefix, s, names, services)
	}

	if cycle := dependsCycle(services); len(cycle) > 0 {
		v.add("services", "dependsOn graph has a cycle: %s", strings.Join(cycle, " -> "))
	}
}

func validateContainerRefs(v *ValidationError, prefix string, refs []ContainerRef) {
	seen := map[string]bool{"primary": true}
	for i, r := range refs {
		field := fmt.Sprintf("%s.containerRefs[%d]", prefix, i)
		if !reVolumeName.MatchString(r.Name) {
			v.add(field+".name", "must match %s", reVolumeName)
		}
		if r.Name == "primary" {
			v.add(field+".name", "%q is reserved for artifact.image", r.Name)
		}
		if seen[r.Name] {
			v.add(field+".name", "duplicate ref name %q", r.Name)
		}
		seen[r.Name] = true
		if r.Image == "" {
			v.add(field+".image", "is required")
		}
		if r.Digest != "" && !reHexDigest.MatchString(r.Digest) {
			v.add(field+".digest", "must match %s when present", reHexDigest)
		}
	}
}

// validateDependsOn checks one service's dependsOn edges: each target
// is a distinct sibling, the condition is recognised, and a
// service_healthy target actually declares a health block.
func validateDependsOn(v *ValidationError, prefix string, s *Service, names map[string]bool, services []Service) {
	for dep, cond := range s.DependsOn {
		field := prefix + ".dependsOn." + dep
		switch {
		case dep == s.Name:
			v.add(field, "service %q cannot depend on itself", s.Name)
		case !names[dep]:
			v.add(field, "references unknown service %q", dep)
		}
		switch cond.Condition {
		case DependsServiceStarted:
		case DependsServiceHealthy:
			if t := findService(services, dep); t != nil && t.Health == nil {
				v.add(field, "condition %q requires service %q to declare a health block", DependsServiceHealthy, dep)
			}
		case "":
			v.add(field, "condition is required (%q or %q)", DependsServiceStarted, DependsServiceHealthy)
		default:
			v.add(field, "condition must be %q or %q (got %q)", DependsServiceStarted, DependsServiceHealthy, cond.Condition)
		}
	}
}

// dependsCycle returns a service-name path describing a cycle in the
// dependsOn graph, or nil when it is acyclic. Edges to unknown services
// are skipped (validateDependsOn reports those separately). Iteration
// order is fixed (declared order, sorted edges) for stable messages.
func dependsCycle(services []Service) []string {
	known := make(map[string]bool, len(services))
	for i := range services {
		known[services[i].Name] = true
	}
	graph := make(map[string][]string, len(services))
	for i := range services {
		s := &services[i]
		deps := make([]string, 0, len(s.DependsOn))
		for dep := range s.DependsOn {
			if known[dep] && dep != s.Name {
				deps = append(deps, dep)
			}
		}
		sort.Strings(deps)
		graph[s.Name] = deps
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var path []string
	var visit func(n string) []string
	visit = func(n string) []string {
		color[n] = gray
		path = append(path, n)
		for _, m := range graph[n] {
			switch color[m] {
			case gray:
				// Back-edge: slice the path from m to close the cycle.
				for idx, p := range path {
					if p == m {
						return append(append([]string{}, path[idx:]...), m)
					}
				}
			case white:
				if c := visit(m); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return nil
	}

	for i := range services {
		n := services[i].Name
		if color[n] == white {
			path = path[:0]
			if c := visit(n); c != nil {
				return c
			}
		}
	}
	return nil
}

// findService returns the service with the given name, or nil.
func findService(services []Service, name string) *Service {
	for i := range services {
		if services[i].Name == name {
			return &services[i]
		}
	}
	return nil
}

func validateMetadata(v *ValidationError, m *Metadata) {
	if !reName.MatchString(m.Name) {
		v.add("metadata.name", "must match %s (DNS-1123 label, ≤40 chars)", reName)
	}
	if !reSemver.MatchString(m.Version) {
		v.add("metadata.version", "must be semver MAJOR.MINOR.PATCH (got %q)", m.Version)
	}
}

func validateArtifact(v *ValidationError, prefix string, a *Artifact) {
	switch a.Type {
	case ArtifactOCIImage:
		if a.Image == "" {
			v.add(prefix+".artifact.image", "is required for type %q", ArtifactOCIImage)
		}
		if a.Digest != "" && !reHexDigest.MatchString(a.Digest) {
			v.add(prefix+".artifact.digest", "must match %s when present", reHexDigest)
		}
		// Reject lxc-distro fields populated by mistake.
		if a.Distro != "" || a.Release != "" || len(a.Packages) > 0 || len(a.Setup) > 0 {
			v.add(prefix+".artifact", "lxc-distro fields (distro/release/packages/setup) must be empty when type is %q", ArtifactOCIImage)
		}
	case ArtifactLXCDistro:
		if !reDistroToken.MatchString(a.Distro) {
			v.add(prefix+".artifact.distro", "is required and must match %s", reDistroToken)
		}
		if !reDistroToken.MatchString(a.Release) {
			v.add(prefix+".artifact.release", "is required and must match %s", reDistroToken)
		}
		if a.Arch != "" {
			if _, ok := validArches[a.Arch]; !ok {
				v.add(prefix+".artifact.arch", "must be one of amd64/arm64/armhf/i386 (got %q)", a.Arch)
			}
		}
		// Reject oci-image fields populated by mistake.
		if a.Image != "" || a.Digest != "" {
			v.add(prefix+".artifact", "oci-image fields (image/digest) must be empty when type is %q", ArtifactLXCDistro)
		}
	case "":
		v.add(prefix+".artifact.type", "is required (must be %q or %q)", ArtifactOCIImage, ArtifactLXCDistro)
	default:
		v.add(prefix+".artifact.type", "must be %q or %q (got %q)", ArtifactOCIImage, ArtifactLXCDistro, a.Type)
	}
}

func validateContainer(v *ValidationError, prefix string, c *Container, a *Artifact, config []ConfigField) {
	switch c.RestartPolicy {
	case "", RestartUnlessStopped, RestartOnFailure, RestartNo:
		// "" means "use default" (unless-stopped) — install.go fills it in.
	default:
		v.add(prefix+".container.restartPolicy", "must be one of %q/%q/%q (got %q)",
			RestartUnlessStopped, RestartOnFailure, RestartNo, c.RestartPolicy)
	}
	// lxc-distro services must declare a command — distro templates have
	// no default CMD.
	if a.Type == ArtifactLXCDistro && len(c.Command) == 0 {
		v.add(prefix+".container.command", "is required when artifact.type is %q", ArtifactLXCDistro)
	}
	validateResources(v, prefix, &c.Resources, config)
}

func validateResources(v *ValidationError, prefix string, r *Resources, config []ConfigField) {
	validateConfigurableResource(v, prefix+".container.resources.memory", r.Memory, config, parseByteSize)
	validateConfigurableResource(v, prefix+".container.resources.cpu", r.CPU, config, parseCPUCount)
}

func validateConfigurableResource(v *ValidationError, field, value string, config []ConfigField, parse func(string) (int64, error)) {
	if value == "" {
		return
	}
	if key, ok := configReference(value); ok {
		if !configKeyExists(config, key) {
			v.add(field, "references unknown config key %q", key)
		}
		return
	}
	if _, err := parse(value); err != nil {
		v.add(field, "%v", err)
	}
}

func configReference(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
	if !reConfigKey.MatchString(key) {
		return "", false
	}
	return key, true
}

func configKeyExists(config []ConfigField, key string) bool {
	for _, f := range config {
		if f.Key == key {
			return true
		}
	}
	return false
}

func parseByteSize(value string) (int64, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, fmt.Errorf("must be a positive byte size")
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("must start with a positive integer byte size")
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive byte size")
	}
	mult := int64(1)
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b", "byte", "bytes":
		mult = 1
	case "k", "kb":
		mult = 1000
	case "ki", "kib":
		mult = 1 << 10
	case "m", "mb":
		mult = 1000 * 1000
	case "mi", "mib":
		mult = 1 << 20
	case "g", "gb":
		mult = 1000 * 1000 * 1000
	case "gi", "gib":
		mult = 1 << 30
	case "t", "tb":
		mult = 1000 * 1000 * 1000 * 1000
	case "ti", "tib":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("has unsupported size suffix %q", strings.TrimSpace(s[i:]))
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("byte size overflows int64")
	}
	return n * mult, nil
}

func parseCPUCount(value string) (int64, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, fmt.Errorf("must be a positive CPU count")
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 0, fmt.Errorf("must be a positive CPU count")
	}
	if n > float64(math.MaxInt64)/1_000_000_000 {
		return 0, fmt.Errorf("CPU count overflows int64 nanocpus")
	}
	nano := int64(n * 1_000_000_000)
	if nano <= 0 {
		return 0, fmt.Errorf("must be at least 0.000000001 CPUs")
	}
	return nano, nil
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

func validateVolumes(v *ValidationError, prefix string, vols []Volume) {
	seen := map[string]bool{}
	for i, vol := range vols {
		field := fmt.Sprintf("%s.volumes[%d]", prefix, i)
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

func validatePorts(v *ValidationError, prefix string, ports []Port) {
	seen := map[string]bool{}
	for i, p := range ports {
		field := fmt.Sprintf("%s.ports[%d]", prefix, i)
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

func validateConfig(v *ValidationError, prefix string, fields []ConfigField) {
	seen := map[string]bool{}
	for i, f := range fields {
		field := fmt.Sprintf("%s.config[%d]", prefix, i)
		if !reConfigKey.MatchString(f.Key) {
			v.add(field+".key", "must match %s", reConfigKey)
		}
		if seen[f.Key] {
			v.add(field+".key", "duplicate key %q", f.Key)
		}
		seen[f.Key] = true
		switch f.Type {
		case "", ConfigTypeString, ConfigTypeNumber, ConfigTypeSelect, ConfigTypeBoolean, ConfigTypeGPU:
			// "" is accepted for old manifests and rendered as string.
		default:
			v.add(field+".type", "must be string, number, select, boolean, or gpu (got %q)", f.Type)
		}
		if f.Type == ConfigTypeSelect && len(f.Options) == 0 {
			v.add(field+".options", "is required when type is select")
		}
		if f.Type == ConfigTypeGPU {
			switch f.GPUVendor {
			case "", GPUVendorNVIDIA, GPUVendorAMD, GPUVendorIntel:
			default:
				v.add(field+".gpuVendor", "must be nvidia, amd, or intel when type is gpu (got %q)", f.GPUVendor)
			}
		} else if f.GPUVendor != "" {
			v.add(field+".gpuVendor", "is only valid when type is gpu")
		}
		for j, opt := range f.Options {
			if opt.Value == "" {
				v.add(fmt.Sprintf("%s.options[%d].value", field, j), "is required")
			}
		}
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

// EffectiveRestartPolicy returns the service's restart policy after
// defaulting.
func (s *Service) EffectiveRestartPolicy() string {
	if s.Container.RestartPolicy == "" {
		return RestartUnlessStopped
	}
	return s.Container.RestartPolicy
}

// EffectiveContainerRefs returns every OCI ref the service should track.
// artifact.image is the implicit primary runtime ref for oci-image services;
// containerRefs entries are auxiliary refs that can still trigger recreation
// when their tags move.
func (s *Service) EffectiveContainerRefs() []ContainerRef {
	if s == nil {
		return nil
	}
	refs := make([]ContainerRef, 0, len(s.ContainerRefs)+1)
	if s.Artifact.Type == ArtifactOCIImage {
		refs = append(refs, ContainerRef{
			Name:   "primary",
			Image:  s.Artifact.Image,
			Digest: s.Artifact.Digest,
		})
	}
	refs = append(refs, s.ContainerRefs...)
	return refs
}

// DistroSummary returns a single-line description of an lxc-distro
// artifact for display ("ubuntu/jammy/amd64"). Empty for oci-image.
func (s *Service) DistroSummary() string {
	if s.Artifact.Type != ArtifactLXCDistro {
		return ""
	}
	arch := s.Artifact.Arch
	if arch == "" {
		arch = "host"
	}
	return strings.Join([]string{s.Artifact.Distro, s.Artifact.Release, arch}, "/")
}
