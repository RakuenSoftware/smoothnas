package tier

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier/backend"
)

func newTestManager(t *testing.T, assignments map[string]string) *Manager {
	t.Helper()

	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	store, err := db.Open(filepath.Join(t.TempDir(), "tiers.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier instance: %v", err)
	}
	for slot, array := range assignments {
		if err := store.AddArrayToTierSlot("media", slot, array); err != nil {
			t.Fatalf("add array %s to %s: %v", array, slot, err)
		}
	}

	return NewManager(store)
}

func stubTierLVM(t *testing.T) {
	t.Helper()

	origListPVsInVG := listPVsInVG
	origLookupPV := lookupPV
	origWipeSignatures := wipeSignatures
	origCreatePV := createPV
	origAddPVTags := addPVTags
	origEnsureVG := ensureVG
	origVGRemove := vgRemove
	origVGRemoveIfEmpty := vgRemoveIfEmpty
	origRemovePVLabel := removePVLabel
	origLVExists := lvExists
	origCreateLVOnPVs := createLVOnPVs
	origExtendLVOnPV := extendLVOnPV
	origRepairFilesystem := repairFilesystem
	origFormatLV := formatLV
	origGrowFilesystem := growFilesystem
	origVerifyLVSegments := verifyLVSegments
	origIsMounted := isMounted
	origMountLV := mountLV
	origUnmountLV := unmountLV
	origRemoveLV := removeLV
	origMountedByDev := mountedByDev
	origListLVSegments := listLVSegments
	origVGExtentSizeBytes := vgExtentSizeBytes
	origLVSizeBytes := lvSizeBytes
	origMkdirAll := mkdirAll

	mountedByDev = func(string) string { return "" }
	listLVSegments = func(string, string) ([]lvm.Segment, error) { return nil, nil }
	vgExtentSizeBytes = func(string) (uint64, error) { return 4 << 20, nil }
	lvSizeBytes = func(string, string) (uint64, error) { return 0, nil }
	mkdirAll = func(string, os.FileMode) error { return nil }

	listPVsInVG = func(string) ([]lvm.PVInfo, error) { return nil, nil }
	lookupPV = func(string) (*lvm.PVLookup, error) { return nil, nil }
	wipeSignatures = func(string) error { return nil }
	createPV = func(string) error { return nil }
	addPVTags = func(string, string, string) error { return nil }
	ensureVG = func(string, string) error { return nil }
	vgRemove = func(string) error { return nil }
	vgRemoveIfEmpty = func(string) error { return nil }
	removePVLabel = func(string) error { return nil }
	lvExists = func(string, string) (bool, error) { return false, nil }
	createLVOnPVs = func(string, string, string, []string) error { return nil }
	extendLVOnPV = func(string, string, string, string) error { return nil }
	repairFilesystem = func(string, string) error { return nil }
	formatLV = func(string, string, string) error { return nil }
	growFilesystem = func(string, string, string, string) error { return nil }
	verifyLVSegments = func(string, string, map[string]int) error { return nil }
	isMounted = func(string) bool { return true }
	mountLV = func(string, string, string) error { return nil }
	unmountLV = func(string) error { return nil }
	removeLV = func(string, string) error { return nil }

	// The mdadm provision path now lives in tier/backend/mdadm.go and uses
	// its own function-var shims. Redirect them at the same stubs so tests
	// that call ProvisionPerTierStorage see a fully faked LVM layer.
	resetBackend := (&backend.MdadmHooks{
		LookupPV:         func(string) (*lvm.PVLookup, error) { return nil, nil },
		WipeSignatures:   func(string) error { return nil },
		CreatePV:         func(string) error { return nil },
		EnsureVG:         func(string, string) error { return nil },
		AddPVTags:        func(string, string, string) error { return nil },
		ListPVsInVG:      func(string) ([]lvm.PVInfo, error) { return nil, nil },
		LVExists:         func(string, string) (bool, error) { return false, nil },
		CreateLVOnDevs:   func(string, string, string, []string) error { return nil },
		FormatLV:         func(string, string, string) error { return nil },
		RemoveLV:         func(string, string) error { return nil },
		VGRemove:         func(string) error { return nil },
		Mount:            func(string, string, string) error { return nil },
		IsMounted:        func(string) bool { return true },
		EnsureFSTabEntry: func(string, string, string, string) error { return nil },
		RepairFilesystem: func(string, string) error { return nil },
		MkdirAll:         func(string, os.FileMode) error { return nil },
	}).Install()
	t.Cleanup(resetBackend)

	t.Cleanup(func() {
		listPVsInVG = origListPVsInVG
		lookupPV = origLookupPV
		wipeSignatures = origWipeSignatures
		createPV = origCreatePV
		addPVTags = origAddPVTags
		ensureVG = origEnsureVG
		vgRemove = origVGRemove
		vgRemoveIfEmpty = origVGRemoveIfEmpty
		removePVLabel = origRemovePVLabel
		lvExists = origLVExists
		createLVOnPVs = origCreateLVOnPVs
		extendLVOnPV = origExtendLVOnPV
		repairFilesystem = origRepairFilesystem
		formatLV = origFormatLV
		growFilesystem = origGrowFilesystem
		verifyLVSegments = origVerifyLVSegments
		isMounted = origIsMounted
		mountLV = origMountLV
		unmountLV = origUnmountLV
		removeLV = origRemoveLV
		mountedByDev = origMountedByDev
		listLVSegments = origListLVSegments
		vgExtentSizeBytes = origVGExtentSizeBytes
		lvSizeBytes = origLVSizeBytes
		mkdirAll = origMkdirAll
	})
}

