package tiermeta

import (
	"errors"
	"testing"
)

// newTestStore returns a Store with LVM helpers stubbed out so no real
// lvcreate/lvremove/lvs processes are spawned.  The stubs record calls so
// tests can assert on them.
func newTestStore() *Store {
	s := NewStore()
	s.createLV = func(vg, name, size, pvDevice string) error { return nil }
	s.removeLV = func(vg, name string) error { return nil }
	s.lvExists = func(vg, name string) (bool, error) { return false, nil }
	return s
}

// --- CreatePool ---

func TestCreatePool_BasicSuccess(t *testing.T) {
	s := newTestStore()
	if err := s.CreatePool("media", "xfs", DefaultTierDefinitions()); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	pool, err := s.GetPool("media")
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if pool.Name != "media" {
		t.Errorf("name = %q, want media", pool.Name)
	}
	if pool.State != PoolStateProvisioning {
		t.Errorf("state = %q, want %q", pool.State, PoolStateProvisioning)
	}
	if len(pool.Slots) != 3 {
		t.Errorf("want 3 slots, got %d", len(pool.Slots))
	}
}

func TestCreatePool_DuplicateRejected(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	if err := s.CreatePool("media", "xfs", DefaultTierDefinitions()); err == nil {
		t.Fatal("expected error for duplicate pool name")
	}
}

func TestCreatePool_InvalidName(t *testing.T) {
	s := newTestStore()
	if err := s.CreatePool("", "xfs", DefaultTierDefinitions()); err == nil {
		t.Fatal("expected error for empty name")
	}
	if err := s.CreatePool("ROOT", "xfs", DefaultTierDefinitions()); err == nil {
		t.Fatal("expected error for upper-case name")
	}
}

func TestCreatePool_DefaultsApplied(t *testing.T) {
	s := newTestStore()
	// Pass nil defs — should default to 3 slots.
	if err := s.CreatePool("test", "", nil); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	pool, _ := s.GetPool("test")
	if pool.Filesystem != "xfs" {
		t.Errorf("filesystem = %q, want xfs", pool.Filesystem)
	}
	if len(pool.Slots) != 3 {
		t.Errorf("want 3 slots, got %d", len(pool.Slots))
	}
}

// --- GetPool / ListPools ---

func TestGetPool_NotFound(t *testing.T) {
	s := newTestStore()
	_, err := s.GetPool("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestListPools_Empty(t *testing.T) {
	s := newTestStore()
	if pools := s.ListPools(); len(pools) != 0 {
		t.Errorf("want empty slice, got %d pools", len(pools))
	}
}

func TestListPools_ReturnsCopies(t *testing.T) {
	s := newTestStore()
	s.CreatePool("alpha", "xfs", DefaultTierDefinitions())
	s.CreatePool("beta", "xfs", DefaultTierDefinitions())

	pools := s.ListPools()
	if len(pools) != 2 {
		t.Fatalf("want 2 pools, got %d", len(pools))
	}
	// Mutating the copy must not affect the store.
	pools[0].Name = "mutated"
	again := s.ListPools()
	for _, p := range again {
		if p.Name == "mutated" {
			t.Error("ListPools returned a live reference, not a copy")
		}
	}
}

// --- AssignSlot ---

func TestAssignSlot_Success(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())

	if err := s.AssignSlot("media", SlotNVME, "/dev/md1", "/dev/md1"); err != nil {
		t.Fatalf("AssignSlot: %v", err)
	}
	pool, _ := s.GetPool("media")
	for _, sl := range pool.Slots {
		if sl.SlotName == SlotNVME {
			if sl.State != SlotStateAssigned {
				t.Errorf("slot state = %q, want %q", sl.State, SlotStateAssigned)
			}
			if sl.ArrayPath != "/dev/md1" {
				t.Errorf("array path = %q, want /dev/md1", sl.ArrayPath)
			}
			return
		}
	}
	t.Fatal("NVME slot not found")
}

func TestAssignSlot_DuplicateArrayRejected(t *testing.T) {
	s := newTestStore()
	s.CreatePool("alpha", "xfs", DefaultTierDefinitions())
	s.CreatePool("beta", "xfs", DefaultTierDefinitions())

	s.AssignSlot("alpha", SlotHDD, "/dev/md0", "/dev/md0")
	if err := s.AssignSlot("beta", SlotHDD, "/dev/md0", "/dev/md0"); err == nil {
		t.Fatal("expected error for duplicate array assignment across pools")
	}
}

