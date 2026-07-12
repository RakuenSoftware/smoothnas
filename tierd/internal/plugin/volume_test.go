package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// fakeTierProvider is an in-memory TierProvider used by preflight
// tests. Tests register tiers and pick the failure modes they want to
// exercise.
type fakeTierProvider struct {
	tiers map[string]*db.TierInstance
}

func newFakeTP() *fakeTierProvider {
	return &fakeTierProvider{
		tiers: map[string]*db.TierInstance{},
	}
}

// put registers a tier. Any trailing slot names are accepted but ignored —
// slot is deprecated and no longer influences placement or preflight.
func (f *fakeTierProvider) put(name, mountpoint, state string, _ ...string) {
	f.tiers[name] = &db.TierInstance{Name: name, MountPoint: mountpoint, State: state}
}

// putTemp registers a healthy tier whose mount point is a real, writable temp
// dir. Use it whenever the code under test provisions tier-bound directories
// under the mount (compose Materialise mkdirs each bind source), which a fake
// "/mnt/…" path can't satisfy. Returns the mount path for assertions.
func (f *fakeTierProvider) putTemp(t *testing.T, name string) string {
	t.Helper()
	mp := t.TempDir()
	f.put(name, mp, "healthy")
	return mp
}

func (f *fakeTierProvider) GetTierInstance(name string) (*db.TierInstance, error) {
	if t, ok := f.tiers[name]; ok {
		return t, nil
	}
	return nil, db.ErrNotFound
}

// fakeStatfs returns whatever the test sets for "available bytes".
type fakeStatfs map[string]uint64

func (s fakeStatfs) avail(path string) (uint64, error) {
	if v, ok := s[path]; ok {
		return v, nil
	}
	return 1 << 50, nil // huge default so free-space gate passes silently
}

func TestPreflight_HappyPath_TierBound(t *testing.T) {
	tp := newFakeTP()
	tp.put("media", "/mnt/media", db.TierPoolStateHealthy, "NVME", "SSD", "HDD")
	m := mustParse(t, "llama.yaml") // tier-bound NVME volume

	res, err := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "media"}, "/var/lib/smoothnas/plugins")
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !res.OK {
		t.Errorf("expected ok, got placements: %+v", res.Placements)
	}
	if len(res.Placements) != 1 {
		t.Fatalf("placements = %d want 1", len(res.Placements))
	}
	p := res.Placements[0]
	if p.Pool != "media" {
		t.Errorf("pool = %q want media", p.Pool)
	}
	wantPath := "/mnt/media/.plugins/llama-cpp/models"
	if p.HostPath != wantPath {
		t.Errorf("host_path = %q want %q", p.HostPath, wantPath)
	}
}

func TestPreflight_FlatVolumeBypassesGates(t *testing.T) {
	tp := newFakeTP()                       // no tiers registered
	m := mustParse(t, "ubuntu-python.yaml") // flat volume only
	res, err := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{}, "/var/lib/smoothnas/plugins")
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !res.OK {
		t.Errorf("flat volumes should pass without tier registration: %+v", res.Placements)
	}
	if got := res.Placements[0].HostPath; got != "/var/lib/smoothnas/plugins/ubuntu-python/data" {
		t.Errorf("flat path = %q", got)
	}
}

func TestPreflight_NoTierAssignmentBlocks(t *testing.T) {
	tp := newFakeTP()
	tp.put("media", "/mnt/media", db.TierPoolStateHealthy, "NVME")
	m := mustParse(t, "llama.yaml")

	res, err := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{}, "/x") // no Default, no PerVolume
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if res.OK {
		t.Fatal("expected preflight failure")
	}
	if !strings.Contains(res.Placements[0].Errors[0], "no tier assignment") {
		t.Errorf("error = %q", res.Placements[0].Errors[0])
	}
}

func TestPreflight_UnknownTierBlocks(t *testing.T) {
	tp := newFakeTP() // no tiers registered
	m := mustParse(t, "llama.yaml")
	res, _ := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "ghost"}, "/x")
	if res.OK {
		t.Fatal("expected failure for missing tier")
	}
	if !strings.Contains(res.Placements[0].Errors[0], "does not exist") {
		t.Errorf("error = %q", res.Placements[0].Errors[0])
	}
}

func TestPreflight_UnreadyTierBlocks(t *testing.T) {
	tp := newFakeTP()
	tp.put("media", "/mnt/media", db.TierPoolStateProvisioning, "NVME")
	m := mustParse(t, "llama.yaml")
	res, _ := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "media"}, "/x")
	if res.OK {
		t.Fatal("expected failure for provisioning tier")
	}
	found := false
	for _, e := range res.Placements[0].Errors {
		if strings.Contains(e, "provisioning") || strings.Contains(e, "healthy or degraded") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected state error, got: %v", res.Placements[0].Errors)
	}
}