func TestTeardownStorageRemovesVGThenPVLabels(t *testing.T) {
	m := newTestManager(t, map[string]string{
		db.TierSlotNVME: "md1",
		db.TierSlotHDD:  "md0",
	})
	stubTierLVM(t)

	var removedVGs []string
	var removedPVs []string

	lvExists = func(vg, name string) (bool, error) { return true, nil }
	listPVsInVG = func(vg string) ([]lvm.PVInfo, error) {
		if vg != tierVGName("media") {
			return nil, fmt.Errorf("unexpected vg %s", vg)
		}
		return []lvm.PVInfo{
			{Device: "/dev/md1"},
			{Device: "/dev/md0"},
		}, nil
	}
	vgRemove = func(vg string) error {
		removedVGs = append(removedVGs, vg)
		return nil
	}
	removePVLabel = func(device string) error {
		removedPVs = append(removedPVs, device)
		return nil
	}

	if err := m.TeardownStorage("media"); err != nil {
		t.Fatalf("TeardownStorage: %v", err)
	}

	if !reflect.DeepEqual(removedVGs, []string{"tier-media"}) {
		t.Fatalf("removed VGs = %v, want [tier-media]", removedVGs)
	}
	if !reflect.DeepEqual(removedPVs, []string{"/dev/md1", "/dev/md0"}) {
		t.Fatalf("removed PV labels = %v, want [/dev/md1 /dev/md0]", removedPVs)
	}
}

func TestProvisionPerTierStorageRepairsDirtyFilesystem(t *testing.T) {
	// newTestManager already creates a "media" pool with default slots.
	m := newTestManager(t, map[string]string{"HDD": "/dev/md1"})
	stubTierLVM(t)

	// The provision path now runs inside tier/backend/mdadm.go, so
	// override the backend's hooks (which stubTierLVM seeded with
	// no-op defaults above). Any field left unset reverts to whatever
	// stubTierLVM installed.
	mountAttempts := 0
	var fsckCalled bool
	reset := (&backend.MdadmHooks{
		LVExists:  func(string, string) (bool, error) { return true, nil },
		IsMounted: func(string) bool { return false },
		Mount: func(vg, name, mp string) error {
			mountAttempts++
			if mountAttempts == 1 {
				return fmt.Errorf("mount: %s: needs cleaning: exit status 32", mp)
			}
			return nil
		},
		RepairFilesystem: func(vg, name string) error {
			fsckCalled = true
			return nil
		},
	}).Install()
	t.Cleanup(reset)

	if err := m.ProvisionPerTierStorage("media", "HDD"); err != nil {
		t.Fatalf("ProvisionPerTierStorage: %v", err)
	}
	if !fsckCalled {
		t.Fatal("expected RepairFilesystem to be called on dirty filesystem")
	}
	if mountAttempts != 2 {
		t.Fatalf("expected 2 mount attempts (fail + retry), got %d", mountAttempts)
	}
}
