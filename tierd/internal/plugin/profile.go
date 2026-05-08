package plugin

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultOperatorProfilesDir is where operator-supplied profile
// YAML files live. Files override built-ins by metadata.name.
const DefaultOperatorProfilesDir = "/etc/smoothnas/plugin-profiles.d"

// builtinProfiles is the embedded built-in catalog. Adding a YAML
// under profiles/builtin/ at compile time picks it up automatically.
//
//go:embed profiles/builtin/*.yaml
var builtinProfiles embed.FS

// ProfileAPIVersion + ProfileKind are the only manifest header
// values the catalog accepts.
const (
	ProfileAPIVersion = "smoothnas.io/v1"
	ProfileKind       = "PluginProfile"
)

// Profile is one declarative bundle of container-create policy that
// plugins opt into by name in their manifest.profiles list. Built-in
// profiles ship with tierd; operator profiles live in
// /etc/smoothnas/plugin-profiles.d/<name>.yaml.
type Profile struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   ProfileMetadata    `yaml:"metadata"`
	Container  ProfileContainer   `yaml:"container"`
	LXC        ProfileLXC         `yaml:"lxc"`
	Preflight  ProfilePreflight   `yaml:"preflight"`

	// Source is set by the catalog loader. "builtin" for embedded
	// profiles, "operator" for operator-supplied overrides. Used by
	// the read-only API and CLI.
	Source string `yaml:"-"`
}

// ProfileMetadata is the descriptive header.
type ProfileMetadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// ProfileContainer is the slice of the container-create payload a
// profile contributes to.
type ProfileContainer struct {
	HostConfig ProfileHostConfig `yaml:"hostConfig"`
	Env        map[string]string `yaml:"env"`
}

// ProfileHostConfig mirrors a strict subset of runtime.HostConfig.
// Adding a field here makes it usable from profiles; we keep the
// surface small on purpose so an operator-supplied profile cannot
// silently grant unexpected capabilities.
type ProfileHostConfig struct {
	Devices     []ProfileDevice `yaml:"devices,omitempty"`
	CapAdd      []string        `yaml:"capAdd,omitempty"`
	PidsLimit   int64           `yaml:"pidsLimit,omitempty"`
	OomScoreAdj *int            `yaml:"oomScoreAdj,omitempty"` // pointer so 0 is distinguishable from unset
}

// ProfileDevice is one DeviceMapping declaration. Maps directly to
// runtime.DeviceMapping at apply time.
type ProfileDevice struct {
	Path              string `yaml:"path"`
	CgroupPermissions string `yaml:"cgroupPermissions"`
}

// ProfileLXC carries LXC-specific raw-config directives that don't
// map to any Docker API field. tierd renders them as
// io.smoothnas.lxc.raw.<n>=<directive> labels on the container;
// LXC2Docker reads these labels and applies them to the underlying
// LXC config.
type ProfileLXC struct {
	RawConfig []string `yaml:"rawConfig,omitempty"`
}

// ProfilePreflight declares host-side preconditions checked at
// install time.
type ProfilePreflight struct {
	HostHas []ProfilePreflightCheck `yaml:"hostHas,omitempty"`
}

// ProfilePreflightCheck is one path existence requirement.
type ProfilePreflightCheck struct {
	Path        string `yaml:"path"`
	Requirement string `yaml:"requirement"` // "required" | "optional"
}

// Catalog is the in-memory profile catalog. Operator profiles
// override built-ins by metadata.name (operator wins).
type Catalog struct {
	profiles map[string]*Profile
}

// NewCatalog loads built-ins from the embedded FS and overlays
// operator profiles from operatorDir. Pass "" for operatorDir to
// load built-ins only (useful for tests; production passes
// DefaultOperatorProfilesDir).
//
// Returns the partial catalog plus a non-nil error if any profile
// fails to parse — bad operator profiles do NOT block tierd from
// starting; the failed profiles are logged + skipped + reported in
// the error so an operator can fix them.
func NewCatalog(operatorDir string) (*Catalog, error) {
	c := &Catalog{profiles: map[string]*Profile{}}
	var loadErrs []string

	if err := c.loadBuiltin(); err != nil {
		// Built-in failures are fatal — they're our own files and
		// breaking them is a build-time bug.
		return nil, fmt.Errorf("load built-in profiles: %w", err)
	}

	if operatorDir != "" {
		if errs := c.loadOperator(operatorDir); len(errs) > 0 {
			for _, e := range errs {
				loadErrs = append(loadErrs, e.Error())
			}
		}
	}

	if len(loadErrs) > 0 {
		return c, fmt.Errorf("operator profile errors: %s", strings.Join(loadErrs, "; "))
	}
	return c, nil
}

