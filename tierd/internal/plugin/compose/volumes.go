package compose

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// TieredVolume is a compose top-level named volume annotated for smoothfs tier
// placement via the x-smoothnas extension (plugins-11 Phase 2):
//
//	volumes:
//	  data:
//	    x-smoothnas:
//	      tier: fast-pool     # smoothfs pool/slot selector
//	      minSize: 10G        # optional preflight minimum
type TieredVolume struct {
	Name    string
	Tier    string
	MinSize string
}

// TieredVolumes parses the x-smoothnas-annotated named volumes from a compose
// project. Volumes without the annotation are ordinary compose volumes (not
// tier-managed) and are omitted. Deterministic order (by name).
func TieredVolumes(composeYAML []byte) ([]TieredVolume, error) {
	var doc struct {
		Volumes map[string]struct {
			XSmoothNAS *struct {
				Tier    string `yaml:"tier"`
				MinSize string `yaml:"minSize"`
			} `yaml:"x-smoothnas"`
		} `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse volumes: %w", err)
	}
	var out []TieredVolume
	for name, v := range doc.Volumes {
		if v.XSmoothNAS == nil || v.XSmoothNAS.Tier == "" {
			continue
		}
		out = append(out, TieredVolume{Name: name, Tier: v.XSmoothNAS.Tier, MinSize: v.XSmoothNAS.MinSize})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BindOverride renders a compose OVERRIDE file that redefines each named volume
// as a local bind to its resolved smoothfs host path (mechanism A). Layered as
// `-f base -f override`, it leaves the plugin's base compose untouched. The
// `com.smoothnas.tiered` label marks the volume so uninstall can refuse `-v`
// (tiered data is tierd-owned, not deleted by compose down). binds maps volume
// name -> resolved host path.
func BindOverride(binds map[string]string) (string, error) {
	if len(binds) == 0 {
		return "", nil
	}
	type driverOpts struct {
		Type   string `yaml:"type"`
		O      string `yaml:"o"`
		Device string `yaml:"device"`
	}
	type vol struct {
		Driver     string            `yaml:"driver"`
		DriverOpts driverOpts        `yaml:"driver_opts"`
		Labels     map[string]string `yaml:"labels"`
	}
	doc := struct {
		Volumes map[string]vol `yaml:"volumes"`
	}{Volumes: map[string]vol{}}
	for name, host := range binds {
		doc.Volumes[name] = vol{
			Driver:     "local",
			DriverOpts: driverOpts{Type: "none", O: "bind", Device: host},
			Labels:     map[string]string{"com.smoothnas.tiered": "true"},
		}
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("compose: render bind override: %w", err)
	}
	return string(b), nil
}