func TestPreflight_DegradedTierAllowed(t *testing.T) {
	tp := newFakeTP()
	tp.put("media", "/mnt/media", db.TierPoolStateDegraded, "NVME")
	m := mustParse(t, "llama.yaml")
	res, _ := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "media"}, "/x")
	if !res.OK {
		t.Errorf("degraded tier should be writable: %+v", res.Placements)
	}
}

func TestPreflight_FreeSpaceWarnsButDoesNotBlock(t *testing.T) {
	tp := newFakeTP()
	tp.put("media", "/mnt/media", db.TierPoolStateHealthy, "NVME")
	m := mustParse(t, "llama.yaml")             // declares minSize 50G
	tinyFs := fakeStatfs{"/mnt/media": 1 << 20} // 1 MB available

	res, _ := PreflightTierAssignments(tp, tinyFs.avail, m,
		TierAssignments{Default: "media"}, "/x")
	if !res.OK {
		t.Errorf("free-space gate must be warn-only, got blocking errors: %+v", res.Placements[0].Errors)
	}
	if len(res.Placements[0].Warnings) == 0 {
		t.Errorf("expected free-space warning")
	}
}

func TestPreflight_PerVolumeOverridesDefault(t *testing.T) {
	tp := newFakeTP()
	tp.put("nvme-pool", "/mnt/nvme", db.TierPoolStateHealthy, "NVME")
	tp.put("ssd-pool", "/mnt/ssd", db.TierPoolStateHealthy, "NVME")
	m := mustParse(t, "llama.yaml")

	res, _ := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "ssd-pool", PerVolume: map[string]string{"models": "nvme-pool"}}, "/x")
	if !res.OK {
		t.Fatalf("not ok: %+v", res.Placements)
	}
	if res.Placements[0].Pool != "nvme-pool" {
		t.Errorf("PerVolume should win over Default; got %q", res.Placements[0].Pool)
	}
}

func TestPreflight_LeftoverSlotIgnored(t *testing.T) {
	tp := newFakeTP()
	// A tier with no matching slot at all. The manifest carries a leftover
	// slot that isn't NVME/SSD/HDD — slot is deprecated, so preflight ignores
	// it and the install proceeds against the tier as a whole.
	tp.put("media", "/mnt/media", db.TierPoolStateHealthy)

	yaml := []byte(`apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: weird
  version: 0.0.1
services:
  - name: weird
    artifact:
      type: oci-image
      image: example.com/x:1
    container:
      command: ["sleep","1"]
      restartPolicy: unless-stopped
    volumes:
      - name: data
        mode: tier-bound
        slot: NONEXISTENT
        bind: /data
`)
	m, err := ParseManifest(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	res, _ := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "media"}, "/x")
	if !res.OK {
		t.Fatalf("leftover slot should be ignored, got errors: %+v", res.Placements[0].Errors)
	}
}

func TestPreflight_TierBoundNeedsNoSlot(t *testing.T) {
	tp := newFakeTP()
	// A healthy tier is all a tier-bound volume needs — there is no slot to
	// validate anymore.
	tp.put("media", "/mnt/media", db.TierPoolStateHealthy)
	m := mustParse(t, "llama.yaml")
	res, _ := PreflightTierAssignments(tp, fakeStatfs{}.avail, m,
		TierAssignments{Default: "media"}, "/x")
	if !res.OK {
		t.Errorf("tier-bound volume should resolve against a healthy tier: %+v", res.Placements[0].Errors)
	}
}

func TestPreflightError_AsErrorListsVolumes(t *testing.T) {
	res := &PreflightResult{
		Placements: []VolumePlacement{
			{Volume: "models", Errors: []string{"tier missing"}},
			{Volume: "scratch", Errors: []string{"slot missing"}},
		},
	}
	pe := &PreflightError{Result: res}
	msg := pe.Error()
	if !strings.Contains(msg, "models: tier missing") || !strings.Contains(msg, "scratch: slot missing") {
		t.Errorf("error message missing volume detail: %q", msg)
	}
	var unwrap *PreflightError
	if !errors.As(pe, &unwrap) {
		t.Error("errors.As(*PreflightError) should succeed")
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in     string
		want   uint64
		wantOK bool
	}{
		{"50G", 50 << 30, true},
		{"100M", 100 << 20, true},
		{"2T", 2 << 40, true},
		{"512K", 512 << 10, true},
		{"1024", 1024, true},
		{"", 0, false},
		{"abc", 0, false},
		{"5x", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSize(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseSize(%q) = (%d, %v) want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