// Get returns the profile with the given name, or false. Operator
// profiles override built-ins by name.
func (c *Catalog) Get(name string) (*Profile, bool) {
	p, ok := c.profiles[name]
	return p, ok
}

// List returns every profile in the catalog, sorted by name.
func (c *Catalog) List() []*Profile {
	out := make([]*Profile, 0, len(c.profiles))
	for _, p := range c.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// Resolved is the merged output of applying default-limits + every
// profile in manifest.Profiles + the manifest's own overrides. The
// Lifecycle / payload renderer reads from this rather than from the
// raw manifest so profile fragments take effect.
type Resolved struct {
	Devices     []ProfileDevice
	CapAdd      []string
	PidsLimit   int64
	OomScoreAdj *int
	Env         map[string]string
	LXCRaw      []string

	// Names is the ordered list of profile names that were applied,
	// after default-limits injection and operator overrides. This
	// is what gets persisted to plugins.profiles_json.
	Names []string

	// PreflightWarnings collects messages for hostHas checks marked
	// optional that did not pass. Required failures are surfaced via
	// the returned error from Resolve.
	PreflightWarnings []string
}

// Resolve walks the profiles list per the proposal's merge order,
// runs preflight gates, and returns the merged Resolved. Returns an
// error when a referenced profile is not in the catalog or when a
// required preflight check fails.
//
// The default-limits profile is applied first unless the manifest
// explicitly opts out via "!default-limits". A missing default-limits
// in the catalog (very early bootstrap, tests) is not an error.
func Resolve(catalog *Catalog, m *Manifest, statHost func(path string) error) (*Resolved, error) {
	if m == nil {
		return nil, fmt.Errorf("Resolve: nil manifest")
	}
	if statHost == nil {
		statHost = osStatHost
	}

	names, optedOutDefault := orderProfiles(m.Profiles)
	if !optedOutDefault {
		// Inject default-limits at the front if the catalog has it.
		if _, ok := catalog.Get("default-limits"); ok {
			// Avoid duplicate injection if the manifest already
			// listed it explicitly.
			already := false
			for _, n := range names {
				if n == "default-limits" {
					already = true
					break
				}
			}
			if !already {
				names = append([]string{"default-limits"}, names...)
			}
		}
	}

	res := &Resolved{Env: map[string]string{}, Names: names}

	for _, name := range names {
		p, ok := catalog.Get(name)
		if !ok {
			return nil, fmt.Errorf("profile %q not in catalog", name)
		}
		if err := mergeProfile(res, p); err != nil {
			return nil, fmt.Errorf("merge profile %q: %w", name, err)
		}
		if err := runPreflight(res, p, statHost); err != nil {
			return nil, err
		}
	}

	return res, nil
}

// orderProfiles processes the manifest's profiles list, separating
// the "!default-limits" opt-out marker from real profile names.
// Preserves the operator's order of explicit entries.
func orderProfiles(declared []string) (names []string, optedOutDefault bool) {
	for _, n := range declared {
		if n == "!default-limits" {
			optedOutDefault = true
			continue
		}
		names = append(names, n)
	}
	return
}

// mergeProfile applies one profile's fragments onto the running
// Resolved. Per the proposal's merge rules:
//   - Scalars: replace (PidsLimit, OomScoreAdj)
//   - Maps: deep-merge (Env)
//   - Arrays of scalars: concatenate, deduplicate (CapAdd, LXCRaw)
//   - Arrays of objects: concatenate, no dedup (Devices)
func mergeProfile(into *Resolved, p *Profile) error {
	for k, v := range p.Container.Env {
		into.Env[k] = v // last writer wins on map keys
	}

	into.Devices = append(into.Devices, p.Container.HostConfig.Devices...)

	for _, c := range p.Container.HostConfig.CapAdd {
		if !containsString(into.CapAdd, c) {
			into.CapAdd = append(into.CapAdd, c)
		}
	}
	for _, r := range p.LXC.RawConfig {
		if !containsString(into.LXCRaw, r) {
			into.LXCRaw = append(into.LXCRaw, r)
		}
	}

	if p.Container.HostConfig.PidsLimit != 0 {
		into.PidsLimit = p.Container.HostConfig.PidsLimit
	}
	if p.Container.HostConfig.OomScoreAdj != nil {
		v := *p.Container.HostConfig.OomScoreAdj
		into.OomScoreAdj = &v
	}
	return nil
}

// runPreflight evaluates each hostHas check against statHost.
// Required failures bubble up as an error; optional failures are
// recorded in PreflightWarnings and the merge continues.
func runPreflight(into *Resolved, p *Profile, statHost func(string) error) error {
	for _, check := range p.Preflight.HostHas {
		if check.Path == "" {
			continue
		}
		if err := statHost(check.Path); err != nil {
			msg := fmt.Sprintf("profile %s: host path %s missing (%v)", p.Metadata.Name, check.Path, err)
			switch check.Requirement {
			case "required":
				return errors.New(msg)
			case "optional", "":
				into.PreflightWarnings = append(into.PreflightWarnings, msg)
			default:
				return fmt.Errorf("profile %s: unknown requirement %q", p.Metadata.Name, check.Requirement)
			}
		}
	}
	return nil
}

// osStatHost is the production statHost — just os.Stat plus
// translating "exists but unreadable" into "exists" so an
// underprivileged tierd doesn't false-fail GPU preflight on hosts
// where /dev/dri/* is mode 0660 owned by root:render.
func osStatHost(path string) error {
	_, err := os.Stat(path)
	return err
}

// loadBuiltin walks the embedded profiles/builtin/ tree.
func (c *Catalog) loadBuiltin() error {
	return fs.WalkDir(builtinProfiles, "profiles/builtin", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".yaml" {
			return nil
		}
		data, err := builtinProfiles.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		p, err := ParseProfile(data)
		if err != nil {
			return fmt.Errorf("parse embedded %s: %w", path, err)
		}
		p.Source = "builtin"
		c.profiles[p.Metadata.Name] = p
		return nil
	})
}

