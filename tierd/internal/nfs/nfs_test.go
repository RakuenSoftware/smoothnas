package nfs

import (
	"strings"
	"testing"
)

func TestValidateExportPath(t *testing.T) {
	valid := []string{"/mnt/data", "/mnt/pool/share", "/srv/nfs", "/data"}
	for _, path := range valid {
		if err := ValidateExportPath(path); err != nil {
			t.Errorf("expected valid export path %q, got error: %v", path, err)
		}
	}

	invalid := []string{
		"relative",
		"/mnt/da ta",      // space
		"/mnt/data;rm",    // semicolon
		"/mnt/data$(cmd)", // shell expansion
		"",                // empty
	}
	for _, path := range invalid {
		if err := ValidateExportPath(path); err == nil {
			t.Errorf("expected invalid export path %q to fail", path)
		}
	}
}

func TestValidateNetwork(t *testing.T) {
	valid := []string{
		"192.168.1.0/24",
		"10.0.0.0/8",
		"192.168.1.100",
		"*",
		"client.example.com",
		"2001:db8::/32",
	}
	for _, net := range valid {
		if err := ValidateNetwork(net); err != nil {
			t.Errorf("expected valid network %q, got error: %v", net, err)
		}
	}

	invalid := []string{
		"",
		"not a network!",
		"192.168.1.0/33", // invalid CIDR mask
	}
	for _, net := range invalid {
		if err := ValidateNetwork(net); err == nil {
			t.Errorf("expected invalid network %q to fail", net)
		}
	}
}

func TestBuildOptions(t *testing.T) {
	tests := []struct {
		export   Export
		expected string
	}{
		{
			Export{},
			"rw,async,no_root_squash,no_subtree_check",
		},
		{
			Export{Sync: true, RootSquash: true, ReadOnly: false},
			"rw,sync,root_squash,no_subtree_check",
		},
		{
			Export{Sync: false, RootSquash: false, ReadOnly: true},
			"ro,async,no_root_squash,no_subtree_check",
		},
		{
			Export{Sync: true, RootSquash: false, ReadOnly: false},
			"rw,sync,no_root_squash,no_subtree_check",
		},
	}

	for _, tt := range tests {
		got := BuildOptions(tt.export)
		if got != tt.expected {
			t.Errorf("BuildOptions() = %q, want %q", got, tt.expected)
		}
	}
}

func TestGenerateExportsEmpty(t *testing.T) {
	content := GenerateExports(nil)
	if !strings.Contains(content, "Auto-generated") {
		t.Error("should contain auto-generated comment")
	}
}

func TestGenerateExportsWithEntries(t *testing.T) {
	exports := []Export{
		{
			Path:       "/mnt/data",
			Networks:   []string{"192.168.1.0/24", "10.0.0.0/8"},
			Sync:       true,
			RootSquash: true,
			ReadOnly:   false,
		},
		{
			Path:       "/mnt/public",
			Networks:   []string{"*"},
			Sync:       true,
			RootSquash: true,
			ReadOnly:   true,
		},
	}

	content := GenerateExports(exports)

	// Check first export with two networks creates two lines.
	if !strings.Contains(content, "/mnt/data 192.168.1.0/24(rw,sync,root_squash,no_subtree_check)") {
		t.Errorf("missing first network for /mnt/data, got:\n%s", content)
	}
	if !strings.Contains(content, "/mnt/data 10.0.0.0/8(rw,sync,root_squash,no_subtree_check)") {
		t.Errorf("missing second network for /mnt/data, got:\n%s", content)
	}

	// Check public export.
	if !strings.Contains(content, "/mnt/public *(ro,sync,root_squash,no_subtree_check)") {
		t.Errorf("missing public export, got:\n%s", content)
	}
}

func TestGenerateExportsMultipleNetworks(t *testing.T) {
	exports := []Export{
		{
			Path:     "/mnt/share",
			Networks: []string{"192.168.1.0/24", "192.168.2.0/24", "10.0.0.5"},
			Sync:     true,
		},
	}

	content := GenerateExports(exports)
	lines := strings.Split(strings.TrimSpace(content), "\n")

	// Header + 3 export lines.
	exportLines := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "/mnt/share") {
			exportLines++
		}
	}
	if exportLines != 3 {
		t.Errorf("expected 3 export lines, got %d in:\n%s", exportLines, content)
	}
}

func TestServerTuningConfig(t *testing.T) {
	content := ServerTuningConfig()
	want := []string{
		"[nfsd]",
		"threads = 32",
		"vers3 = y",
		"vers4.2 = y",
		"[mountd]",
		"port = 20048",
		"[statd]",
		"port = 32765",
		"outgoing-port = 32766",
		"[lockd]",
		"port = 32767",
		"udp-port = 32767",
	}
	for _, s := range want {
		if !strings.Contains(content, s) {
			t.Fatalf("ServerTuningConfig missing %q:\n%s", s, content)
		}
	}
}

func TestNfsdSystemdDropInConfig(t *testing.T) {
	content := NfsdSystemdDropInConfig()
	want := []string{
		"[Service]",
		"ExecStartPre=",
		"/proc/fs/nfsd/max_block_size",
		"2097152",
	}
	for _, s := range want {
		if !strings.Contains(content, s) {
			t.Fatalf("NfsdSystemdDropInConfig missing %q:\n%s", s, content)
		}
	}
}

func TestLockdSysctlConfig(t *testing.T) {
	content := LockdSysctlConfig()
	want := []string{
		"fs.nfs.nlm_tcpport = 32767",
		"fs.nfs.nlm_udpport = 32767",
	}
	for _, s := range want {
		if !strings.Contains(content, s) {
			t.Fatalf("LockdSysctlConfig missing %q:\n%s", s, content)
		}
	}
}

func TestLockdModprobeConfig(t *testing.T) {
	content := LockdModprobeConfig()
	want := []string{
		"options lockd",
		"nlm_tcpport=32767",
		"nlm_udpport=32767",
	}
	for _, s := range want {
		if !strings.Contains(content, s) {
			t.Fatalf("LockdModprobeConfig missing %q:\n%s", s, content)
		}
	}
}

func TestDefaultClientMountOptions(t *testing.T) {
	want := []string{
		"vers=4.2",
		"rsize=2097152",
		"wsize=2097152",
		"lookupcache=all",
		"actimeo=60",
		"timeo=50",
		"retrans=3",
	}
	for _, s := range want {
		if !strings.Contains(DefaultClientMountOptions, s) {
			t.Fatalf("DefaultClientMountOptions missing %q: %s", s, DefaultClientMountOptions)
		}
	}
}
