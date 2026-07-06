package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFixture is a small helper that reads a YAML fixture and returns
// the parsed manifest, failing the test on any error. Tests that need
// to mutate the fixture before validating get a fresh copy each call.
func loadFixture(t *testing.T, name string) *Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return m
}

func TestManifestJSONUsesYAMLFieldNames(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["metadata"]; !ok {
		t.Fatalf("json missing metadata key: %s", data)
	}
	if _, ok := got["Metadata"]; ok {
		t.Fatalf("json should not expose Go field name Metadata: %s", data)
	}
	if _, ok := got["services"]; !ok {
		t.Fatalf("json missing services key: %s", data)
	}
}

func TestParseManifest_Fixtures(t *testing.T) {
	cases := []struct {
		file         string
		wantArtifact string
		wantCount    int
		wantDistro   string
	}{
		{"llama.yaml", ArtifactOCIImage, 1, ""},
		{"gh-runner.yaml", ArtifactOCIImage, 2, ""},
		{"ubuntu-python.yaml", ArtifactLXCDistro, 1, "ubuntu/jammy/amd64"},
		{"wolf.yaml", ArtifactOCIImage, 1, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.file, func(t *testing.T) {
			m := loadFixture(t, tc.file)
			if len(m.Services) != 1 {
				t.Fatalf("expected 1 service, got %d", len(m.Services))
			}
			svc := &m.Services[0]
			if svc.Artifact.Type != tc.wantArtifact {
				t.Errorf("artifact.type = %q, want %q", svc.Artifact.Type, tc.wantArtifact)
			}
			if got := m.EffectiveCount(); got != tc.wantCount {
				t.Errorf("EffectiveCount = %d, want %d", got, tc.wantCount)
			}
			if got := svc.DistroSummary(); got != tc.wantDistro {
				t.Errorf("DistroSummary = %q, want %q", got, tc.wantDistro)
			}
			if err := ValidateManifest(m); err != nil {
				t.Errorf("ValidateManifest unexpectedly failed: %v", err)
			}
		})
	}
}

