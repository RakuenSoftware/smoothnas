package api

import (
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

func recWithPin(image, pin string) *plugin.PluginRecord {
	man := "apiVersion: smoothnas.io/v1\n" +
		"kind: Plugin\n" +
		"metadata:\n  name: app\n  version: 0.0.1\n" +
		"services:\n  - name: app\n    artifact:\n      type: oci-image\n      image: " + image + "\n"
	rec := &plugin.PluginRecord{}
	rec.Plugin.Name = "app"
	rec.Plugin.ManifestYAML = man
	rec.Services = []plugin.ServiceRow{{Service: "app", PinnedImage: pin}}
	return rec
}

func TestPinShadowWarning(t *testing.T) {
	// A pin that differs from the manifest image → warning (the silent deploy trap).
	w := pinShadowWarning(recWithPin("ghcr.io/x/app:testing-new", "ghcr.io/x/app:testing-old"))
	if w == "" {
		t.Fatal("expected a warning when the pin shadows a different manifest image")
	}
	if !strings.Contains(w, "testing-new") || !strings.Contains(w, "testing-old") ||
		!strings.Contains(w, "/image") {
		t.Fatalf("warning should name both images and the fix route, got: %q", w)
	}

	// No pin → no warning (the manifest image deploys normally).
	if w := pinShadowWarning(recWithPin("ghcr.io/x/app:testing-new", "")); w != "" {
		t.Fatalf("expected no warning without a pin, got: %q", w)
	}

	// Pin equals the manifest image → no warning (pin is redundant, not shadowing).
	if w := pinShadowWarning(recWithPin("ghcr.io/x/app:v1", "ghcr.io/x/app:v1")); w != "" {
		t.Fatalf("expected no warning when pin == manifest image, got: %q", w)
	}

	// Unparseable manifest → best-effort empty (never panics).
	bad := &plugin.PluginRecord{}
	bad.Plugin.ManifestYAML = "not: [valid"
	bad.Services = []plugin.ServiceRow{{Service: "app", PinnedImage: "x"}}
	if w := pinShadowWarning(bad); w != "" {
		t.Fatalf("expected no warning on unparseable manifest, got: %q", w)
	}
}
