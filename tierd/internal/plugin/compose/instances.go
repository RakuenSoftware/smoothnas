package compose

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// InstanceSpec is a scalable service declared via service-level x-smoothnas.
// Count is the current instance count (defaults to 1 when unset); Min/Max are
// optional bounds Scale validates the target against (0 = unbounded). Example:
//
//	services:
//	  gh-runner:
//	    x-smoothnas:
//	      instances: { count: 2, min: 1, max: 8 }
//	    volumes: ["work:/home/runner/_work"]
//	volumes:
//	  work:
//	    x-smoothnas: { tier: runner-ssd, perInstance: true }
type InstanceSpec struct {
	Service string
	Count   int
	Min     int
	Max     int
}

// ScalableServices returns the services declaring x-smoothnas.instances, sorted
// by service name. A compose plugin with none is an ordinary single-shot project.
func ScalableServices(composeYAML []byte) ([]InstanceSpec, error) {
	var doc struct {
		Services map[string]struct {
			XS *struct {
				Instances *struct {
					Count *int `yaml:"count"`
					Min   int  `yaml:"min"`
					Max   int  `yaml:"max"`
				} `yaml:"instances"`
			} `yaml:"x-smoothnas"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse instances: %w", err)
	}
	var out []InstanceSpec
	for name, svc := range doc.Services {
		if svc.XS == nil || svc.XS.Instances == nil {
			continue
		}
		in := svc.XS.Instances
		count := 1
		if in.Count != nil {
			count = *in.Count
		}
		out = append(out, InstanceSpec{Service: name, Count: count, Min: in.Min, Max: in.Max})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// perInstanceVolumes returns the set of top-level volume names marked
// x-smoothnas.perInstance: true.
func perInstanceVolumes(doc map[string]any) map[string]bool {
	out := map[string]bool{}
	vols, ok := doc["volumes"].(map[string]any)
	if !ok {
		return out
	}
	for name, v := range vols {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		xs, ok := m["x-smoothnas"].(map[string]any)
		if !ok {
			continue
		}
		if b, _ := xs["perInstance"].(bool); b {
			out[name] = true
		}
	}
	return out
}

// ExpandInstances rewrites each x-smoothnas.instances service into N discrete
// services `<svc>-1..N` (plugins-11 gh-runner). Each expanded service gets its
// OWN copy of the perInstance volumes its parent mounts, named `<svc>-<n>-<vol>`
// with the parent volume's x-smoothnas tier (minus the perInstance flag) — so
// concurrent instances never share a _work dir. `counts` overrides the declared
// count per service (operator scaling); a service absent from counts keeps its
// declared count. The x-smoothnas.instances block and the template perInstance
// volume are dropped from the output. A project with no scalable service is
// returned unchanged.
func ExpandInstances(composeYAML []byte, counts map[string]int) ([]byte, error) {
	specs, err := ScalableServices(composeYAML)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return composeYAML, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse for instance expansion: %w", err)
	}
	services, _ := doc["services"].(map[string]any)
	topVols, _ := doc["volumes"].(map[string]any)
	perInst := perInstanceVolumes(doc)
	if services == nil {
		return composeYAML, nil
	}

	usedTemplateVols := map[string]bool{}
	for _, spec := range specs {
		n := spec.Count
		if c, ok := counts[spec.Service]; ok {
			n = c
		}
		if n < 1 {
			n = 1
		}
		tmpl, ok := services[spec.Service].(map[string]any)
		if !ok {
			continue
		}
		delete(services, spec.Service)
		for i := 1; i <= n; i++ {
			inst, err := deepCopyMap(tmpl)
			if err != nil {
				return nil, err
			}
			dropInstancesExtension(inst)
			perVols := rewriteInstanceMounts(inst, spec.Service, i, perInst)
			for v := range perVols {
				usedTemplateVols[v] = true
				if topVols != nil {
					if def, ok := topVols[v]; ok {
						cp, err := deepCopyVolumeDef(def)
						if err != nil {
							return nil, err
						}
						topVols[fmt.Sprintf("%s-%d-%s", spec.Service, i, v)] = cp
					}
				}
			}
			services[fmt.Sprintf("%s-%d", spec.Service, i)] = inst
		}
	}
	// Drop the now-expanded template volumes from the top-level map.
	for v := range usedTemplateVols {
		delete(topVols, v)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("compose: render expanded project: %w", err)
	}
	return out, nil
}

// rewriteInstanceMounts rewrites the service's mounts of perInstance volumes to
// the per-instance name `<svc>-<i>-<vol>`, returning the set of template volume
// names it rewrote.
func rewriteInstanceMounts(svc map[string]any, service string, i int, perInst map[string]bool) map[string]bool {
	used := map[string]bool{}
	vols, ok := svc["volumes"].([]any)
	if !ok {
		return used
	}
	for idx, v := range vols {
		s, ok := v.(string)
		if !ok {
			continue
		}
		name, rest, hasTarget := cutColon(s)
		if !hasTarget || !perInst[name] {
			continue
		}
		used[name] = true
		vols[idx] = fmt.Sprintf("%s-%d-%s:%s", service, i, name, rest)
	}
	return used
}

func cutColon(s string) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func dropInstancesExtension(svc map[string]any) {
	xs, ok := svc["x-smoothnas"].(map[string]any)
	if !ok {
		return
	}
	delete(xs, "instances")
	if len(xs) == 0 {
		delete(svc, "x-smoothnas")
	}
}

func deepCopyMap(m map[string]any) (map[string]any, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func deepCopyVolumeDef(v any) (any, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	// Strip the perInstance flag from the materialised copy (it's now a concrete
	// per-instance volume, not a template).
	if m, ok := out.(map[string]any); ok {
		if xs, ok := m["x-smoothnas"].(map[string]any); ok {
			delete(xs, "perInstance")
		}
	}
	return out, nil
}
