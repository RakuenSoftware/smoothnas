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
			if m.Artifact.Type != tc.wantArtifact {
				t.Errorf("artifact.type = %q, want %q", m.Artifact.Type, tc.wantArtifact)
			}
			if got := m.EffectiveCount(); got != tc.wantCount {
				t.Errorf("EffectiveCount = %d, want %d", got, tc.wantCount)
			}
			if got := m.DistroSummary(); got != tc.wantDistro {
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

// mutateOCI returns a fresh, valid llama.yaml with a single targeted
// mutation applied. Keeps the table-driven failure cases below tight.
func mutateOCI(t *testing.T, mutate func(*Manifest)) *Manifest {
	t.Helper()
	m := loadFixture(t, "llama.yaml")
	mutate(m)
	return m
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
			name:      "unknown artifact.type",
			mutate:    func(m *Manifest) { m.Artifact.Type = "tarball" },
			wantField: "artifact.type",
		},
		{
			name:      "oci-image missing image",
			mutate:    func(m *Manifest) { m.Artifact.Image = "" },
			wantField: "artifact.image",
		},
		{
			name:      "oci-image bad digest",
			mutate:    func(m *Manifest) { m.Artifact.Digest = "deadbeef" },
			wantField: "artifact.digest",
		},
		{
			name: "oci-image with lxc-distro fields populated",
			mutate: func(m *Manifest) {
				m.Artifact.Distro = "ubuntu"
				m.Artifact.Release = "jammy"
			},
			wantField: "artifact",
		},
		{
			name:      "bad container.restartPolicy",
			mutate:    func(m *Manifest) { m.Container.RestartPolicy = "always" },
			wantField: "container.restartPolicy",
		},
		{
			name:      "bad container.resources.memory",
			mutate:    func(m *Manifest) { m.Container.Resources.Memory = "64XB" },
			wantField: "container.resources.memory",
		},
		{
			name:      "container.resources.memory references unknown config",
			mutate:    func(m *Manifest) { m.Container.Resources.Memory = "${MEMORY_LIMIT}" },
			wantField: "container.resources.memory",
		},
		{
			name:      "instances.count negative",
			mutate:    func(m *Manifest) { m.Instances.Count = -3 },
			wantField: "instances.count",
		},
		{
			name:      "volume bind not absolute",
			mutate:    func(m *Manifest) { m.Volumes[0].Bind = "models" },
			wantField: "volumes[0].bind",
		},
		{
			name:      "tier-bound volume missing slot",
			mutate:    func(m *Manifest) { m.Volumes[0].Slot = "" },
			wantField: "volumes[0].slot",
		},
		{
			name: "flat volume with slot set",
			mutate: func(m *Manifest) {
				m.Volumes[0].Mode = VolumeModeFlat
				// Slot left non-empty from the fixture.
			},
			wantField: "volumes[0].slot",
		},
		{
			name:      "duplicate volume name",
			mutate:    func(m *Manifest) { m.Volumes = append(m.Volumes, m.Volumes[0]) },
			wantField: "volumes[1].name",
		},
		{
			name:      "port out of range",
			mutate:    func(m *Manifest) { m.Ports[0].Port = 70000 },
			wantField: "ports[0].port",
		},
		{
			name:      "bad port protocol",
			mutate:    func(m *Manifest) { m.Ports[0].Protocol = "sctp" },
			wantField: "ports[0].protocol",
		},
		{
			name:      "bad UI auth mode",
			mutate:    func(m *Manifest) { m.UI.Embed.Auth = "basic" },
			wantField: "ui.embed.auth",
		},
		{
			name:      "lowercase config key",
			mutate:    func(m *Manifest) { m.Config[0].Key = "model_path" },
			wantField: "config[0].key",
		},
		{
			name:      "bad config type",
			mutate:    func(m *Manifest) { m.Config[0].Type = "range" },
			wantField: "config[0].type",
		},
		{
			name:      "select config missing options",
			mutate:    func(m *Manifest) { m.Config[0].Type = "select" },
			wantField: "config[0].options",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := mutateOCI(t, tc.mutate)
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

func TestValidateManifest_AllowsConfigurableMemoryResource(t *testing.T) {
	m := loadFixture(t, "llama.yaml")
	m.Config = append(m.Config, ConfigField{
		Key:     "MEMORY_LIMIT",
		Type:    "string",
		Default: "64GiB",
	})
	m.Container.Resources.Memory = "${MEMORY_LIMIT}"

	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifest_LXCDistroMissingCommand(t *testing.T) {
	m := loadFixture(t, "ubuntu-python.yaml")
	m.Container.Command = nil
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
