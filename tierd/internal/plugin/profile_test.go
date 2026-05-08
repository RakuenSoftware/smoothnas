package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statHostFromMap fakes preflight.hostHas — paths in `present` are
// returned as os.Stat-success, everything else as os.IsNotExist.
func statHostFromMap(present map[string]bool) func(string) error {
	return func(p string) error {
		if present[p] {
			return nil
		}
		return os.ErrNotExist
	}
}

func TestNewCatalog_LoadsBuiltins(t *testing.T) {
	c, err := NewCatalog("")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, want := range []string{"default-limits", "gpu-amd", "gpu-intel", "gpu-nvidia"} {
		if _, ok := c.Get(want); !ok {
			t.Errorf("built-in %q missing from catalog", want)
		}
	}
	for _, p := range c.List() {
		if p.Source != "builtin" {
			t.Errorf("profile %q source = %q, want builtin", p.Metadata.Name, p.Source)
		}
	}
}

func TestNewCatalog_OperatorOverride(t *testing.T) {
	dir := t.TempDir()
	// Operator override of gpu-amd with a different cgroup mask.
	if err := os.WriteFile(filepath.Join(dir, "gpu-amd.yaml"), []byte(`apiVersion: smoothnas.io/v1
kind: PluginProfile
metadata:
  name: gpu-amd
  description: operator override
container:
  hostConfig:
    devices:
      - { path: /dev/dri, cgroupPermissions: rw }
`), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}
	c, err := NewCatalog(dir)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	got, ok := c.Get("gpu-amd")
	if !ok {
		t.Fatal("gpu-amd missing")
	}
	if got.Source != "operator" {
		t.Errorf("source = %q want operator", got.Source)
	}
	if len(got.Container.HostConfig.Devices) != 1 || got.Container.HostConfig.Devices[0].CgroupPermissions != "rw" {
		t.Errorf("operator override didn't take effect: %+v", got.Container.HostConfig.Devices)
	}
}

func TestNewCatalog_BadOperatorFileReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("not even YAML: {{{"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := NewCatalog(dir)
	if err == nil {
		t.Fatal("expected error reporting the bad file")
	}
	if c == nil {
		t.Fatal("catalog should be non-nil even with bad operator profiles")
	}
	// Built-ins still present.
	if _, ok := c.Get("gpu-amd"); !ok {
		t.Errorf("built-in lost after bad operator file: %v", err)
	}
}

