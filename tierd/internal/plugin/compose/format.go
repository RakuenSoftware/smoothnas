package compose

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// IsComposeFormat reports whether raw YAML is a docker-compose project (a
// compose-native plugin) rather than a smoothnas.io/v1 Plugin manifest.
//
// A compose file has a top-level `services:` mapping and does NOT carry the
// smoothnas apiVersion; a manifest declares `apiVersion: smoothnas.io/v1`
// (and `kind: Plugin`). This lets install/lifecycle route a compose-format
// plugin to the compose Backend and a legacy manifest to BuildCreatePayload,
// during the migration where both coexist.
func IsComposeFormat(b []byte) bool {
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil || doc == nil {
		return false
	}
	if av, _ := doc["apiVersion"].(string); strings.HasPrefix(av, "smoothnas.io/") {
		return false
	}
	svc, ok := doc["services"].(map[string]any)
	return ok && len(svc) > 0
}

// ProjectName returns the compose top-level `name:` if set, else "". tierd
// falls back to the plugin's install name (the compose -p project name) when
// this is empty.
func ProjectName(b []byte) string {
	var doc struct {
		Name string `yaml:"name"`
	}
	_ = yaml.Unmarshal(b, &doc)
	return doc.Name
}

// SpecFromSingle builds a ProjectSpec for a single-file compose plugin (the
// common Phase-1 case: one compose.yaml). name is the tierd install name, used
// as the compose -p project name; env is the rendered operator config (-> .env).
func SpecFromSingle(name, composeYAML string, env map[string]string) ProjectSpec {
	return ProjectSpec{
		Name:      name,
		Files:     map[string]string{"compose.yaml": composeYAML},
		FileOrder: []string{"compose.yaml"},
		Env:       env,
	}
}
