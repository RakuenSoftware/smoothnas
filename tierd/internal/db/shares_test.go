package db_test

import (
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func openSharesDB(t *testing.T) *db.Store {
	t.Helper()
	store := openTestDB(t)
	if err := store.MigrateShares(); err != nil {
		t.Fatalf("migrate shares: %v", err)
	}
	return store
}

// --- SMB Shares ---

func TestSmbShareCRUD(t *testing.T) {
	store := openSharesDB(t)

	// Create.
	share, err := store.CreateSmbShare(db.SmbShare{Name: "data", Path: "/mnt/data", Comment: "Data share"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if share.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	if share.Name != "data" {
		t.Errorf("expected name 'data', got %q", share.Name)
	}

	// List.
	shares, err := store.ListSmbShares()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(shares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(shares))
	}

	// Delete.
	if err := store.DeleteSmbShare("data"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	shares, _ = store.ListSmbShares()
	if len(shares) != 0 {
		t.Fatalf("expected 0 shares after delete, got %d", len(shares))
	}
}

func TestSmbShareDuplicate(t *testing.T) {
	store := openSharesDB(t)

	store.CreateSmbShare(db.SmbShare{Name: "test", Path: "/mnt/test"})
	_, err := store.CreateSmbShare(db.SmbShare{Name: "test", Path: "/mnt/other"})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestSmbShareDeleteNotFound(t *testing.T) {
	store := openSharesDB(t)

	err := store.DeleteSmbShare("nonexistent")
	if err == nil {
		t.Fatal("expected not found error")
	}
}

func TestSmbShareBoolFields(t *testing.T) {
	store := openSharesDB(t)

	store.CreateSmbShare(db.SmbShare{Name: "ro", Path: "/mnt/ro", ReadOnly: true, GuestOK: true})

	shares, _ := store.ListSmbShares()
	if !shares[0].ReadOnly {
		t.Error("expected read_only true")
	}
	if !shares[0].GuestOK {
		t.Error("expected guest_ok true")
	}
}

// --- NFS Exports ---

func TestNfsExportCRUD(t *testing.T) {
	store := openSharesDB(t)

	exp, err := store.CreateNfsExport(db.NfsExport{
		Path: "/mnt/data", Networks: "192.168.1.0/24", Sync: true, RootSquash: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if exp.ID == 0 {
		t.Fatal("expected non-zero ID")
	}

	exports, err := store.ListNfsExports()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}
	if !exports[0].Sync {
		t.Error("expected sync true")
	}

	if err := store.DeleteNfsExport(exp.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	exports, _ = store.ListNfsExports()
	if len(exports) != 0 {
		t.Fatalf("expected 0 exports after delete, got %d", len(exports))
	}
}

// --- iSCSI Targets ---

func TestIscsiTargetCRUD(t *testing.T) {
	store := openSharesDB(t)

	target, err := store.CreateIscsiTarget(db.IscsiTarget{
		IQN: "iqn.2026-01.com.smoothnas:host:vol0", BlockDevice: "/dev/zvol/tank/lun0",
		CHAPUser: "user", CHAPPass: "password1234",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !target.HasCHAP {
		t.Error("expected has_chap true")
	}

	targets, err := store.ListIscsiTargets()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	// Password should not be in list results.
	if targets[0].CHAPPass != "" {
		t.Error("CHAP password should not be returned in list")
	}

	if err := store.DeleteIscsiTarget("iqn.2026-01.com.smoothnas:host:vol0"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	targets, _ = store.ListIscsiTargets()
	if len(targets) != 0 {
		t.Fatalf("expected 0 targets after delete, got %d", len(targets))
	}
}

func TestIscsiTargetDuplicate(t *testing.T) {
	store := openSharesDB(t)

	store.CreateIscsiTarget(db.IscsiTarget{IQN: "iqn.2026-01.com.smoothnas:dup", BlockDevice: "/dev/zvol/tank/a"})
	_, err := store.CreateIscsiTarget(db.IscsiTarget{IQN: "iqn.2026-01.com.smoothnas:dup", BlockDevice: "/dev/zvol/tank/b"})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}
