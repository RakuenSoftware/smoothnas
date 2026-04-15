package db_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func openTierStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "tiers.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestTierInstancesCreateListDelete(t *testing.T) {
	store := openTierStore(t)

	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}

	tiers, err := store.ListTierInstances()
	if err != nil {
		t.Fatalf("list tiers: %v", err)
	}
	if len(tiers) != 1 {
		t.Fatalf("expected 1 tier, got %d", len(tiers))
	}
	if tiers[0].MountPoint != "/mnt/media" {
		t.Fatalf("mount point = %q, want /mnt/media", tiers[0].MountPoint)
	}
	if tiers[0].Filesystem != "xfs" {
		t.Fatalf("filesystem = %q, want xfs", tiers[0].Filesystem)
	}
	if tiers[0].State != db.TierPoolStateProvisioning {
		t.Fatalf("state = %q, want %q", tiers[0].State, db.TierPoolStateProvisioning)
	}

	if err := store.DeleteTierInstance("media"); err != nil {
		t.Fatalf("delete tier: %v", err)
	}
}

func TestTierInstancesSlotRules(t *testing.T) {
	store := openTierStore(t)
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := store.CreateTierInstance("backup"); err != nil {
		t.Fatalf("create tier: %v", err)
	}

	if err := store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("add array: %v", err)
	}
	a, err := store.GetTierAssignmentByArrayPath("md0")
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if a.State != db.TierSlotStateAssigned {
		t.Fatalf("slot state = %q, want %q", a.State, db.TierSlotStateAssigned)
	}
	if err := store.AddArrayToTierSlot("media", db.TierSlotNVME, "md1"); err == nil {
		t.Fatal("expected slot conflict for second NVME assignment")
	}
	if err := store.AddArrayToTierSlot("backup", db.TierSlotSSD, "md0"); err == nil {
		t.Fatal("expected global array conflict for md0")
	}

	assignments, err := store.GetTierAssignments("media")
	if err != nil {
		t.Fatalf("get tier assignments: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assigned slot, got %d", len(assignments))
	}
	if assignments[0].Rank != 1 {
		t.Fatalf("rank = %d, want 1", assignments[0].Rank)
	}
}

func TestTierInstanceStateTransitions(t *testing.T) {
	store := openTierStore(t)
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateDestroying); err != nil {
		t.Fatalf("transition provisioning to destroying: %v", err)
	}
}

func TestTierInstanceStateTransitionsHealthyToDestroying(t *testing.T) {
	store := openTierStore(t)
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition to healthy: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateDestroying); err != nil {
		t.Fatalf("transition to destroying: %v", err)
	}
}

func TestTierInstanceStateRejectsInvalidTransition(t *testing.T) {
	store := openTierStore(t)
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateDegraded); err == nil {
		t.Fatal("expected invalid provisioning -> degraded transition to fail")
	}
}

func TestTierInstanceErrorAndReconciledTimestamps(t *testing.T) {
	store := openTierStore(t)
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := store.SetTierInstanceError("media", "boom"); err != nil {
		t.Fatalf("set error: %v", err)
	}
	ti, err := store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("get tier: %v", err)
	}
	if ti.State != db.TierPoolStateError {
		t.Fatalf("state = %q, want error", ti.State)
	}
	if ti.ErrorReason != "boom" {
		t.Fatalf("error reason = %q, want boom", ti.ErrorReason)
	}

	time.Sleep(1100 * time.Millisecond)
	if err := store.MarkTierReconciled("media"); err != nil {
		t.Fatalf("mark reconciled: %v", err)
	}
	ti, err = store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("get tier after reconcile: %v", err)
	}
	if ti.LastReconciledAt == "" {
		t.Fatal("expected last_reconciled_at to be set")
	}
	if ti.UpdatedAt == ti.CreatedAt {
		t.Fatalf("expected updated_at to change, got created=%q updated=%q", ti.CreatedAt, ti.UpdatedAt)
	}
}

func TestTierInstanceDestroyingReason(t *testing.T) {
	store := openTierStore(t)
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateDestroying); err != nil {
		t.Fatalf("transition destroying: %v", err)
	}
	if err := store.SetTierInstanceDestroyingReason("media", "umount failed"); err != nil {
		t.Fatalf("set destroying reason: %v", err)
	}

	ti, err := store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("get tier: %v", err)
	}
	if ti.State != db.TierPoolStateDestroying {
		t.Fatalf("state = %q, want destroying", ti.State)
	}
	if ti.ErrorReason != "umount failed" {
		t.Fatalf("error reason = %q, want umount failed", ti.ErrorReason)
	}
}

func TestValidateTierInstanceName(t *testing.T) {
	valid := []string{
		"media",
		"pool_1",
		"z",
		"9fast",
		"archive-tier_2",
	}
	for _, name := range valid {
		if err := db.ValidateTierInstanceName(name); err != nil {
			t.Fatalf("expected %q to be valid, got %v", name, err)
		}
	}

	tests := []struct {
		name string
		want string
	}{
		{name: "", want: "required"},
		{name: "Media", want: "must start with a lowercase letter or digit"},
		{name: "-media", want: "must start with a lowercase letter or digit"},
		{name: "media!", want: "must contain only lowercase letters, digits, hyphens, or underscores"},
		{name: strings.Repeat("a", 32), want: "31 characters or fewer"},
		{name: "root", want: "is reserved"},
		{name: "lost+found", want: "is reserved"},
	}
	for _, tc := range tests {
		err := db.ValidateTierInstanceName(tc.name)
		if err == nil {
			t.Fatalf("expected %q to be rejected", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %q error to contain %q, got %v", tc.name, tc.want, err)
		}
	}
}
