package mdadm

import (
	"strings"
	"testing"
)

func TestValidateRAIDLevel(t *testing.T) {
	valid := []string{"raid0", "0", "raid1", "1", "raid4", "4", "raid5", "5", "raid6", "6", "raid10", "10", "linear"}
	for _, level := range valid {
		if err := ValidateRAIDLevel(level); err != nil {
			t.Errorf("expected valid RAID level %q, got error: %v", level, err)
		}
	}

	invalid := []string{"raid7", "stripe", "mirror", "", "2", "raid2"}
	for _, level := range invalid {
		if err := ValidateRAIDLevel(level); err == nil {
			t.Errorf("expected invalid RAID level %q to fail", level)
		}
	}
}

func TestValidateDiskPath(t *testing.T) {
	valid := []string{"/dev/sda", "/dev/sdb", "/dev/sdz", "/dev/sdaa", "/dev/nvme0n1", "/dev/nvme10n2"}
	for _, p := range valid {
		if err := ValidateDiskPath(p); err != nil {
			t.Errorf("expected valid disk path %q, got error: %v", p, err)
		}
	}

	invalid := []string{
		"/dev/sd",
		"/dev/sda1",         // partition
		"/dev/md0",          // array, not disk
		"/etc/passwd",
		"sda",               // no /dev/ prefix
		"/dev/sda; rm -rf /", // injection
		"/dev/nvme0n",       // incomplete
	}
	for _, p := range invalid {
		if err := ValidateDiskPath(p); err == nil {
			t.Errorf("expected invalid disk path %q to fail", p)
		}
	}
}

func TestValidateArrayPath(t *testing.T) {
	valid := []string{"/dev/md0", "/dev/md1", "/dev/md127"}
	for _, p := range valid {
		if err := ValidateArrayPath(p); err != nil {
			t.Errorf("expected valid array path %q, got error: %v", p, err)
		}
	}

	invalid := []string{"/dev/sda", "/dev/md", "/dev/md-1", "md0", "/dev/md0/something"}
	for _, p := range invalid {
		if err := ValidateArrayPath(p); err == nil {
			t.Errorf("expected invalid array path %q to fail", p)
		}
	}
}

func TestBuildCreateArgs(t *testing.T) {
	args, err := BuildCreateArgs("md0", "raid5", []string{"/dev/sda", "/dev/sdb", "/dev/sdc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"--create", "/dev/md0",
		"--level=raid5",
		"--raid-devices=3",
		"--metadata=1.2",
		"--homehost=smoothnas",
		"--name=md0",
		"--run",
		"/dev/sda", "/dev/sdb", "/dev/sdc",
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i := range args {
		if args[i] != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], args[i])
		}
	}
}

