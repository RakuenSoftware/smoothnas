package zfs

import (
	"strings"
	"testing"
)

// --- Pool validation tests ---

func TestValidatePoolName(t *testing.T) {
	valid := []string{"tank", "pool0", "my-pool", "storage_1", "Pool"}
	for _, name := range valid {
		if err := ValidatePoolName(name); err != nil {
			t.Errorf("expected valid pool name %q, got error: %v", name, err)
		}
	}

	invalid := []string{
		"",                             // empty
		"0pool",                        // starts with digit
		"-pool",                        // starts with hyphen
		"pool name",                    // space
		"pool;rm",                      // semicolon
		"import",                       // reserved route name
		"a" + string(make([]byte, 65)), // too long (>64)
	}
	for _, name := range invalid {
		if err := ValidatePoolName(name); err == nil {
			t.Errorf("expected invalid pool name %q to fail", name)
		}
	}
}

func TestValidateVdevType(t *testing.T) {
	valid := []string{"mirror", "raidz", "raidz1", "raidz2", "raidz3", "draid1", "draid2", "draid3"}
	for _, vt := range valid {
		if err := ValidateVdevType(vt); err != nil {
			t.Errorf("expected valid vdev type %q, got error: %v", vt, err)
		}
	}

	invalid := []string{"raid5", "stripe", "", "raid1", "single"}
	for _, vt := range invalid {
		if err := ValidateVdevType(vt); err == nil {
			t.Errorf("expected invalid vdev type %q to fail", vt)
		}
	}
}

func TestValidateDiskPath(t *testing.T) {
	valid := []string{"/dev/sda", "/dev/sdb", "/dev/nvme0n1", "/dev/nvme10n2"}
	for _, p := range valid {
		if err := ValidateDiskPath(p); err != nil {
			t.Errorf("expected valid disk path %q, got error: %v", p, err)
		}
	}

	invalid := []string{"/dev/md0", "/dev/sda1", "/etc/passwd", "sda", "/dev/sda; rm -rf /"}
	for _, p := range invalid {
		if err := ValidateDiskPath(p); err == nil {
			t.Errorf("expected invalid disk path %q to fail", p)
		}
	}
}

func TestValidateImportDevicePath(t *testing.T) {
	valid := []string{"", "/dev/sda", "/dev/sda1", "/dev/disk/by-id/ata-test_123"}
	for _, path := range valid {
		if err := ValidateImportDevicePath(path); err != nil {
			t.Errorf("expected valid import path %q, got %v", path, err)
		}
	}
	invalid := []string{"/tmp/disk", "/dev/../sda", "/dev/sda;rm", "sda"}
	for _, path := range invalid {
		if err := ValidateImportDevicePath(path); err == nil {
			t.Errorf("expected invalid import path %q to fail", path)
		}
	}
}

// --- Pool command building tests ---