func TestParseManifest_StrictDecodingRejectsUnknownFields(t *testing.T) {
	yaml := []byte(`
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: foo
  version: 0.1.0
unknownField: oops
`)
	_, err := ParseManifest(yaml)
	if err == nil {
		t.Fatal("expected parse error on unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unknownField") {
		t.Errorf("error %q should mention the unknown field", err)
	}
}

// TestParseManifest_WrapsLegacyTopLevelShape proves backward compat: a
// pre-plugins-10 single-image manifest (top-level artifact/container/
// volumes/ports/config) parses and is auto-wrapped into one service named
// after the plugin, so already-installed and third-party plugins keep
// working after the schema change.
func TestParseManifest_WrapsLegacyTopLevelShape(t *testing.T) {
	yaml := []byte(`
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: foo
  version: 0.1.0
artifact:
  type: oci-image
  image: example/foo:latest
container:
  command: ["/run"]
  restartPolicy: unless-stopped
volumes:
  - name: data
    mode: flat
    bind: /data
ports:
  - name: http
    port: 8080
    protocol: tcp
    expose: true
config:
  - key: FOO
    type: string
    default: bar
`)
	m, err := ParseManifest(yaml)
	if err != nil {
		t.Fatalf("legacy manifest should parse, got: %v", err)
	}
	if len(m.Services) != 1 {
		t.Fatalf("legacy manifest should wrap into 1 service, got %d", len(m.Services))
	}
	svc := m.Services[0]
	if svc.Name != "foo" {
		t.Errorf("wrapped service name = %q, want the plugin name foo", svc.Name)
	}
	if svc.Artifact.Image != "example/foo:latest" {
		t.Errorf("artifact not carried into service: %+v", svc.Artifact)
	}
	if len(svc.Volumes) != 1 || len(svc.Ports) != 1 || len(svc.Config) != 1 {
		t.Errorf("volumes/ports/config not carried into service: %+v", svc)
	}
	if svc.Container.RestartPolicy != "unless-stopped" {
		t.Errorf("container not carried into service: %+v", svc.Container)
	}
	// The legacy fields are consumed; the JSON/runtime view is services-only.
	if m.LegacyArtifact != nil {
		t.Error("legacy artifact should be cleared after normalization")
	}
	if err := ValidateManifest(m); err != nil {
		t.Errorf("wrapped legacy manifest should validate: %v", err)
	}
}

// TestValidateManifest_RejectsMixedShape ensures a manifest can't set both
// the legacy top-level artifact and an explicit services: block.
func TestValidateManifest_RejectsMixedShape(t *testing.T) {
	yaml := []byte(`
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: foo
  version: 0.1.0
artifact:
  type: oci-image
  image: example/foo:latest
services:
  - name: foo
    artifact:
      type: oci-image
      image: example/foo:latest
`)
	m, err := ParseManifest(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m); err == nil {
		t.Fatal("expected validation error for mixed legacy + services shape")
	}
}

func TestValidateManifest_FailureModes(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*Manifest)
		wantField string
	}{
		{
			name:      "bad apiVersion",
			mutate:    func(m *Manifest) { m.APIVersion = "v2" },
			wantField: "apiVersion",
		},
		{
			name:      "bad kind",
			mutate:    func(m *Manifest) { m.Kind = "Pod" },
			wantField: "kind",
		},
		{
			name:      "bad metadata.name (uppercase)",
			mutate:    func(m *Manifest) { m.Metadata.Name = "BadName" },
			wantField: "metadata.name",
		},
		{
			name:      "bad metadata.version",
			mutate:    func(m *Manifest) { m.Metadata.Version = "not-semver" },
			wantField: "metadata.version",
		},
		{
			name:      "no services",
			mutate:    func(m *Manifest) { m.Services = nil },
			wantField: "services",
		},
		{
			name:      "bad service name (uppercase)",
			mutate:    func(m *Manifest) { m.Services[0].Name = "BadSvc" },
			wantField: "services[0].name",
		},
		{
			name:      "duplicate service name",
			mutate:    func(m *Manifest) { m.Services = append(m.Services, m.Services[0]) },
			wantField: "services[1].name",
		},
		{
			name:      "unknown artifact.type",
			mutate:    func(m *Manifest) { m.Services[0].Artifact.Type = "tarball" },
			wantField: "services[0].artifact.type",
		},
		{
			name:      "oci-image missing image",
			mutate:    func(m *Manifest) { m.Services[0].Artifact.Image = "" },
			wantField: "services[0].artifact.image",
		},
		{
			name:      "oci-image bad digest",
			mutate:    func(m *Manifest) { m.Services[0].Artifact.Digest = "deadbeef" },
			wantField: "services[0].artifact.digest",
		},
		{
			name: "oci-image with lxc-distro fields populated",
			mutate: func(m *Manifest) {
				m.Services[0].Artifact.Distro = "ubuntu"
				m.Services[0].Artifact.Release = "jammy"
			},
			wantField: "services[0].artifact",
		},
		{
			name:      "bad container.restartPolicy",
			mutate:    func(m *Manifest) { m.Services[0].Container.RestartPolicy = "always" },
			wantField: "services[0].container.restartPolicy",
		},
		{
			name:      "bad container.resources.memory",
			mutate:    func(m *Manifest) { m.Services[0].Container.Resources.Memory = "64XB" },
			wantField: "services[0].container.resources.memory",
		},
		{
			name:      "container.resources.memory references unknown config",
			mutate:    func(m *Manifest) { m.Services[0].Container.Resources.Memory = "${MEMORY_LIMIT}" },
			wantField: "services[0].container.resources.memory",
		},
		{
			name:      "bad container.resources.cpu",
			mutate:    func(m *Manifest) { m.Services[0].Container.Resources.CPU = "fast" },
			wantField: "services[0].container.resources.cpu",
		},
		{
			name:      "container.resources.cpu references unknown config",
			mutate:    func(m *Manifest) { m.Services[0].Container.Resources.CPU = "${CPU_LIMIT}" },
			wantField: "services[0].container.resources.cpu",
		},
		{
			name:      "instances.count negative",
			mutate:    func(m *Manifest) { m.Instances.Count = -3 },
			wantField: "instances.count",
		},
		{
			name:      "volume bind not absolute",
			mutate:    func(m *Manifest) { m.Services[0].Volumes[0].Bind = "models" },
			wantField: "services[0].volumes[0].bind",
		},
		{
			name: "duplicate volume name",
			mutate: func(m *Manifest) {
				m.Services[0].Volumes = append(m.Services[0].Volumes, m.Services[0].Volumes[0])
			},
			wantField: "services[0].volumes[1].name",
		},
		{
			name:      "port out of range",
			mutate:    func(m *Manifest) { m.Services[0].Ports[0].Port = 70000 },
			wantField: "services[0].ports[0].port",
		},
		{
			name:      "bad port protocol",
			mutate:    func(m *Manifest) { m.Services[0].Ports[0].Protocol = "sctp" },
			wantField: "services[0].ports[0].protocol",
		},
		{
			name:      "bad UI auth mode",
			mutate:    func(m *Manifest) { m.UI.Embed.Auth = "basic" },
			wantField: "ui.embed.auth",
		},
		{
			name:      "lowercase config key",
			mutate:    func(m *Manifest) { m.Services[0].Config[0].Key = "model_path" },
			wantField: "services[0].config[0].key",
		},
		{
			name:      "bad config type",
			mutate:    func(m *Manifest) { m.Services[0].Config[0].Type = "range" },
			wantField: "services[0].config[0].type",
		},
		{
			name: "bad gpu vendor",
			mutate: func(m *Manifest) {
				m.Services[0].Config[0].Type = ConfigTypeGPU
				m.Services[0].Config[0].GPUVendor = "matrox"
			},
			wantField: "services[0].config[0].gpuVendor",
		},
		{
			name: "gpu vendor on non-gpu field",
			mutate: func(m *Manifest) {
				m.Services[0].Config[0].Type = ConfigTypeString
				m.Services[0].Config[0].GPUVendor = GPUVendorNVIDIA
			},
			wantField: "services[0].config[0].gpuVendor",
		},
		{
			name:      "select config missing options",
			mutate:    func(m *Manifest) { m.Services[0].Config[0].Type = "select" },
			wantField: "services[0].config[0].options",
		},
		{
			name: "dependsOn unknown service",
			mutate: func(m *Manifest) {
				m.Services[0].DependsOn = map[string]DependsCondition{
					"nope": {Condition: DependsServiceStarted},
				}
			},
			wantField: "services[0].dependsOn.nope",
		},
		{
			name: "dependsOn self",
			mutate: func(m *Manifest) {
				m.Services[0].DependsOn = map[string]DependsCondition{
					"llama-cpp": {Condition: DependsServiceStarted},
				}
			},
			wantField: "services[0].dependsOn.llama-cpp",
		},
		{
			name: "dependsOn bad condition",
			mutate: func(m *Manifest) {
				m.Services = append(m.Services, Service{
					Name:     "sidecar",
					Artifact: Artifact{Type: ArtifactOCIImage, Image: "example/sidecar:latest"},
				})
				m.Services[0].DependsOn = map[string]DependsCondition{
					"sidecar": {Condition: "service_blessed"},
				}
			},
			wantField: "services[0].dependsOn.sidecar",
		},
		{
			name: "dependsOn service_healthy without health block",
			mutate: func(m *Manifest) {
				m.Services = append(m.Services, Service{
					Name:     "sidecar",
					Artifact: Artifact{Type: ArtifactOCIImage, Image: "example/sidecar:latest"},
				})
				m.Services[0].DependsOn = map[string]DependsCondition{
					"sidecar": {Condition: DependsServiceHealthy},
				}
			},
			wantField: "services[0].dependsOn.sidecar",
		},
		{
			name: "dependsOn cycle",
			mutate: func(m *Manifest) {
				m.Services = append(m.Services, Service{
					Name:      "sidecar",
					Artifact:  Artifact{Type: ArtifactOCIImage, Image: "example/sidecar:latest"},
					DependsOn: map[string]DependsCondition{"llama-cpp": {Condition: DependsServiceStarted}},
				})
				m.Services[0].DependsOn = map[string]DependsCondition{
					"sidecar": {Condition: DependsServiceStarted},
				}
			},
			wantField: "services",
		},
		{
			name: "host port collision across services",
			mutate: func(m *Manifest) {
				// Give llama's sole service a host-published port, then add
				// a second service that host-publishes the same port number.
				m.Services[0].Ports[0].HostExpose = true
				m.Services = append(m.Services, Service{
					Name:     "sidecar",
					Artifact: Artifact{Type: ArtifactOCIImage, Image: "example/sidecar:latest"},
					Ports:    []Port{{Name: "http", Port: 8080, Protocol: "tcp", HostExpose: true}},
				})
			},
			wantField: "services[1].ports",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := loadFixture(t, "llama.yaml")
			tc.mutate(m)
			err := ValidateManifest(m)
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", tc.name)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected *ValidationError, got %T", err)
			}
			found := false
			for _, iss := range ve.Issues {
				if iss.Field == tc.wantField {
					found = true
					break
				}
			}
			if !found {
				var fields []string
				for _, iss := range ve.Issues {
					fields = append(fields, iss.Field)
				}
				t.Errorf("expected issue on field %q, got fields %v", tc.wantField, fields)
			}
		})
	}
}