func TestBuildCreateArgsSingleDisk(t *testing.T) {
	args, err := BuildCreateArgs("md0", "raid1", []string{"/dev/sda"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasForce := false
	for _, a := range args {
		if a == "--force" {
			hasForce = true
			break
		}
	}
	if !hasForce {
		t.Errorf("expected --force for single-disk array, got args: %v", args)
	}

	// raid-devices should be 1
	found := false
	for _, a := range args {
		if a == "--raid-devices=1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --raid-devices=1, got args: %v", args)
	}
}

func TestBuildCreateArgsValidation(t *testing.T) {
	// Invalid RAID level.
	_, err := BuildCreateArgs("md0", "raid7", []string{"/dev/sda"})
	if err == nil {
		t.Error("expected error for invalid RAID level")
	}

	// Invalid disk path.
	_, err = BuildCreateArgs("md0", "raid1", []string{"/dev/sda", "/tmp/evil"})
	if err == nil {
		t.Error("expected error for invalid disk path")
	}
}

func TestParseDetailOutput(t *testing.T) {
	// Sample mdadm --detail output.
	output := `
/dev/md0:
           Version : 1.2
     Creation Time : Sat Mar 29 10:00:00 2026
        Raid Level : raid5
        Array Size : 209584128 (199.87 GiB 214.60 GB)
     Used Dev Size : 104792064 (99.94 GiB 107.30 GB)
      Raid Devices : 3
     Total Devices : 3
       Persistence : Superblock is persistent

       Update Time : Sat Mar 29 10:05:00 2026
             State : active
    Active Devices : 3
   Working Devices : 3
    Failed Devices : 0
     Spare Devices : 0

            Layout : left-symmetric
        Chunk Size : 512K

Consistency Policy : resync

              Name : smoothnas:md0
              UUID : abcd1234:ef567890:12345678:90abcdef
            Events : 42

    Number   Major   Minor   RaidDevice State
       0       8        0        0      active sync   /dev/sda
       1       8       16        1      active sync   /dev/sdb
       2       8       32        2      active sync   /dev/sdc
`

	a, err := ParseDetailOutput("/dev/md0", output)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if a.Name != "md0" {
		t.Errorf("expected name md0, got %s", a.Name)
	}
	if a.RAIDLevel != "raid5" {
		t.Errorf("expected raid5, got %s", a.RAIDLevel)
	}
	if a.State != "active" {
		t.Errorf("expected active, got %s", a.State)
	}
	if a.ActiveDisks != 3 {
		t.Errorf("expected 3 active disks, got %d", a.ActiveDisks)
	}
	if a.TotalDisks != 3 {
		t.Errorf("expected 3 total disks, got %d", a.TotalDisks)
	}
	if a.RebuildPct != -1 {
		t.Errorf("expected rebuild pct -1, got %f", a.RebuildPct)
	}
	// Size: 209584128 KB = ~200 GiB
	if a.Size != 209584128*1024 {
		t.Errorf("expected size %d, got %d", 209584128*1024, a.Size)
	}
	if len(a.MemberDisks) != 3 {
		t.Errorf("expected 3 member disks, got %d", len(a.MemberDisks))
	}
	if a.MemberDisks[0] != "/dev/sda" {
		t.Errorf("expected /dev/sda, got %s", a.MemberDisks[0])
	}
}

func TestParseDetailDegraded(t *testing.T) {
	output := `
/dev/md0:
        Raid Level : raid5
        Array Size : 209584128 (199.87 GiB 214.60 GB)
      Raid Devices : 3
     Total Devices : 2
             State : degraded
    Active Devices : 2
   Working Devices : 2
    Failed Devices : 1
     Spare Devices : 0
    Rebuild Status : 45% complete

    Number   Major   Minor   RaidDevice State
       0       8        0        0      active sync   /dev/sda
       -       0        0        1      removed
       2       8       32        2      active sync   /dev/sdc
`

	a, err := ParseDetailOutput("/dev/md0", output)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if a.State != "degraded" {
		t.Errorf("expected degraded, got %s", a.State)
	}
	if a.ActiveDisks != 2 {
		t.Errorf("expected 2 active, got %d", a.ActiveDisks)
	}
	if a.RebuildPct != 45 {
		t.Errorf("expected rebuild 45%%, got %f", a.RebuildPct)
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

func TestParseKV(t *testing.T) {
	tests := []struct {
		line     string
		key      string
		expected string
	}{
		{"        Raid Level : raid5", "Raid Level", "raid5"},
		{"    Active Devices : 3", "Active Devices", "3"},
		{"             State : active", "State", "active"},
		{"random line", "State", ""},
	}
	for _, tt := range tests {
		got := parseKV(tt.line, tt.key)
		if got != tt.expected {
			t.Errorf("parseKV(%q, %q) = %q, want %q", tt.line, tt.key, got, tt.expected)
		}
	}
}

// TestMemberDiskParsing verifies disk path extraction handles various formats.
func TestMemberDiskParsing(t *testing.T) {
	output := `
/dev/md0:
        Raid Level : raid1
      Raid Devices : 2
     Total Devices : 2
             State : active
    Active Devices : 2

    Number   Major   Minor   RaidDevice State
       0     259        1        0      active sync   /dev/nvme0n1
       1     259        2        1      active sync   /dev/nvme1n1
`

	a, err := ParseDetailOutput("/dev/md0", output)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(a.MemberDisks) != 2 {
		t.Fatalf("expected 2 member disks, got %d", len(a.MemberDisks))
	}
	if !strings.HasPrefix(a.MemberDisks[0], "/dev/nvme") {
		t.Errorf("expected nvme disk, got %s", a.MemberDisks[0])
	}
}
