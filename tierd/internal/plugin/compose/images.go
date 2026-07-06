package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServiceImages parses services.*.image from a compose project, returning a
// service -> image map. Services with no `image:` (build-only) are omitted.
//
// The raw image string is preserved verbatim, including any ${VAR} substitution
// — a mutable tag (no @sha256 digest) still reads as "updatable" for the update
// button, and `compose pull` resolves the substitution at apply time. This is
// what lets a compose plugin participate in the same image-update tracking
// (plugin_services + plugin_container_refs) that manifest plugins get.
func ServiceImages(composeYAML []byte) (map[string]string, error) {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse images: %w", err)
	}
	out := make(map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		if img := strings.TrimSpace(svc.Image); img != "" {
			out[name] = img
		}
	}
	return out, nil
}