// loadOperator walks operatorDir for *.yaml files. A bad file is
// recorded in the returned error slice but does not block the
// rest of the directory.
func (c *Catalog) loadOperator(operatorDir string) []error {
	entries, err := os.ReadDir(operatorDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("read %s: %w", operatorDir, err)}
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		full := filepath.Join(operatorDir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", full, err))
			continue
		}
		p, err := ParseProfile(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", full, err))
			continue
		}
		p.Source = "operator"
		c.profiles[p.Metadata.Name] = p
	}
	return errs
}

// ParseProfile parses a profile YAML document. Strict decoding so
// typos in operator-supplied profiles surface as clear errors
// rather than silently being dropped.
func ParseProfile(data []byte) (*Profile, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}
	if err := ValidateProfile(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ValidateProfile runs field-level checks. Used by ParseProfile and
// by the CLI's validate verb.
func ValidateProfile(p *Profile) error {
	if p.APIVersion != ProfileAPIVersion {
		return fmt.Errorf("apiVersion must be %q (got %q)", ProfileAPIVersion, p.APIVersion)
	}
	if p.Kind != ProfileKind {
		return fmt.Errorf("kind must be %q (got %q)", ProfileKind, p.Kind)
	}
	if !reName.MatchString(p.Metadata.Name) {
		return fmt.Errorf("metadata.name must match %s", reName)
	}
	for i, d := range p.Container.HostConfig.Devices {
		if d.Path == "" || !strings.HasPrefix(d.Path, "/") {
			return fmt.Errorf("devices[%d].path must be an absolute path", i)
		}
		switch d.CgroupPermissions {
		case "", "r", "w", "m", "rw", "rm", "wm", "rwm":
		default:
			return fmt.Errorf("devices[%d].cgroupPermissions must be a subset of r/w/m (got %q)", i, d.CgroupPermissions)
		}
	}
	for i, c := range p.Preflight.HostHas {
		if c.Path == "" {
			return fmt.Errorf("preflight.hostHas[%d].path is required", i)
		}
		switch c.Requirement {
		case "", "required", "optional":
		default:
			return fmt.Errorf("preflight.hostHas[%d].requirement must be required/optional (got %q)", i, c.Requirement)
		}
	}
	return nil
}

// containsString is a tiny utility used by mergeProfile for dedup.
func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