func TestAssignSlot_AlreadyAssignedRejected(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	s.AssignSlot("media", SlotHDD, "/dev/md0", "/dev/md0")
	if err := s.AssignSlot("media", SlotHDD, "/dev/md1", "/dev/md1"); err == nil {
		t.Fatal("expected error for assigning to already-assigned slot")
	}
}

func TestAssignSlot_NormalizesPath(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	// Omit /dev/ prefix — NormalizeArrayPath should add it.
	if err := s.AssignSlot("media", SlotHDD, "md0", "md0"); err != nil {
		t.Fatalf("AssignSlot without /dev/ prefix: %v", err)
	}
	pool, _ := s.GetPool("media")
	for _, sl := range pool.Slots {
		if sl.SlotName == SlotHDD && sl.ArrayPath != "/dev/md0" {
			t.Errorf("array path = %q, want /dev/md0", sl.ArrayPath)
		}
	}
}

// --- ClearSlot ---

func TestClearSlot_Success(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	s.AssignSlot("media", SlotHDD, "/dev/md0", "/dev/md0")

	if err := s.ClearSlot("media", SlotHDD); err != nil {
		t.Fatalf("ClearSlot: %v", err)
	}
	pool, _ := s.GetPool("media")
	for _, sl := range pool.Slots {
		if sl.SlotName == SlotHDD {
			if sl.State != SlotStateEmpty {
				t.Errorf("slot state = %q, want %q", sl.State, SlotStateEmpty)
			}
			if sl.ArrayPath != "" {
				t.Errorf("array path = %q, want empty", sl.ArrayPath)
			}
		}
	}
}

func TestClearSlot_NotFound(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	if err := s.ClearSlot("media", "BOGUS"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// --- UpdateSlotState ---

func TestUpdateSlotState_ValidTransition(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	s.AssignSlot("media", SlotHDD, "/dev/md0", "/dev/md0")

	if err := s.UpdateSlotState("media", SlotHDD, SlotStateDegraded); err != nil {
		t.Fatalf("UpdateSlotState: %v", err)
	}
	pool, _ := s.GetPool("media")
	for _, sl := range pool.Slots {
		if sl.SlotName == SlotHDD && sl.State != SlotStateDegraded {
			t.Errorf("state = %q, want %q", sl.State, SlotStateDegraded)
		}
	}
}

func TestUpdateSlotState_InvalidTransition(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	// NVME slot starts empty; empty → missing is not a valid transition.
	if err := s.UpdateSlotState("media", SlotNVME, SlotStateMissing); err == nil {
		t.Fatal("expected error for invalid slot state transition")
	}
}

func TestUpdateSlotState_NoopIfSameState(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	// empty → empty should be a no-op (no error).
	if err := s.UpdateSlotState("media", SlotHDD, SlotStateEmpty); err != nil {
		t.Fatalf("UpdateSlotState same→same: %v", err)
	}
}

// --- UpdatePoolState ---

func TestUpdatePoolState_ValidTransition(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())

	if err := s.UpdatePoolState("media", PoolStateHealthy, ""); err != nil {
		t.Fatalf("UpdatePoolState: %v", err)
	}
	pool, _ := s.GetPool("media")
	if pool.State != PoolStateHealthy {
		t.Errorf("state = %q, want %q", pool.State, PoolStateHealthy)
	}
}

func TestUpdatePoolState_InvalidTransition(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	// provisioning → unmounted is not valid.
	if err := s.UpdatePoolState("media", PoolStateUnmounted, ""); err == nil {
		t.Fatal("expected error for invalid pool state transition")
	}
}

// --- SetPoolError ---

func TestSetPoolError_Success(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())

	if err := s.SetPoolError("media", "disk failed"); err != nil {
		t.Fatalf("SetPoolError: %v", err)
	}
	pool, _ := s.GetPool("media")
	if pool.State != PoolStateError {
		t.Errorf("state = %q, want %q", pool.State, PoolStateError)
	}
	if pool.ErrorReason != "disk failed" {
		t.Errorf("error_reason = %q, want %q", pool.ErrorReason, "disk failed")
	}
}

func TestSetPoolError_EmptyReasonRejected(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	if err := s.SetPoolError("media", ""); err == nil {
		t.Fatal("expected error for empty reason")
	}
}

