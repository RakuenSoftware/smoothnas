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
		"",          // empty
		"0bad",      // starts with digit
		"has space",
		"semi;colon",
		"global",    // reserved
		"homes",     // reserved
		"printers",  // reserved
		"Global",    // reserved (case insensitive)
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

func TestGenerateConfigEmpty(t *testing.T) {
	config := GenerateConfig(nil, "smoothnas")
	if !strings.Contains(config, "[global]") {
		t.Error("config should contain [global] section")
	}
	if !strings.Contains(config, "server string = smoothnas") {
		t.Error("config should contain hostname")
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

	config := GenerateConfig(shares, "mynas")

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
	config := GenerateConfig(nil, "nas")
	// Should have security settings.
	if !strings.Contains(config, "security = user") {
		t.Error("missing security = user")
	}
	if !strings.Contains(config, "map to guest = never") {
		t.Error("missing map to guest = never")
	}
}
