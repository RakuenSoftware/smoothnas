package compose

import (
	"fmt"
	"sort"
	"strings"

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

// RewriteTieredBinds rewrites a compose project so services mounting a tiered
// named volume instead bind-mount its resolved smoothfs host path (mechanism B),
// and drops those volumes from the top-level `volumes:` map. This is the form
// LXC2Docker honors: its `local` volume driver ignores driver_opts device= (a
// named volume always mounts at <volumeRoot>/<name>), whereas a service bind
// mount uses the host source path directly. binds maps volume name -> host path.
func RewriteTieredBinds(composeYAML []byte, binds map[string]string) ([]byte, error) {
	if len(binds) == 0 {
		return composeYAML, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse for bind rewrite: %w", err)
	}
	if services, ok := doc["services"].(map[string]any); ok {
		for _, sv := range services {
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			vols, ok := sm["volumes"].([]any)
			if !ok {
				continue
			}
			for i, v := range vols {
				switch m := v.(type) {
				case string:
					vols[i] = rewriteShortMount(m, binds)
				case map[string]any:
					rewriteLongMount(m, binds)
				}
			}
		}
	}
	// The tiered volumes are now binds; drop their top-level named-volume defs so
	// compose doesn't also create an (unused, wrong-location) managed volume.
	if topVols, ok := doc["volumes"].(map[string]any); ok {
		for name := range binds {
			delete(topVols, name)
		}
		if len(topVols) == 0 {
			delete(doc, "volumes")
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("compose: render rewritten project: %w", err)
	}
	return out, nil
}

// rewriteShortMount turns "vol:/target[:opts]" into "hostpath:/target[:opts]"
// when vol is a tiered volume; leaves other mounts untouched.
func rewriteShortMount(s string, binds map[string]string) string {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) < 2 {
		return s // no target -> not a named-volume mount we rewrite
	}
	if host, ok := binds[parts[0]]; ok {
		return host + ":" + parts[1]
	}
	return s
}

// rewriteLongMount converts a long-form {type: volume, source: vol, ...} mount to
// a bind on the resolved host path when source is a tiered volume.
func rewriteLongMount(m map[string]any, binds map[string]string) {
	src, _ := m["source"].(string)
	host, ok := binds[src]
	if !ok {
		return
	}
	m["type"] = "bind"
	m["source"] = host
}

// SetVolumeTiers overrides the x-smoothnas.tier of the named top-level volumes —
// the operator's install-time tier assignment. Volumes not listed, or without an
// x-smoothnas block, are untouched (a tier is never created implicitly). This
// lets a compose plugin ship a default tier and be remapped to the operator's
// pool at install without editing the file.
func SetVolumeTiers(composeYAML []byte, tiers map[string]string) ([]byte, error) {
	if len(tiers) == 0 {
		return composeYAML, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse for tier override: %w", err)
	}
	topVols, ok := doc["volumes"].(map[string]any)
	if !ok {
		return composeYAML, nil
	}
	changed := false
	for name, tier := range tiers {
		v, ok := topVols[name].(map[string]any)
		if !ok {
			continue
		}
		xs, ok := v["x-smoothnas"].(map[string]any)
		if !ok {
			continue
		}
		xs["tier"] = tier
		changed = true
	}
	if !changed {
		return composeYAML, nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("compose: render tier override: %w", err)
	}
	return out, nil
}

// SecretKeys returns the env keys a compose plugin declares as secrets via a
// top-level `x-smoothnas: {secrets: [KEY, ...]}` list. Their values are stored in
// the tierd secret store (not the compose file) and injected at `compose up`.
func SecretKeys(composeYAML []byte) ([]string, error) {
	var doc struct {
		XS *struct {
			Secrets []string `yaml:"secrets"`
		} `yaml:"x-smoothnas"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse secrets: %w", err)
	}
	if doc.XS == nil {
		return nil, nil
	}
	return doc.XS.Secrets, nil
}
