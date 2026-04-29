package db_test

import (
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestConfigCRUD(t *testing.T) {
	store := openTestDB(t)

	if _, err := store.GetConfig("missing"); err != db.ErrNotFound {
		t.Fatalf("missing config error = %v, want ErrNotFound", err)
	}

	if err := store.SetConfig("smb.compatibility_mode", "1"); err != nil {
		t.Fatalf("set config: %v", err)
	}
	got, err := store.GetConfig("smb.compatibility_mode")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got != "1" {
		t.Fatalf("config = %q, want 1", got)
	}

	if err := store.SetBoolConfig("smb.compatibility_mode", false); err != nil {
		t.Fatalf("set bool: %v", err)
	}
	enabled, err := store.GetBoolConfig("smb.compatibility_mode", true)
	if err != nil {
		t.Fatalf("get bool: %v", err)
	}
	if enabled {
		t.Fatal("expected bool config false")
	}

	if err := store.DeleteConfig("smb.compatibility_mode"); err != nil {
		t.Fatalf("delete config: %v", err)
	}
	if _, err := store.GetConfig("smb.compatibility_mode"); err != db.ErrNotFound {
		t.Fatalf("deleted config error = %v, want ErrNotFound", err)
	}
}

func TestGetBoolConfigDefaultAndInvalid(t *testing.T) {
	store := openTestDB(t)

	got, err := store.GetBoolConfig("missing", true)
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if !got {
		t.Fatal("expected default true")
	}

	if err := store.SetConfig("bad", "sometimes"); err != nil {
		t.Fatalf("set invalid: %v", err)
	}
	if _, err := store.GetBoolConfig("bad", false); err == nil {
		t.Fatal("expected invalid boolean error")
	}
}
