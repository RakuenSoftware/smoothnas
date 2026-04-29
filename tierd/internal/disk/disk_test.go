package disk

import (
	"fmt"
	"testing"
)

func TestIsValidDiskPath(t *testing.T) {
	valid := []string{
		"/dev/sda",
		"/dev/sdb",
		"/dev/sdz",
		"/dev/sdaa",
		"/dev/nvme0n1",
		"/dev/nvme1n1",
		"/dev/nvme10n2",
	}
	for _, p := range valid {
		if !isValidDiskPath(p) {
			t.Errorf("expected valid: %s", p)
		}
	}

	invalid := []string{
		"/dev/sd",            // no letter after sd
		"/dev/sda1",          // partition, not disk
		"/dev/nvme0n",        // incomplete
		"/dev/nvmen1",        // no digits before n
		"/dev/md0",           // not a physical disk path
		"/etc/passwd",        // not a device
		"sda",                // no /dev/ prefix
		"/dev/sda; rm -rf /", // injection attempt
	}
	for _, p := range invalid {
		if isValidDiskPath(p) {
			t.Errorf("expected invalid: %s", p)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected uint64
	}{
		{"1000000000", 1000000000},
		{"0", 0},
		{"500107862016", 500107862016},
	}
	for _, tt := range tests {
		got := parseSize(tt.input)
		if got != tt.expected {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes    uint64
		expected string
	}{
		{500, "500B"},
		{1048576, "1.0M"},
		{1073741824, "1.0G"},
		{2000000000000, "1.8T"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.expected {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.expected)
		}
	}
}

func TestMergeAssignedZPoolMembersNormalizesByIDPaths(t *testing.T) {
	origEval := evalSymlinks
	evalSymlinks = func(path string) (string, error) {
		if path == "/dev/disk/by-id/nvme-test-part1" {
			return "/dev/nvme0n1p1", nil
		}
		return path, fmt.Errorf("unexpected path: %s", path)
	}
	defer func() { evalSymlinks = origEval }()

	assigned := make(map[string]string)
	mergeAssignedZPoolMembers(assigned, `
  pool: tank
 state: ONLINE
config:

	NAME                         STATE     READ WRITE CKSUM
	tank                         ONLINE       0     0     0
	  /dev/disk/by-id/nvme-test-part1 ONLINE 0     0     0
`)

	if got := assigned["/dev/nvme0n1"]; got != "zfs-pool" {
		t.Fatalf("expected /dev/nvme0n1 to be marked as zfs-pool, got %q", got)
	}
}

func TestMergeAssignedMDADMMembersUsesWholeDisks(t *testing.T) {
	assigned := make(map[string]string)
	mergeAssignedMDADMMembers(assigned, `
md0 : active raid1 nvme0n1p1[0] sda1[1]
      976630336 blocks super 1.2 [2/2] [UU]
`)

	if got := assigned["/dev/nvme0n1"]; got != "mdadm" {
		t.Fatalf("expected /dev/nvme0n1 to be marked as mdadm, got %q", got)
	}
	if got := assigned["/dev/sda"]; got != "mdadm" {
		t.Fatalf("expected /dev/sda to be marked as mdadm, got %q", got)
	}
}

func TestRequireUnassignedFromDisksRejectsAssignedDisk(t *testing.T) {
	origEval := evalSymlinks
	evalSymlinks = func(path string) (string, error) {
		if path == "/dev/disk/by-id/nvme-test-part1" {
			return "/dev/nvme0n1p1", nil
		}
		return path, nil
	}
	defer func() { evalSymlinks = origEval }()

	disks := []Disk{
		{Path: "/dev/nvme0n1", Assignment: "zfs-pool"},
		{Path: "/dev/sdb", Assignment: "unassigned"},
	}

	if err := requireUnassignedFromDisks(disks, []string{"/dev/disk/by-id/nvme-test-part1"}); err == nil {
		t.Fatal("expected assigned disk to be rejected")
	}
	if err := requireUnassignedFromDisks(disks, []string{"/dev/sdb"}); err != nil {
		t.Fatalf("expected unassigned disk to pass, got %v", err)
	}
}

func TestAssignmentFromFSTypeUsesChildMembers(t *testing.T) {
	dev := lsblkDevice{
		Name: "nvme0n1",
		Path: "/dev/nvme0n1",
		Type: "disk",
		Children: []lsblkDevice{
			{Name: "nvme0n1p1", Path: "/dev/nvme0n1p1", Type: "part", FSType: "linux_raid_member"},
		},
	}

	if got := assignmentFromFSType(dev); got != "mdadm" {
		t.Fatalf("assignmentFromFSType() = %q, want mdadm", got)
	}
}

func TestAssignmentForDevicePrefersLsblkSignatures(t *testing.T) {
	dev := lsblkDevice{
		Name: "sdb",
		Path: "/dev/sdb",
		Type: "disk",
		Children: []lsblkDevice{
			{Name: "sdb1", Path: "/dev/sdb1", Type: "part", FSType: "LVM2_member"},
		},
	}

	if got := assignmentForDevice(dev, "/dev/sdb", map[string]string{}); got != "lvm" {
		t.Fatalf("assignmentForDevice() = %q, want lvm", got)
	}
}

func TestBaseDiskPath(t *testing.T) {
	tests := map[string]string{
		"/dev/sda":        "/dev/sda",
		"/dev/sda1":       "/dev/sda",
		"/dev/sdaa12":     "/dev/sdaa",
		"/dev/nvme0n1":    "/dev/nvme0n1",
		"/dev/nvme0n1p1":  "/dev/nvme0n1",
		"/dev/nvme10n2p7": "/dev/nvme10n2",
	}
	for in, want := range tests {
		if got := BaseDiskPath(in); got != want {
			t.Fatalf("baseDiskPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNoMDSuperblock(t *testing.T) {
	ignored := []string{
		"mdadm: Unrecognised md component device - /dev/sdb",
		"mdadm: No md superblock detected on /dev/sdb",
		"mdadm: Couldn't open /dev/sdb for write - not zeroing",
		"mdadm: /dev/sdb is not an md array",
	}
	for _, msg := range ignored {
		if !isNoMDSuperblock(msg) {
			t.Fatalf("isNoMDSuperblock(%q) = false, want true", msg)
		}
	}
	if isNoMDSuperblock("mdadm: failed to write superblock") {
		t.Fatal("real mdadm write failures must not be ignored")
	}
}

func TestParsePowerState(t *testing.T) {
	tests := map[string]string{
		"drive state is:  active/idle": "active",
		"drive state is:  standby":     "standby",
		"drive state is:  sleeping":    "sleeping",
		"unexpected":                   "unknown",
	}
	for in, want := range tests {
		if got := ParsePowerState(in); got != want {
			t.Fatalf("ParsePowerState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStandbyTimerValue(t *testing.T) {
	tests := map[int]int{
		0:   0,
		1:   12,
		20:  240,
		21:  241,
		30:  241,
		330: 251,
	}
	for minutes, want := range tests {
		got, err := StandbyTimerValue(minutes)
		if err != nil {
			t.Fatalf("StandbyTimerValue(%d): %v", minutes, err)
		}
		if got != want {
			t.Fatalf("StandbyTimerValue(%d) = %d, want %d", minutes, got, want)
		}
	}
	if _, err := StandbyTimerValue(331); err == nil {
		t.Fatal("expected timer above hdparm maximum to fail")
	}
}

func TestPowerStatusEligibility(t *testing.T) {
	origRun := runPowerCommand
	runPowerCommand = func(string, ...string) ([]byte, error) {
		return []byte(" drive state is:  standby\n"), nil
	}
	t.Cleanup(func() { runPowerCommand = origRun })

	status := PowerStatusFor(Disk{Path: "/dev/sdb", Rotational: true, Assignment: "mdadm"}, 30)
	if !status.Eligible || status.State != "standby" || status.TimerMinutes != 30 {
		t.Fatalf("unexpected eligible status: %+v", status)
	}

	status = PowerStatusFor(Disk{Path: "/dev/nvme0n1", Rotational: false, Assignment: "unassigned"}, 0)
	if status.Eligible || status.IneligibleWhy == "" {
		t.Fatalf("expected non-rotational disk to be ineligible: %+v", status)
	}
}
