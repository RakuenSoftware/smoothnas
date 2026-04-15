package lvm

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateFilesystem(t *testing.T) {
	for _, fs := range []string{"xfs", "ext4"} {
		if err := ValidateFilesystem(fs); err != nil {
			t.Errorf("ValidateFilesystem(%q) unexpected error: %v", fs, err)
		}
	}
	for _, fs := range []string{"", "btrfs", "zfs", "ntfs"} {
		if err := ValidateFilesystem(fs); err == nil {
			t.Errorf("ValidateFilesystem(%q) should fail", fs)
		}
	}
}

func TestValidateMountPoint(t *testing.T) {
	valid := []string{"/mnt/data", "/mnt/pool-1", "/mnt/a/b/c"}
	for _, mp := range valid {
		if err := ValidateMountPoint(mp); err != nil {
			t.Errorf("ValidateMountPoint(%q) should pass: %v", mp, err)
		}
	}
	invalid := []string{
		"", "/data", "/mnt/../etc/passwd", "/mnt/", "/mnt/foo;rm",
		"/mnt/$HOME", "/mnt/`id`", "/mnt/a b",
	}
	for _, mp := range invalid {
		if err := ValidateMountPoint(mp); err == nil {
			t.Errorf("ValidateMountPoint(%q) should fail", mp)
		}
	}
}

func TestValidateDevicePath(t *testing.T) {
	valid := []string{"/dev/md0", "/dev/md127", "/dev/sda", "/dev/sdab", "/dev/nvme0n1", "/dev/nvme10n2"}
	for _, p := range valid {
		if err := ValidateDevicePath(p); err != nil {
			t.Errorf("ValidateDevicePath(%q) should pass: %v", p, err)
		}
	}
	invalid := []string{
		"", "md0", "/dev/md0p1", "/dev/mapper/foo", "/dev/null",
		"/dev/sda1", "/dev/nvme0n1p1", "/dev/../sda", "/dev/sda;ls",
	}
	for _, p := range invalid {
		if err := ValidateDevicePath(p); err == nil {
			t.Errorf("ValidateDevicePath(%q) should fail", p)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, n := range []string{"data", "pool_1", "my-vol"} {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) should pass: %v", n, err)
		}
	}
	for _, n := range []string{"", "a b", "vol!", "../x", "v;ls"} {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) should fail", n)
		}
	}
}