func TestResolve_DefaultLimitsAutoInjected(t *testing.T) {
	c, _ := NewCatalog("")
	m := &Manifest{
		Profiles: []string{"gpu-amd"},
	}
	r, err := Resolve(c, m, statHostFromMap(map[string]bool{"/dev/dri": true}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(r.Names) != 2 || r.Names[0] != "default-limits" || r.Names[1] != "gpu-amd" {
		t.Errorf("Names = %v want [default-limits gpu-amd]", r.Names)
	}
	// default-limits set PidsLimit=1024.
	if r.PidsLimit != 1024 {
		t.Errorf("PidsLimit = %d want 1024", r.PidsLimit)
	}
}

func TestResolve_OptOutOfDefaultLimits(t *testing.T) {
	c, _ := NewCatalog("")
	m := &Manifest{
		Profiles: []string{"!default-limits", "gpu-amd"},
	}
	r, err := Resolve(c, m, statHostFromMap(map[string]bool{"/dev/dri": true}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(r.Names) != 1 || r.Names[0] != "gpu-amd" {
		t.Errorf("Names = %v want [gpu-amd]", r.Names)
	}
	if r.PidsLimit != 0 {
		t.Errorf("PidsLimit = %d want 0 (no default-limits)", r.PidsLimit)
	}
}

func TestResolve_ExplicitDefaultLimitsNotDuplicated(t *testing.T) {
	c, _ := NewCatalog("")
	m := &Manifest{
		Profiles: []string{"default-limits", "gpu-amd"},
	}
	r, err := Resolve(c, m, statHostFromMap(map[string]bool{"/dev/dri": true}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(r.Names) != 2 {
		t.Errorf("Names = %v want length 2 (no duplicate)", r.Names)
	}
}

func TestResolve_MissingProfileErrors(t *testing.T) {
	c, _ := NewCatalog("")
	m := &Manifest{Profiles: []string{"!default-limits", "ghost-profile"}}
	_, err := Resolve(c, m, nil)
	if err == nil || !strings.Contains(err.Error(), "ghost-profile") {
		t.Errorf("err = %v", err)
	}
}

func TestResolve_RequiredPreflightFailsHard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hard.yaml"), []byte(`apiVersion: smoothnas.io/v1
kind: PluginProfile
metadata:
  name: hard
preflight:
  hostHas:
    - { path: /dev/required-thing, requirement: required }
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := NewCatalog(dir)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	m := &Manifest{Profiles: []string{"!default-limits", "hard"}}
	_, err = Resolve(c, m, statHostFromMap(map[string]bool{}))
	if err == nil || !strings.Contains(err.Error(), "/dev/required-thing") {
		t.Errorf("err = %v", err)
	}
}

func TestResolve_OptionalPreflightWarnsAndContinues(t *testing.T) {
	c, _ := NewCatalog("")
	m := &Manifest{Profiles: []string{"!default-limits", "gpu-amd"}}
	// /dev/dri NOT present — gpu-amd's optional check should warn,
	// not block.
	r, err := Resolve(c, m, statHostFromMap(map[string]bool{}))
	if err != nil {
		t.Fatalf("resolve should not fail on optional preflight: %v", err)
	}
	if len(r.PreflightWarnings) == 0 {
		t.Error("expected at least one preflight warning")
	}
}

func TestResolve_DeviceConcatenation(t *testing.T) {
	// gpu-amd contributes 1 device, gpu-nvidia contributes 4. Merged
	// should have 5 (no dedup on device arrays).
	c, _ := NewCatalog("")
	m := &Manifest{Profiles: []string{"!default-limits", "gpu-amd", "gpu-nvidia"}}
	r, err := Resolve(c, m, statHostFromMap(map[string]bool{
		"/dev/dri":         true,
		"/dev/nvidiactl":   true,
		"/dev/nvidia-uvm":  true,
		"/dev/nvidia0":     true,
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(r.Devices) != 5 {
		t.Errorf("Devices count = %d want 5", len(r.Devices))
	}
}

func TestResolve_LXCRawDedup(t *testing.T) {
	c, _ := NewCatalog("")
	// Apply gpu-amd twice (would be a manifest bug, but the merger
	// should not produce duplicates anyway).
	m := &Manifest{Profiles: []string{"!default-limits", "gpu-amd", "gpu-amd"}}
	r, err := Resolve(c, m, statHostFromMap(map[string]bool{"/dev/dri": true}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	seen := map[string]int{}
	for _, l := range r.LXCRaw {
		seen[l]++
	}
	for line, count := range seen {
		if count > 1 {
			t.Errorf("LXC raw duplicate: %q × %d", line, count)
		}
	}
}

func TestParseProfile_StrictRejectsUnknownFields(t *testing.T) {
	_, err := ParseProfile([]byte(`apiVersion: smoothnas.io/v1
kind: PluginProfile
metadata:
  name: x
unknownField: nope
`))
	if err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func TestValidateProfile_DeviceCgroupPerms(t *testing.T) {
	cases := []struct {
		perms string
		ok    bool
	}{
		{"rwm", true},
		{"rw", true},
		{"", true},
		{"xrw", false},
		{"qqq", false},
	}
	for _, tc := range cases {
		p := &Profile{
			APIVersion: ProfileAPIVersion,
			Kind:       ProfileKind,
			Metadata:   ProfileMetadata{Name: "x"},
			Container: ProfileContainer{HostConfig: ProfileHostConfig{
				Devices: []ProfileDevice{{Path: "/dev/x", CgroupPermissions: tc.perms}},
			}},
		}
		err := ValidateProfile(p)
		if (err == nil) != tc.ok {
			t.Errorf("perms=%q: err=%v want ok=%v", tc.perms, err, tc.ok)
		}
	}
}

func TestPayload_ProfileFragmentsApplied(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	pidsLimit := int64(2048)
	oom := -100
	in.Profiles = &Resolved{
		Names: []string{"default-limits", "gpu-amd"},
		Devices: []ProfileDevice{
			{Path: "/dev/dri", CgroupPermissions: "rwm"},
		},
		Env:         map[string]string{"FROM_PROFILE": "yes"},
		PidsLimit:   pidsLimit,
		OomScoreAdj: &oom,
		LXCRaw: []string{
			"lxc.cgroup2.devices.allow = c 226:* rwm",
			"lxc.mount.entry = /dev/dri dev/dri none bind,optional 0 0",
		},
	}
	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.HostConfig.PidsLimit != pidsLimit {
		t.Errorf("PidsLimit = %d want %d", got.HostConfig.PidsLimit, pidsLimit)
	}
	if got.HostConfig.OomScoreAdj != -100 {
		t.Errorf("OomScoreAdj = %d", got.HostConfig.OomScoreAdj)
	}
	if len(got.HostConfig.Devices) != 1 || got.HostConfig.Devices[0].PathOnHost != "/dev/dri" {
		t.Errorf("Devices = %+v", got.HostConfig.Devices)
	}
	// Env contains both profile env and the manifest's MODEL_PATH;
	// keys are sorted.
	foundProfile := false
	for _, e := range got.Env {
		if e == "FROM_PROFILE=yes" {
			foundProfile = true
		}
	}
	if !foundProfile {
		t.Errorf("profile env missing: %v", got.Env)
	}
	// LXC raw labels indexed.
	if got.Labels["io.smoothnas.lxc.raw.0"] != "lxc.cgroup2.devices.allow = c 226:* rwm" {
		t.Errorf("lxc.raw.0 label = %q", got.Labels["io.smoothnas.lxc.raw.0"])
	}
	if got.Labels["io.smoothnas.lxc.raw.1"] != "lxc.mount.entry = /dev/dri dev/dri none bind,optional 0 0" {
		t.Errorf("lxc.raw.1 label = %q", got.Labels["io.smoothnas.lxc.raw.1"])
	}
}

func TestPayload_ManifestConfigOverridesProfileEnv(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Profiles = &Resolved{
		Env: map[string]string{
			"MODEL_PATH": "/profile-default",
		},
	}
	got, _ := BuildCreatePayload(in)
	// Manifest's MODEL_PATH default = /models/default.gguf — that
	// should win over the profile's value.
	for _, e := range got.Env {
		if e == "MODEL_PATH=/profile-default" {
			t.Errorf("profile env should not override manifest config: %v", got.Env)
		}
	}
}

func TestStore_SetResolvedProfilesPersists(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsForSingleInstance(m, "/tmp"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SetResolvedProfiles("llama-cpp", []string{"default-limits", "gpu-amd"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	rec, err := s.Get("llama-cpp")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(rec.Plugin.ResolvedProfiles) != 2 ||
		rec.Plugin.ResolvedProfiles[0] != "default-limits" ||
		rec.Plugin.ResolvedProfiles[1] != "gpu-amd" {
		t.Errorf("ResolvedProfiles = %v", rec.Plugin.ResolvedProfiles)
	}
}

func TestStore_SetResolvedProfiles_MissingPluginErrors(t *testing.T) {
	s := openTestStore(t)
	err := s.SetResolvedProfiles("ghost", []string{"x"})
	if !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("err = %v, want ErrPluginNotFound", err)
	}
}
