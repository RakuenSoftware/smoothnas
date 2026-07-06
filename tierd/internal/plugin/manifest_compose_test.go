package plugin

import "testing"

const composeWithMeta = `name: aimee-server
services:
  aimee-server:
    image: ghcr.io/rakuensoftware/aimee-server:latest
    ports: ["8740:8740", "8443:8443"]
volumes:
  home:
    x-smoothnas: { tier: HDD }
x-smoothnas:
  description: aimee agent/memory broker
  vendor: RakuenSoftware
  homepage: https://github.com/RakuenSoftware/aimee
  ui:
    service: aimee-server
    port: 8443
    path: /
`

func TestParseManifest_Compose_LiftsMetadata(t *testing.T) {
	m, err := ParseManifest([]byte(composeWithMeta))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if !m.IsCompose() {
		t.Fatal("expected IsCompose() true")
	}
	if m.Metadata.Name != "aimee-server" {
		t.Errorf("name = %q, want aimee-server", m.Metadata.Name)
	}
	if m.Metadata.Description != "aimee agent/memory broker" {
		t.Errorf("description = %q", m.Metadata.Description)
	}
	if m.Metadata.Vendor != "RakuenSoftware" {
		t.Errorf("vendor = %q", m.Metadata.Vendor)
	}
	if m.UI == nil || m.UI.Embed.Service != "aimee-server" || m.UI.Embed.Port != 8443 || m.UI.Embed.Path != "/" {
		t.Errorf("ui embed = %+v", m.UI)
	}
	// Version is blank at parse (stamped from the release tag by the catalog).
	if m.Metadata.Version != "" {
		t.Errorf("version = %q, want blank at parse", m.Metadata.Version)
	}
	// A compose manifest passes validation on a good name; native checks skipped.
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest(compose): %v", err)
	}
}

func TestParseManifest_Compose_HeadlessNoUI(t *testing.T) {
	y := `name: gh-runner
services:
  runner:
    image: ghcr.io/acme/runner:1
x-smoothnas:
  description: outbound-only, no UI
`
	m, err := ParseManifest([]byte(y))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.UI != nil {
		t.Errorf("headless plugin should have nil UI, got %+v", m.UI)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestParseManifest_Compose_RejectsBadUIService(t *testing.T) {
	y := `name: bad
services:
  web:
    image: nginx
x-smoothnas:
  ui: { service: does-not-exist, port: 80 }
`
	if _, err := ParseManifest([]byte(y)); err == nil {
		t.Fatal("expected error: ui.service names no compose service")
	}
}

func TestParseManifest_Compose_RejectsUIWithoutService(t *testing.T) {
	y := `name: bad
services:
  web:
    image: nginx
x-smoothnas:
  ui: { port: 80 }
`
	if _, err := ParseManifest([]byte(y)); err == nil {
		t.Fatal("expected error: ui.service required when ui present")
	}
}

// Native manifests must be unaffected by the compose branch.
func TestParseManifest_NativeStillWorks(t *testing.T) {
	y := `apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: demo
  version: 1.2.3
services:
  - name: demo
    artifact: { type: oci-image, image: nginx:1 }
`
	m, err := ParseManifest([]byte(y))
	if err != nil {
		t.Fatalf("ParseManifest(native): %v", err)
	}
	if m.IsCompose() {
		t.Error("native manifest wrongly marked compose")
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest(native): %v", err)
	}
}

func TestParseManifest_Compose_RejectsEmptyName(t *testing.T) {
	y := "services:\n  web:\n    image: nginx\n"
	if _, err := ParseManifest([]byte(y)); err == nil {
		t.Fatal("expected error: empty/missing compose name")
	}
}

func TestParseManifest_Compose_RejectsBadPort(t *testing.T) {
	for _, p := range []string{"0", "70000"} {
		y := "name: bad\nservices:\n  web:\n    image: nginx\nx-smoothnas:\n  ui: { service: web, port: " + p + " }\n"
		if _, err := ParseManifest([]byte(y)); err == nil {
			t.Fatalf("expected error for ui.port=%s", p)
		}
	}
}

func TestParseManifest_Compose_RejectsRelativeUIPath(t *testing.T) {
	y := "name: bad\nservices:\n  web:\n    image: nginx\nx-smoothnas:\n  ui: { service: web, port: 80, path: dashboard }\n"
	if _, err := ParseManifest([]byte(y)); err == nil {
		t.Fatal("expected error: ui.path must be absolute")
	}
}

func TestParseManifest_Compose_RejectsMultiDoc(t *testing.T) {
	y := "name: a\nservices:\n  web:\n    image: nginx\n---\nname: b\nservices:\n  x:\n    image: redis\n"
	if _, err := ParseManifest([]byte(y)); err == nil {
		t.Fatal("expected error: multi-document compose")
	}
}

func TestParseManifest_Compose_MultiServiceUIResolves(t *testing.T) {
	y := "name: aimee-combined\nservices:\n  server:\n    image: s\n  kb:\n    image: k\n  postgres:\n    image: p\nx-smoothnas:\n  ui: { service: server, port: 8443, path: / }\n"
	m, err := ParseManifest([]byte(y))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.UI == nil || m.UI.Embed.Service != "server" {
		t.Fatalf("ui embed = %+v", m.UI)
	}
}

func TestParseManifest_Compose_ExposesConfigSchema(t *testing.T) {
	y := `name: p
services:
  s: { image: x }
x-smoothnas:
  config:
    - { key: LLM_ENDPOINT, label: LLM endpoint, type: url }
    - { key: PW, label: password, secret: true }
`
	m, err := ParseManifest([]byte(y))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Config) != 2 {
		t.Fatalf("config len = %d, want 2", len(m.Config))
	}
	if m.Config[0].Key != "LLM_ENDPOINT" || m.Config[0].Type != "url" {
		t.Errorf("config[0] = %+v", m.Config[0])
	}
	if !m.Config[1].Secret {
		t.Errorf("config[1] should be secret: %+v", m.Config[1])
	}
}

func TestParseManifest_Compose_UIAuth(t *testing.T) {
	y := "name: p\nservices:\n  web: { image: x }\nx-smoothnas:\n  ui: { service: web, port: 80, path: /, auth: bearer-injected }\n"
	m, err := ParseManifest([]byte(y))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.UI == nil || m.UI.Embed.Auth != "bearer-injected" {
		t.Fatalf("ui auth not carried: %+v", m.UI)
	}
}