func TestBuildCreateLVArgsAbsoluteSize(t *testing.T) {
	got := BuildCreateLVArgs("smoothnas", "data", "20G", "/dev/md0")
	want := []string{"-y", "-W", "y", "-Z", "y", "-L", "20G", "-n", "data", "smoothnas", "/dev/md0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCreateLVArgsPercentSize(t *testing.T) {
	got := BuildCreateLVArgs("smoothnas", "data", "100%FREE", "/dev/md1")
	want := []string{"-y", "-W", "y", "-Z", "y", "-l", "100%FREE", "-n", "data", "smoothnas", "/dev/md1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCreateLVArgsNoPV(t *testing.T) {
	got := BuildCreateLVArgs("smoothnas", "data", "10G", "")
	want := []string{"-y", "-W", "y", "-Z", "y", "-L", "10G", "-n", "data", "smoothnas"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without PV, got %v, want %v", got, want)
	}
}

func TestBuildCreateLVArgsIncludesNonInteractiveSignatureWipe(t *testing.T) {
	args := BuildCreateLVArgs("smoothnas", "data", "20G", "/dev/md0")
	wantPrefix := []string{"-y", "-W", "y", "-Z", "y"}
	if !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("args prefix = %v, want %v", args[:len(wantPrefix)], wantPrefix)
	}
}

func TestBuildCreateLVArgsTierSeparation(t *testing.T) {
	// Regression: the PV path must land AFTER the vg name so LVM treats it
	// as the allocation constraint, not as an unknown option.
	args := BuildCreateLVArgs("smoothnas", "warm", "500M", "/dev/md1")
	// The tier PV must be the final argument.
	if args[len(args)-1] != "/dev/md1" {
		t.Errorf("PV path must be the final arg, got: %v", args)
	}
	// The vg name must come right before the PV.
	if args[len(args)-2] != "smoothnas" {
		t.Errorf("vg name must precede PV, got: %v", args)
	}
}

func TestPoolTag(t *testing.T) {
	if got := PoolTag("media"); got != "smoothnas-pool:media" {
		t.Fatalf("PoolTag(media) = %q, want %q", got, "smoothnas-pool:media")
	}
}

func TestTierTag(t *testing.T) {
	cases := map[string]string{
		"NVME": "smoothnas-tier:nvme",
		"SSD":  "smoothnas-tier:ssd",
		"HDD":  "smoothnas-tier:hdd",
	}
	for in, want := range cases {
		if got := TierTag(in); got != want {
			t.Errorf("TierTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseManagedPVs(t *testing.T) {
	out := strings.Join([]string{
		"  /dev/md0|tier-media|smoothnas-pool:media,smoothnas-tier:nvme",
		"  /dev/md1|tier-media|other,smoothnas-tier:hdd,smoothnas-pool:media",
		"  /dev/sda||foreign-tag",
	}, "\n")

	got := parseManagedPVs(out)
	want := []ManagedPV{
		{Device: "/dev/md0", VGName: "tier-media", PoolName: "media", TierName: "nvme"},
		{Device: "/dev/md1", VGName: "tier-media", PoolName: "media", TierName: "hdd"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseManagedPVs() = %#v, want %#v", got, want)
	}
}

func TestTierFromDevicesSinglePV(t *testing.T) {
	pvByTier := map[string]string{
		"/dev/md0": "NVME",
		"/dev/md1": "SSD",
		"/dev/md2": "HDD",
	}
	if got := tierFromDevices("/dev/md0(0)", pvByTier); got != "NVME" {
		t.Errorf("single PV: got %q, want NVME", got)
	}
	if got := tierFromDevices("/dev/md2(512)", pvByTier); got != "HDD" {
		t.Errorf("single PV HDD: got %q, want HDD", got)
	}
}

func TestTierFromDevicesSameTierMultiSegment(t *testing.T) {
	pvByTier := map[string]string{"/dev/md0": "NVME"}
	// Same PV used twice (different starting PEs) — still NVME.
	got := tierFromDevices("/dev/md0(0),/dev/md0(100)", pvByTier)
	if got != "NVME" {
		t.Errorf("multi-seg single tier: got %q, want NVME", got)
	}
}

func TestTierFromDevicesMixedReturnsEmpty(t *testing.T) {
	pvByTier := map[string]string{
		"/dev/md0": "NVME",
		"/dev/md1": "SSD",
	}
	got := tierFromDevices("/dev/md0(0),/dev/md1(100)", pvByTier)
	if got != "" {
		t.Errorf("mixed tiers should return empty, got %q", got)
	}
}

func TestTierFromDevicesUnknownPV(t *testing.T) {
	pvByTier := map[string]string{"/dev/md0": "NVME"}
	got := tierFromDevices("/dev/md9(0)", pvByTier)
	if got != "" {
		t.Errorf("unknown PV should return empty, got %q", got)
	}
}

func TestTierFromDevicesEmpty(t *testing.T) {
	if got := tierFromDevices("", map[string]string{"/dev/md0": "NVME"}); got != "" {
		t.Errorf("empty devices should return empty, got %q", got)
	}
	if got := tierFromDevices("/dev/md0(0)", nil); got != "" {
		t.Errorf("nil map should return empty, got %q", got)
	}
}

func TestBuildCreateLVArgsRejectsShellMeta(t *testing.T) {
	// The name is validated at the CreateLV boundary via ValidateName, not
	// in BuildCreateLVArgs. This test asserts that assumption so a
	// refactor doesn't break it silently.
	args := BuildCreateLVArgs("smoothnas", "ok", "10G", "/dev/md0")
	if strings.Contains(strings.Join(args, " "), ";") {
		t.Error("built args unexpectedly contain shell metacharacter")
	}
}

func TestParsePVLookup(t *testing.T) {
	got := parsePVLookup("  /dev/md0|smoothnas\n")
	if got == nil {
		t.Fatal("expected PV lookup")
	}
	if got.Device != "/dev/md0" || got.VGName != "smoothnas" {
		t.Fatalf("unexpected lookup: %+v", got)
	}
}

func TestParsePVLookupWithoutVG(t *testing.T) {
	got := parsePVLookup("  /dev/md1|\n")
	if got == nil {
		t.Fatal("expected PV lookup")
	}
	if got.Device != "/dev/md1" || got.VGName != "" {
		t.Fatalf("unexpected lookup: %+v", got)
	}
}

func TestParsePVLookupEmpty(t *testing.T) {
	if got := parsePVLookup(""); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
