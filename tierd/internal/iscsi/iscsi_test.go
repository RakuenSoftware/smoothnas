package iscsi

import (
	"strings"
	"testing"
)

func TestValidateIQN(t *testing.T) {
	valid := []string{
		"iqn.2026-01.com.smoothnas:myhost:vol0",
		"iqn.2026-03.com.example:storage",
		"iqn.2020-01.org.linux-iscsi:lun0",
	}
	for _, iqn := range valid {
		if err := ValidateIQN(iqn); err != nil {
			t.Errorf("expected valid IQN %q, got error: %v", iqn, err)
		}
	}

	invalid := []string{
		"",
		"not-an-iqn",
		"iqn.2026.com.bad",     // missing month
		"iqn.26-01.com.bad",    // 2-digit year
		"naa.5000c5000c5000c5", // NAA format, not IQN
		"iqn.2026-01.com.bad:has space",
	}
	for _, iqn := range invalid {
		if err := ValidateIQN(iqn); err == nil {
			t.Errorf("expected invalid IQN %q to fail", iqn)
		}
	}
}

func TestValidateBlockDevice(t *testing.T) {
	valid := []string{
		"/dev/zvol/tank/lun0",
		"/dev/zvol/tank/iscsi/vol1",
		"/dev/vg0/data",
	}
	for _, path := range valid {
		if err := ValidateBlockDevice(path); err != nil {
			t.Errorf("expected valid block device %q, got error: %v", path, err)
		}
	}

	invalid := []string{
		"/dev/sda", // raw disk, not LV/zvol
		"/etc/passwd",
		"",
		"/dev/zvol/",
		"/dev/zvol/0bad", // starts with digit
	}
	for _, path := range invalid {
		if err := ValidateBlockDevice(path); err == nil {
			t.Errorf("expected invalid block device %q to fail", path)
		}
	}
}

func TestGenerateIQN(t *testing.T) {
	iqn := GenerateIQN("myhost", "tank/lun0")
	expected := "iqn.2026-01.com.smoothnas:myhost:tank.lun0"
	if iqn != expected {
		t.Errorf("expected %q, got %q", expected, iqn)
	}
}

func TestIQNToBackstoreName(t *testing.T) {
	name := IQNToBackstoreName("iqn.2026-01.com.smoothnas:myhost:vol0")
	// Should replace dots, colons, hyphens with underscores.
	if strings.Contains(name, ".") || strings.Contains(name, ":") || strings.Contains(name, "-") {
		t.Errorf("backstore name should not contain ., :, or -: got %q", name)
	}
	if len(name) > 64 {
		t.Errorf("backstore name too long: %d chars", len(name))
	}
}

func TestIQNToBackstoreNameTruncation(t *testing.T) {
	// Very long IQN.
	longIQN := "iqn.2026-01.com.smoothnas:host:" + strings.Repeat("a", 100)
	name := IQNToBackstoreName(longIQN)
	if len(name) > 64 {
		t.Errorf("backstore name should be truncated to 64, got %d", len(name))
	}
}

func TestBuildCreateTargetArgs(t *testing.T) {
	bsArgs, tgtArgs, lunArgs, err := BuildCreateTargetArgs(
		"iqn.2026-01.com.smoothnas:myhost:vol0",
		"/dev/zvol/tank/lun0",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Backstore args should contain "create" and the device path.
	foundCreate := false
	foundDev := false
	for _, a := range bsArgs {
		if a == "create" {
			foundCreate = true
		}
		if strings.Contains(a, "/dev/zvol/tank/lun0") {
			foundDev = true
		}
	}
	if !foundCreate {
		t.Error("backstore args missing 'create'")
	}
	if !foundDev {
		t.Error("backstore args missing device path")
	}

	// Target args should contain the IQN.
	foundIQN := false
	for _, a := range tgtArgs {
		if a == "iqn.2026-01.com.smoothnas:myhost:vol0" {
			foundIQN = true
		}
	}
	if !foundIQN {
		t.Error("target args missing IQN")
	}

	// LUN args should reference the backstore.
	if len(lunArgs) < 3 {
		t.Fatalf("expected at least 3 lun args, got %d", len(lunArgs))
	}
}

func TestBuildCreateTargetArgsInvalidIQN(t *testing.T) {
	_, _, _, err := BuildCreateTargetArgs("bad-iqn", "/dev/zvol/tank/lun0")
	if err == nil {
		t.Error("expected error for invalid IQN")
	}
}

func TestBuildCreateTargetArgsInvalidDevice(t *testing.T) {
	_, _, _, err := BuildCreateTargetArgs(
		"iqn.2026-01.com.smoothnas:host:vol0",
		"/dev/sda",
	)
	if err == nil {
		t.Error("expected error for invalid block device")
	}
}

func TestBuildSetTargetPortalGroupStateArgs(t *testing.T) {
	iqn := "iqn.2026-01.com.smoothnas:host:vol0"

	disableArgs, err := BuildSetTargetPortalGroupStateArgs(iqn, false)
	if err != nil {
		t.Fatalf("disable args: %v", err)
	}
	if got, want := strings.Join(disableArgs, " "), "/iscsi/"+iqn+"/tpg1 disable"; got != want {
		t.Fatalf("disable args = %q, want %q", got, want)
	}

	enableArgs, err := BuildSetTargetPortalGroupStateArgs(iqn, true)
	if err != nil {
		t.Fatalf("enable args: %v", err)
	}
	if got, want := strings.Join(enableArgs, " "), "/iscsi/"+iqn+"/tpg1 enable"; got != want {
		t.Fatalf("enable args = %q, want %q", got, want)
	}
}

func TestBuildSetTargetPortalGroupStateArgsInvalidIQN(t *testing.T) {
	if _, err := BuildSetTargetPortalGroupStateArgs("bad-iqn", false); err == nil {
		t.Fatal("expected error for invalid IQN")
	}
}

func TestSetCHAPValidation(t *testing.T) {
	// Password too short.
	err := SetCHAP("iqn.2026-01.com.smoothnas:host:vol0", "user", "short")
	if err == nil {
		t.Error("expected error for short CHAP password")
	}
	if !strings.Contains(err.Error(), "at least 12") {
		t.Errorf("expected password length error, got: %v", err)
	}
}
