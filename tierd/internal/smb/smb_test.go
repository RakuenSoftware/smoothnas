package smb

import (
	"strings"
	"testing"
)

func TestValidateShareName(t *testing.T) {
	valid := []string{"data", "Media", "share-1", "backup_2026"}
	for _, name := range valid {
		if err := ValidateShareName(name); err != nil {
			t.Errorf("expected valid share name %q, got error: %v", name, err)
		}
	}

	invalid := []string{
		"",     // empty
		"0bad", // starts with digit
		"has space",
		"semi;colon",
		"global",   // reserved
		"homes",    // reserved
		"printers", // reserved
		"Global",   // reserved (case insensitive)
	}
	for _, name := range invalid {
		if err := ValidateShareName(name); err == nil {
			t.Errorf("expected invalid share name %q to fail", name)
		}
	}
}

func TestValidateSharePath(t *testing.T) {
	valid := []string{"/mnt/data", "/mnt/pool/share", "/data"}
	for _, path := range valid {
		if err := ValidateSharePath(path); err != nil {
			t.Errorf("expected valid share path %q, got error: %v", path, err)
		}
	}

	invalid := []string{
		"relative/path",
		"/mnt/data;rm",
		"/mnt/data$evil",
		"/mnt/data`cmd`",
	}
	for _, path := range invalid {
		if err := ValidateSharePath(path); err == nil {
			t.Errorf("expected invalid share path %q to fail", path)
		}
	}
}

func TestGenerateConfigEmitsMultichannelByDefault(t *testing.T) {
	config := GenerateConfigWithOptions(nil, "smoothnas", Options{})
	if !strings.Contains(config, "server multi channel support = yes") {
		t.Fatalf("Phase 6: smb.conf must default-on multichannel support\n%s", config)
	}
	// Without operator-supplied Interfaces, no `interfaces=` line; the
	// kernel's default-bind-everywhere is fine in the default-bond shape.
	if strings.Contains(config, "\n   interfaces = ") {
		t.Fatalf("Phase 6: with empty Interfaces, smb.conf should NOT pin interfaces=\n%s", config)
	}
	if strings.Contains(config, "bind interfaces only = yes") {
		t.Fatalf("Phase 6: with empty Interfaces, smb.conf should NOT bind to a subset\n%s", config)
	}
}

func TestGenerateConfigEmitsInterfacesWhenSupplied(t *testing.T) {
	config := GenerateConfigWithOptions(nil, "smoothnas", Options{
		Interfaces: []string{"192.168.1.10", "192.168.1.11", "10.0.0.5"},
	})
	if !strings.Contains(config, "server multi channel support = yes") {
		t.Fatalf("multichannel directive missing")
	}
	if !strings.Contains(config, "   interfaces = 192.168.1.10 192.168.1.11 10.0.0.5\n") {
		t.Fatalf("interfaces= line missing or malformed\n%s", config)
	}
	if !strings.Contains(config, "bind interfaces only = yes") {
		t.Fatalf("bind-interfaces-only missing alongside interfaces= line\n%s", config)
	}
}

func TestGenerateConfigEmpty(t *testing.T) {
	config := GenerateConfigWithOptions(nil, "smoothnas", Options{SmoothFSVFS: true})
	if !strings.Contains(config, "[global]") {
		t.Error("config should contain [global] section")
	}
	if !strings.Contains(config, "server string = smoothnas") {
		t.Error("config should contain hostname")
	}
	if !strings.Contains(config, "kernel oplocks = no") {
		t.Error("config should disable kernel oplocks when smoothfs VFS is active")
	}
	if !strings.Contains(config, "strict sync = no") || !strings.Contains(config, "sync always = no") {
		t.Error("SMB should default to async write acknowledgement")
	}
	if !strings.Contains(config, "disable spoolss = yes") || !strings.Contains(config, "load printers = no") {
		t.Error("SMB should disable unused print services")
	}
	if strings.Contains(config, "pam password change") || strings.Contains(config, "unix password sync") {
		t.Error("SMB should not enable PAM password sync paths")
	}
	if !strings.Contains(config, "case sensitive = yes") || !strings.Contains(config, "mangled names = no") {
		t.Error("SMB should avoid case-insensitive directory scans by default")
	}
}

func TestGenerateConfigWithShares(t *testing.T) {
	shares := []Share{
		{
			Name:     "data",
			Path:     "/mnt/data",
			ReadOnly: false,
			GuestOK:  false,
			Comment:  "Main data share",
		},
		{
			Name:       "public",
			Path:       "/mnt/public",
			ReadOnly:   true,
			GuestOK:    true,
			AllowUsers: []string{"alice", "bob"},
			Comment:    "Public read-only",
		},
	}

	config := GenerateConfigWithOptions(shares, "mynas", Options{SmoothFSVFS: true})

	// Check global section.
	if !strings.Contains(config, "[global]") {
		t.Error("missing [global]")
	}
	if !strings.Contains(config, "server string = mynas") {
		t.Error("missing hostname")
	}

	// Check data share.
	if !strings.Contains(config, "[data]") {
		t.Error("missing [data] section")
	}
	if !strings.Contains(config, "path = /mnt/data") {
		t.Error("missing data path")
	}
	if !strings.Contains(config, "read only = no") {
		t.Error("data should be read-write")
	}
	if !strings.Contains(config, "guest ok = no") {
		t.Error("data should not allow guests")
	}
	if !strings.Contains(config, "comment = Main data share") {
		t.Error("missing data comment")
	}
	if !strings.Contains(config, "vfs objects = smoothfs") {
		t.Error("shares should load the smoothfs Samba VFS module")
	}
	if !strings.Contains(config, "smoothfs:lease watcher = no") ||
		!strings.Contains(config, "smoothfs:stable fileid = no") {
		t.Error("smoothfs VFS expensive metadata features should be opt-in")
	}
	if !strings.Contains(config, "ea support = yes") {
		t.Error("shares should enable extended attributes for smoothfs metadata")
	}
	if !strings.Contains(config, "kernel oplocks = no") {
		t.Error("smoothfs shares should not use Samba kernel oplocks")
	}

	// Check public share.
	if !strings.Contains(config, "[public]") {
		t.Error("missing [public] section")
	}
	if !strings.Contains(config, "read only = yes") {
		t.Error("public should be read-only")
	}
	if !strings.Contains(config, "guest ok = yes") {
		t.Error("public should allow guests")
	}
	if !strings.Contains(config, "valid users = alice bob") {
		t.Error("missing valid users")
	}
}

func TestGenerateConfigSecurity(t *testing.T) {
	config := GenerateConfigWithOptions(nil, "nas", Options{})
	// Should have security settings.
	if !strings.Contains(config, "security = user") {
		t.Error("missing security = user")
	}
	if !strings.Contains(config, "map to guest = never") {
		t.Error("missing map to guest = never")
	}
	if strings.Contains(config, "vfs objects = smoothfs") {
		t.Error("config should not reference smoothfs VFS when the module is unavailable")
	}
}

func TestGenerateConfigCompatibilityMode(t *testing.T) {
	config := GenerateConfigWithOptions(nil, "nas", Options{CompatibilityMode: true})
	if !strings.Contains(config, "case sensitive = auto") {
		t.Error("compatibility mode should use Samba case-insensitive lookup behavior")
	}
	if !strings.Contains(config, "mangled names = illegal") {
		t.Error("compatibility mode should keep Windows short-name behavior")
	}
	if strings.Contains(config, "case sensitive = yes") || strings.Contains(config, "mangled names = no") {
		t.Error("compatibility mode should not emit performance-only case settings")
	}
}