// --- DeletePool ---

func TestDeletePool_RemovesFromCache(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())

	s.DeletePool("media")

	if _, err := s.GetPool("media"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound after delete, got %v", err)
	}
}

func TestDeletePool_NonExistentIsNoOp(t *testing.T) {
	s := newTestStore()
	// Must not panic.
	s.DeletePool("ghost")
}

func TestDeletePool_CallsRemoveLV(t *testing.T) {
	var removed []string
	s := newTestStore()
	s.removeLV = func(vg, name string) error {
		removed = append(removed, name)
		return nil
	}

	// Seed pool with lvExists=false (default) so AssignSlot skips disk writes.
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	s.AssignSlot("media", SlotHDD, "/dev/md0", "/dev/md0")

	// Now make lvExists report true so DeletePool will call removeLV.
	s.lvExists = func(vg, name string) (bool, error) { return true, nil }

	s.DeletePool("media")

	// Expect tiermeta and tiermeta_complete to have been removed from the HDD VG.
	found := map[string]bool{}
	for _, n := range removed {
		found[n] = true
	}
	if !found[TierLVName] {
		t.Errorf("expected %q to be removed, got %v", TierLVName, removed)
	}
	if !found[CompleteLVName] {
		t.Errorf("expected %q to be removed, got %v", CompleteLVName, removed)
	}
}

// --- EnsureArray ---

func TestEnsureArray_StableID(t *testing.T) {
	s := newTestStore()
	id1 := s.EnsureArray("/dev/md0")
	id2 := s.EnsureArray("/dev/md0")
	if id1 != id2 {
		t.Errorf("EnsureArray returned different IDs: %d vs %d", id1, id2)
	}
}

func TestEnsureArray_UniqueIDs(t *testing.T) {
	s := newTestStore()
	id1 := s.EnsureArray("/dev/md0")
	id2 := s.EnsureArray("/dev/md1")
	if id1 == id2 {
		t.Errorf("different arrays got the same ID %d", id1)
	}
}

func TestEnsureArray_NormalizesPath(t *testing.T) {
	s := newTestStore()
	id1 := s.EnsureArray("md0")     // without /dev/
	id2 := s.EnsureArray("/dev/md0") // with /dev/
	if id1 != id2 {
		t.Errorf("normalized paths should share an ID: %d vs %d", id1, id2)
	}
}

// --- GetSlotByArrayPath ---

func TestGetSlotByArrayPath_Found(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	s.AssignSlot("media", SlotHDD, "/dev/md0", "/dev/md0")

	slot, err := s.GetSlotByArrayPath("/dev/md0")
	if err != nil {
		t.Fatalf("GetSlotByArrayPath: %v", err)
	}
	if slot.SlotName != SlotHDD {
		t.Errorf("slot name = %q, want %q", slot.SlotName, SlotHDD)
	}
}

func TestGetSlotByArrayPath_NotFound(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())

	_, err := s.GetSlotByArrayPath("/dev/md99")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// --- MarkReconciled ---

func TestMarkReconciled_SetsTimestamp(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())

	pool, _ := s.GetPool("media")
	if pool.LastReconciledAt != nil {
		t.Fatal("expected nil LastReconciledAt before mark")
	}

	if err := s.MarkReconciled("media"); err != nil {
		t.Fatalf("MarkReconciled: %v", err)
	}

	pool, _ = s.GetPool("media")
	if pool.LastReconciledAt == nil {
		t.Fatal("expected non-nil LastReconciledAt after mark")
	}
}

// --- SetDestroyingReason ---

func TestSetDestroyingReason_RequiresDestroyingState(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	// Pool is in provisioning, not destroying.
	if err := s.SetDestroyingReason("media", "partial teardown"); err == nil {
		t.Fatal("expected error when pool is not in destroying state")
	}
}

func TestSetDestroyingReason_Success(t *testing.T) {
	s := newTestStore()
	s.CreatePool("media", "xfs", DefaultTierDefinitions())
	s.UpdatePoolState("media", PoolStateDestroying, "")

	if err := s.SetDestroyingReason("media", "lvremove failed"); err != nil {
		t.Fatalf("SetDestroyingReason: %v", err)
	}
	pool, _ := s.GetPool("media")
	if pool.ErrorReason != "lvremove failed" {
		t.Errorf("error_reason = %q, want %q", pool.ErrorReason, "lvremove failed")
	}
}