func TestValidateManifest_AllowsServiceDiscovery(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.Services = append(m.Services, Service{
		Name:     "db",
		Artifact: Artifact{Type: ArtifactOCIImage, Image: "pgvector/pgvector:pg16"},
		Health:   &Healthcheck{Test: []string{"CMD-SHELL", "pg_isready"}, Retries: 5},
	})
	m.Services[0].DependsOn = map[string]DependsCondition{
		"db": {Condition: DependsServiceHealthy},
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifest_SlotIsDeprecatedAndIgnored(t *testing.T) {
	// slot no longer pins a volume to an array: a tier-bound volume without
	// a slot is valid, and a leftover slot on either mode is accepted (parsed
	// but ignored) so existing/third-party manifests keep loading.
	m := loadFixture(t, "llama.yaml")
	m.Services[0].Volumes[0].Slot = ""
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("tier-bound without slot should be valid: %v", err)
	}

	m.Services[0].Volumes[0].Slot = "NVME"
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("tier-bound with leftover slot should be accepted: %v", err)
	}

	m.Services[0].Volumes[0].Mode = VolumeModeFlat
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("flat with leftover slot should be accepted: %v", err)
	}
}

func TestValidateManifest_AllowsGPUConfigField(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.Services[0].Config = append(m.Services[0].Config, ConfigField{
		Key:       "SMOOTHNAS_GPU",
		Type:      ConfigTypeGPU,
		Label:     "GPU",
		GPUVendor: GPUVendorNVIDIA,
	})

	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifest_AllowsConfigurableMemoryResource(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.Services[0].Config = append(m.Services[0].Config, ConfigField{
		Key:     "MEMORY_LIMIT",
		Type:    "string",
		Default: "64GiB",
	})
	m.Services[0].Container.Resources.Memory = "${MEMORY_LIMIT}"

	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifest_AllowsConfigurableCPUResource(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.Services[0].Config = append(m.Services[0].Config, ConfigField{
		Key:     "CPU_LIMIT",
		Type:    "number",
		Default: "1",
	})
	m.Services[0].Container.Resources.CPU = "${CPU_LIMIT}"

	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifest_LXCDistroMissingCommand(t *testing.T) {
	m := loadFixture(t, "ubuntu-python.yaml")
	m.Services[0].Container.Command = nil
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "container.command") {
		t.Errorf("expected container.command issue, got %v", err)
	}
}

func TestValidateManifest_CollectsMultipleIssues(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.APIVersion = "v2"
	m.Kind = "Pod"
	m.Metadata.Name = "Bad"
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(ve.Issues) < 3 {
		t.Errorf("expected ≥3 issues, got %d: %v", len(ve.Issues), ve.Issues)
	}
}

func TestValidateManifest_MultiInstanceWithoutPerInstanceWarns(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.Instances.Count = 3
	// The llama fixture's only volume is not perInstance, so this
	// should surface the "shared state" warning issue.
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("expected warning issue, got nil")
	}
	if !strings.Contains(err.Error(), "perInstance") {
		t.Errorf("expected perInstance warning, got %v", err)
	}
}
