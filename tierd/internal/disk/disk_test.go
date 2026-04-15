package disk

import "testing"

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
		"/dev/sd",        // no letter after sd
		"/dev/sda1",      // partition, not disk
		"/dev/nvme0n",    // incomplete
		"/dev/nvmen1",    // no digits before n
		"/dev/md0",       // not a physical disk path
		"/etc/passwd",    // not a device
		"sda",            // no /dev/ prefix
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