func TestBuildCreateArgsBasic(t *testing.T) {
	args, err := BuildCreateArgs("tank", "raidz1",
		[]string{"/dev/sda", "/dev/sdb", "/dev/sdc"}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "raidz1", "/dev/sda", "/dev/sdb", "/dev/sdc"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsMirror(t *testing.T) {
	args, err := BuildCreateArgs("tank", "mirror",
		[]string{"/dev/sda", "/dev/sdb"}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "mirror", "/dev/sda", "/dev/sdb"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsWithSLOG(t *testing.T) {
	args, err := BuildCreateArgs("tank", "raidz1",
		[]string{"/dev/sda", "/dev/sdb", "/dev/sdc"},
		[]string{"/dev/nvme0n1"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "raidz1",
		"/dev/sda", "/dev/sdb", "/dev/sdc",
		"log", "/dev/nvme0n1"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsWithMirroredSLOG(t *testing.T) {
	args, err := BuildCreateArgs("tank", "raidz1",
		[]string{"/dev/sda", "/dev/sdb", "/dev/sdc"},
		[]string{"/dev/nvme0n1", "/dev/nvme1n1"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "raidz1",
		"/dev/sda", "/dev/sdb", "/dev/sdc",
		"log", "mirror", "/dev/nvme0n1", "/dev/nvme1n1"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsWithL2ARC(t *testing.T) {
	args, err := BuildCreateArgs("tank", "mirror",
		[]string{"/dev/sda", "/dev/sdb"},
		nil, []string{"/dev/nvme0n1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "mirror",
		"/dev/sda", "/dev/sdb",
		"cache", "/dev/nvme0n1"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsWithSLOGAndL2ARC(t *testing.T) {
	args, err := BuildCreateArgs("tank", "raidz2",
		[]string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"},
		[]string{"/dev/nvme0n1"},
		[]string{"/dev/nvme1n1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "raidz2",
		"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd",
		"log", "/dev/nvme0n1",
		"cache", "/dev/nvme1n1"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsNoVdevType(t *testing.T) {
	// Single disk, no vdev type (stripe).
	args, err := BuildCreateArgs("tank", "", []string{"/dev/sda"}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"create", "-f", "tank", "/dev/sda"}
	assertArgs(t, args, expected)
}

func TestBuildCreateArgsInvalidPoolName(t *testing.T) {
	_, err := BuildCreateArgs("0bad", "mirror", []string{"/dev/sda", "/dev/sdb"}, nil, nil)
	if err == nil {
		t.Error("expected error for invalid pool name")
	}
}

func TestBuildCreateArgsInvalidDisk(t *testing.T) {
	_, err := BuildCreateArgs("tank", "mirror", []string{"/dev/sda", "/tmp/evil"}, nil, nil)
	if err == nil {
		t.Error("expected error for invalid disk path")
	}
}

func TestBuildCreateArgsInvalidVdevType(t *testing.T) {
	_, err := BuildCreateArgs("tank", "raid5", []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}, nil, nil)
	if err == nil {
		t.Error("expected error for invalid vdev type")
	}
}

// --- Pool status parsing ---

func TestParsePoolStatusOutput(t *testing.T) {
	output := `  pool: tank
 state: ONLINE
  scan: scrub repaired 0B in 00:01:23 with 0 errors on Sun Mar 29 02:00:00 2026
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  raidz1-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
	    sdb     ONLINE       0     0     0
	    sdc     ONLINE       0     0     0
	logs
	  nvme0n1   ONLINE       0     0     0
	cache
	  nvme1n1   ONLINE       0     0     0

errors: No known data errors
`

	vdevLayout, scanStatus, errors := ParsePoolStatusOutput(output)

	if !containsStr(vdevLayout, "raidz1-0") {
		t.Errorf("vdev layout should contain raidz1-0, got:\n%s", vdevLayout)
	}
	if !containsStr(vdevLayout, "nvme0n1") {
		t.Errorf("vdev layout should contain slog nvme0n1, got:\n%s", vdevLayout)
	}
	if !containsStr(scanStatus, "scrub repaired") {
		t.Errorf("scan status should contain scrub info, got: %s", scanStatus)
	}
	if errors != "No known data errors" {
		t.Errorf("expected 'No known data errors', got: %s", errors)
	}
}

func TestParsePoolStatusDegraded(t *testing.T) {
	output := `  pool: tank
 state: DEGRADED
  scan: resilver in progress since Sat Mar 29 10:00:00 2026
config:

	NAME                    STATE     READ WRITE CKSUM
	tank                    DEGRADED     0     0     0
	  raidz1-0              DEGRADED     0     0     0
	    sda                 ONLINE       0     0     0
	    replacing-1         DEGRADED     0     0     0
	      old-sdb           REMOVED      0     0     0
	      sdd               ONLINE       0     0     0  (resilvering)
	    sdc                 ONLINE       0     0     0

errors: No known data errors
`

	vdevLayout, scanStatus, errors := ParsePoolStatusOutput(output)

	if !containsStr(vdevLayout, "DEGRADED") {
		t.Errorf("vdev layout should show DEGRADED, got:\n%s", vdevLayout)
	}
	if !containsStr(vdevLayout, "resilvering") {
		t.Errorf("vdev layout should show resilvering, got:\n%s", vdevLayout)
	}
	if !containsStr(scanStatus, "resilver in progress") {
		t.Errorf("scan status should show resilver, got: %s", scanStatus)
	}
	if errors != "No known data errors" {
		t.Errorf("expected no errors, got: %s", errors)
	}
}

func TestHasSpecialVdevInLayout(t *testing.T) {
	layout := `
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  raidz1-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
	special
	  mirror-1  ONLINE       0     0     0
	    nvme0n1 ONLINE       0     0     0
	    nvme1n1 ONLINE       0     0     0
`
	if !HasSpecialVdevInLayout(layout) {
		t.Fatal("expected special vdev to be detected")
	}
	if HasSpecialVdevInLayout("cache\n  nvme0n1 ONLINE 0 0 0") {
		t.Fatal("cache vdev must not count as special")
	}
}

func TestParseImportablePools(t *testing.T) {
	output := `   pool: tank
     id: 123456789
  state: ONLINE
 action: The pool can be imported using its name or numeric identifier.
 config:

	tank        ONLINE
	  sdb       ONLINE

   pool: archive
     id: 987654321
  state: DEGRADED
 status: One or more devices contains corrupted data.
        The pool can still be imported.
 action: The pool can be imported despite missing devices.
 config:

	archive    DEGRADED
	  sdc      ONLINE
`

	pools := ParseImportablePools(output)
	if len(pools) != 2 {
		t.Fatalf("got %d importable pools, want 2: %#v", len(pools), pools)
	}
	if pools[0].Name != "tank" || pools[0].ID != "123456789" || pools[0].State != "ONLINE" {
		t.Fatalf("unexpected first pool: %#v", pools[0])
	}
	if pools[1].Name != "archive" || pools[1].ID != "987654321" || pools[1].State != "DEGRADED" {
		t.Fatalf("unexpected second pool: %#v", pools[1])
	}
	if !strings.Contains(pools[1].Status, "corrupted data") || !strings.Contains(pools[1].Status, "still be imported") {
		t.Fatalf("status was not collected: %q", pools[1].Status)
	}
}

// --- Dataset validation tests ---

func TestValidateDatasetName(t *testing.T) {
	valid := []string{"tank/data", "pool0/share-1", "tank/nested/deep", "Tank"}
	for _, name := range valid {
		if err := ValidateDatasetName(name); err != nil {
			t.Errorf("expected valid dataset name %q, got error: %v", name, err)
		}
	}

	invalid := []string{"", "0bad", "-bad", "has space/ds"}
	for _, name := range invalid {
		if err := ValidateDatasetName(name); err == nil {
			t.Errorf("expected invalid dataset name %q to fail", name)
		}
	}
}

func TestValidateCompression(t *testing.T) {
	valid := []string{"lz4", "zstd", "off", "on", "gzip", "lzjb", "zle", "zstd-fast"}
	for _, algo := range valid {
		if err := ValidateCompression(algo); err != nil {
			t.Errorf("expected valid compression %q, got error: %v", algo, err)
		}
	}

	invalid := []string{"snappy", "lzo", "", "brotli"}
	for _, algo := range invalid {
		if err := ValidateCompression(algo); err == nil {
			t.Errorf("expected invalid compression %q to fail", algo)
		}
	}
}

// --- Dataset command building tests ---

func TestBuildCreateDatasetArgsBasic(t *testing.T) {
	args, err := BuildCreateDatasetArgs("tank/data", "", "", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"create", "tank/data"}
	assertArgs(t, args, expected)
}

func TestBuildCreateDatasetArgsFull(t *testing.T) {
	args, err := BuildCreateDatasetArgs("tank/data", "/mnt/data", "lz4", 1073741824, 536870912)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"create",
		"-o", "mountpoint=/mnt/data",
		"-o", "compression=lz4",
		"-o", "quota=1073741824",
		"-o", "reservation=536870912",
		"tank/data"}
	assertArgs(t, args, expected)
}

func TestBuildCreateDatasetArgsInvalidName(t *testing.T) {
	_, err := BuildCreateDatasetArgs("0bad", "", "", 0, 0)
	if err == nil {
		t.Error("expected error for invalid dataset name")
	}
}

func TestBuildCreateDatasetArgsInvalidCompression(t *testing.T) {
	_, err := BuildCreateDatasetArgs("tank/data", "", "snappy", 0, 0)
	if err == nil {
		t.Error("expected error for invalid compression")
	}
}

// --- Zvol command building tests ---

func TestBuildCreateZvolArgsBasic(t *testing.T) {
	args, err := BuildCreateZvolArgs("tank/vol0", "100G", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"create", "-V", "100G", "tank/vol0"}
	assertArgs(t, args, expected)
}

func TestBuildCreateZvolArgsWithBlockSize(t *testing.T) {
	args, err := BuildCreateZvolArgs("tank/vol0", "100G", "8K")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"create", "-V", "100G", "-b", "8K", "tank/vol0"}
	assertArgs(t, args, expected)
}

func TestBuildCreateZvolArgsInvalidName(t *testing.T) {
	_, err := BuildCreateZvolArgs("0bad", "100G", "")
	if err == nil {
		t.Error("expected error for invalid zvol name")
	}
}

// --- Snapshot validation tests ---

func TestValidateSnapshotName(t *testing.T) {
	valid := []string{"tank/data@snap1", "pool/ds@backup-2026", "tank@daily.1"}
	for _, name := range valid {
		if err := ValidateSnapshotName(name); err != nil {
			t.Errorf("expected valid snapshot name %q, got error: %v", name, err)
		}
	}

	invalid := []string{
		"tank/data", // no @
		"@snap",     // no dataset
		"",          // empty
		"0bad@snap", // bad dataset
	}
	for _, name := range invalid {
		if err := ValidateSnapshotName(name); err == nil {
			t.Errorf("expected invalid snapshot name %q to fail", name)
		}
	}
}

func TestBuildSnapshotName(t *testing.T) {
	name := BuildSnapshotName("tank/data", "daily-1")
	if name != "tank/data@daily-1" {
		t.Errorf("expected tank/data@daily-1, got %s", name)
	}
}

// --- Human size tests ---

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

// --- Health monitoring tests ---

func TestHasSLOGInLayout(t *testing.T) {
	withSLOG := `
		NAME        STATE     READ WRITE CKSUM
		tank        ONLINE       0     0     0
		  raidz1-0  ONLINE       0     0     0
		    sda     ONLINE       0     0     0
		logs
		  nvme0n1   ONLINE       0     0     0`

	without := `
		NAME        STATE     READ WRITE CKSUM
		tank        ONLINE       0     0     0
		  sda       ONLINE       0     0     0`

	if !HasSLOGInLayout(withSLOG) {
		t.Error("expected HasSLOGInLayout to return true when logs section is present")
	}
	if HasSLOGInLayout(without) {
		t.Error("expected HasSLOGInLayout to return false when no logs section")
	}
}

func TestHasL2ARCInLayout(t *testing.T) {
	withL2ARC := `
		NAME        STATE     READ WRITE CKSUM
		tank        ONLINE       0     0     0
		  sda       ONLINE       0     0     0
		cache
		  nvme0n1   ONLINE       0     0     0`

	without := `
		NAME        STATE     READ WRITE CKSUM
		tank        ONLINE       0     0     0
		  sda       ONLINE       0     0     0`

	if !HasL2ARCInLayout(withL2ARC) {
		t.Error("expected HasL2ARCInLayout to return true when cache section is present")
	}
	if HasL2ARCInLayout(without) {
		t.Error("expected HasL2ARCInLayout to return false when no cache section")
	}
}

func TestHasChecksumErrorsInLayout(t *testing.T) {
	clean := `	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  raidz1-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
	    sdb     ONLINE       0     0     0`

	withErrors := `	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  raidz1-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
	    sdb     ONLINE       0     0     5`

	if HasChecksumErrorsInLayout(clean) {
		t.Error("expected HasChecksumErrorsInLayout to return false for clean layout")
	}
	if !HasChecksumErrorsInLayout(withErrors) {
		t.Error("expected HasChecksumErrorsInLayout to return true when CKSUM > 0")
	}
	// Dash placeholder (no data) should not be treated as an error.
	withDash := `	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     -`
	if HasChecksumErrorsInLayout(withDash) {
		t.Error("expected HasChecksumErrorsInLayout to treat dash as no error")
	}
}

func TestCheckARCAlerts(t *testing.T) {
	// No alerts when stats are nil.
	if alerts := CheckARCAlerts(nil); len(alerts) != 0 {
		t.Errorf("expected 0 alerts for nil stats, got %d", len(alerts))
	}

	// No L2ARC alert when total accesses are below the minimum threshold (< 1000).
	lowTraffic := &ARCStats{L2Hits: 5, L2Misses: 994}
	if alerts := CheckARCAlerts(lowTraffic); len(alerts) != 0 {
		t.Errorf("expected no alert for low-traffic L2ARC, got %d", len(alerts))
	}

	// L2ARC alert when hit rate < 10% with sufficient traffic.
	badL2ARC := &ARCStats{L2Hits: 50, L2Misses: 1950} // 2.5% hit rate
	alerts := CheckARCAlerts(badL2ARC)
	if len(alerts) != 1 {
		t.Errorf("expected 1 L2ARC alert, got %d", len(alerts))
	} else if alerts[0].Severity != "warning" {
		t.Errorf("expected warning severity, got %s", alerts[0].Severity)
	}

	// No L2ARC alert when hit rate >= 10%.
	goodL2ARC := &ARCStats{L2Hits: 200, L2Misses: 800} // 20% hit rate
	if alerts := CheckARCAlerts(goodL2ARC); len(alerts) != 0 {
		t.Errorf("expected no alert for good L2ARC hit rate, got %d", len(alerts))
	}

	// ARC pressure alert when c <= c_min AND ARC has seen meaningful traffic.
	pressured := &ARCStats{
		Hits: 500, Misses: 1500,
		C: 67108864, CMin: 67108864, CMax: 1073741824,
	}
	alerts = CheckARCAlerts(pressured)
	if len(alerts) != 1 {
		t.Errorf("expected 1 ARC pressure alert, got %d", len(alerts))
	} else if alerts[0].Severity != "warning" {
		t.Errorf("expected warning severity, got %s", alerts[0].Severity)
	}

	// No ARC pressure alert when c <= c_min but ARC is idle (e.g. no pools).
	// ZFS initialises c == c_min by default, so without an activity guard this
	// would permanently fire on any system with ZFS loaded and no pools.
	idle := &ARCStats{C: 67108864, CMin: 67108864, CMax: 1073741824}
	if alerts := CheckARCAlerts(idle); len(alerts) != 0 {
		t.Errorf("expected no alert for idle ARC with no traffic, got %d", len(alerts))
	}

	// No ARC pressure alert when c > c_min.
	healthy := &ARCStats{C: 268435456, CMin: 67108864, CMax: 1073741824}
	if alerts := CheckARCAlerts(healthy); len(alerts) != 0 {
		t.Errorf("expected no alert for healthy ARC, got %d", len(alerts))
	}

	// Both alerts fire simultaneously.
	both := &ARCStats{
		Hits: 500, Misses: 1500,
		L2Hits: 10, L2Misses: 1990, // 0.5% hit rate, high traffic
		C: 67108864, CMin: 67108864, CMax: 1073741824,
	}
	alerts = CheckARCAlerts(both)
	if len(alerts) != 2 {
		t.Errorf("expected 2 alerts (L2ARC + ARC pressure), got %d", len(alerts))
	}
}

// --- helpers ---

func assertArgs(t *testing.T, got, expected []string) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(got), got)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
