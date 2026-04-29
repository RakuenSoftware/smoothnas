package backend

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
)

// Function variables wrap every LVM side effect so tests can stub
// them without monkey-patching the lvm package. Mirrors the pattern
// previously used in the tier package before this code moved here.
var (
	mdLookupPV         = lvm.LookupPV
	mdRemovePV         = lvm.RemovePV
	mdWipeSignatures   = lvm.WipeSignatures
	mdCreatePV         = lvm.CreatePV
	mdEnsureVG         = lvm.EnsureVG
	mdAddPVTags        = lvm.AddPVTags
	mdListPVsInVG      = lvm.ListPVsInVG
	mdLVExists         = lvm.LVExists
	mdCreateLVOnDevs   = lvm.CreateLVOnDevices
	mdFormatLV         = lvm.FormatLV
	mdRemoveLV         = lvm.RemoveLV
	mdVGRemove         = lvm.VGRemove
	mdMount            = lvm.Mount
	mdIsMounted        = lvm.IsMounted
	mdEnsureFSTabEntry = lvm.EnsureFSTabEntry
	mdRepairFS         = repairFilesystem
	mdMkdirAll         = os.MkdirAll
)

// MdadmHooks is the collection of swap-able LVM ops the mdadm backend
// uses. Tests construct a hooks value with the fields they want to
// override, call Install(), and defer the returned reset. Zero-value
// fields keep the production function in place.
type MdadmHooks struct {
	LookupPV         func(string) (*lvm.PVLookup, error)
	RemovePV         func(string) error
	WipeSignatures   func(string) error
	CreatePV         func(string) error
	EnsureVG         func(vg, dev string) error
	AddPVTags        func(dev, pool, tier string) error
	ListPVsInVG      func(vg string) ([]lvm.PVInfo, error)
	LVExists         func(vg, lv string) (bool, error)
	CreateLVOnDevs   func(vg, lv, size string, devs []string) error
	FormatLV         func(vg, lv, fs string) error
	RemoveLV         func(vg, lv string) error
	VGRemove         func(vg string) error
	Mount            func(vg, lv, mount string) error
	IsMounted        func(path string) bool
	EnsureFSTabEntry func(vg, lv, mount, fs string) error
	RepairFilesystem func(vg, lv string) error
	MkdirAll         func(path string, perm os.FileMode) error
}

// Install overwrites the backend's live function vars with any non-
// nil field on the receiver and returns a reset func that restores
// the originals. Defer reset() from the test.
func (h *MdadmHooks) Install() (reset func()) {
	orig := struct {
		lookupPV         func(string) (*lvm.PVLookup, error)
		removePV         func(string) error
		wipeSignatures   func(string) error
		createPV         func(string) error
		ensureVG         func(vg, dev string) error
		addPVTags        func(dev, pool, tier string) error
		listPVsInVG      func(vg string) ([]lvm.PVInfo, error)
		lvExists         func(vg, lv string) (bool, error)
		createLVOnDevs   func(vg, lv, size string, devs []string) error
		formatLV         func(vg, lv, fs string) error
		removeLV         func(vg, lv string) error
		vgRemove         func(vg string) error
		mount            func(vg, lv, mount string) error
		isMounted        func(path string) bool
		ensureFSTabEntry func(vg, lv, mount, fs string) error
		repairFS         func(vg, lv string) error
		mkdirAll         func(path string, perm os.FileMode) error
	}{
		mdLookupPV, mdRemovePV, mdWipeSignatures, mdCreatePV,
		mdEnsureVG, mdAddPVTags, mdListPVsInVG, mdLVExists,
		mdCreateLVOnDevs, mdFormatLV, mdRemoveLV, mdVGRemove,
		mdMount, mdIsMounted, mdEnsureFSTabEntry, mdRepairFS, mdMkdirAll,
	}
	if h.LookupPV != nil {
		mdLookupPV = h.LookupPV
	}
	if h.RemovePV != nil {
		mdRemovePV = h.RemovePV
	}
	if h.WipeSignatures != nil {
		mdWipeSignatures = h.WipeSignatures
	}
	if h.CreatePV != nil {
		mdCreatePV = h.CreatePV
	}
	if h.EnsureVG != nil {
		mdEnsureVG = h.EnsureVG
	}
	if h.AddPVTags != nil {
		mdAddPVTags = h.AddPVTags
	}
	if h.ListPVsInVG != nil {
		mdListPVsInVG = h.ListPVsInVG
	}
	if h.LVExists != nil {
		mdLVExists = h.LVExists
	}
	if h.CreateLVOnDevs != nil {
		mdCreateLVOnDevs = h.CreateLVOnDevs
	}
	if h.FormatLV != nil {
		mdFormatLV = h.FormatLV
	}
	if h.RemoveLV != nil {
		mdRemoveLV = h.RemoveLV
	}
	if h.VGRemove != nil {
		mdVGRemove = h.VGRemove
	}
	if h.Mount != nil {
		mdMount = h.Mount
	}
	if h.IsMounted != nil {
		mdIsMounted = h.IsMounted
	}
	if h.EnsureFSTabEntry != nil {
		mdEnsureFSTabEntry = h.EnsureFSTabEntry
	}
	if h.RepairFilesystem != nil {
		mdRepairFS = h.RepairFilesystem
	}
	if h.MkdirAll != nil {
		mdMkdirAll = h.MkdirAll
	}
	return func() {
		mdLookupPV = orig.lookupPV
		mdRemovePV = orig.removePV
		mdWipeSignatures = orig.wipeSignatures
		mdCreatePV = orig.createPV
		mdEnsureVG = orig.ensureVG
		mdAddPVTags = orig.addPVTags
		mdListPVsInVG = orig.listPVsInVG
		mdLVExists = orig.lvExists
		mdCreateLVOnDevs = orig.createLVOnDevs
		mdFormatLV = orig.formatLV
		mdRemoveLV = orig.removeLV
		mdVGRemove = orig.vgRemove
		mdMount = orig.mount
		mdIsMounted = orig.isMounted
		mdEnsureFSTabEntry = orig.ensureFSTabEntry
		mdRepairFS = orig.repairFS
		mdMkdirAll = orig.mkdirAll
	}
}

// mdadmBackend provisions tier slot storage as an LVM data LV layered
// on top of a dedicated per-tier VG, in turn built on top of a PV on
// the mdadm array passed as ref.
//
// Steps (all idempotent):
//   1. If the VG+LV already exist, just re-mount (with fsck retry on EUCLEAN).
//   2. Otherwise: wipe → pvcreate → vgcreate → pvtag.
//   3. Optional: carve out the meta LV before the data LV so there is
//      always room (the meta LV is ~0.1% of the PV, but allocating
//      after a 100%FREE data LV would find zero space).
//   4. lvcreate 100%FREE → mkfs (filesystem per opts.Filesystem) →
//      mount → fstab entry.
//
// ref is the array path (e.g. "/dev/md0"). Single-device per-tier VGs
// only — multi-device is a follow-up.
type mdadmBackend struct{}

func init() { Register(&mdadmBackend{}) }

func (mdadmBackend) Kind() string { return "mdadm" }

// perTierVGName mirrors tier.PerTierVGName. Duplicated rather than
// imported to avoid an import cycle (tier imports backend).
func perTierVGName(poolName, tierName string) string {
	return "tier-" + poolName + "-" + tierName
}

const mdadmLVName = "data"

func (mdadmBackend) Provision(poolName, tierName, ref, mountPoint string, opts ProvisionOpts) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("mdadm backing requires array path (ref)")
	}
	fsKind := opts.Filesystem
	if fsKind == "" {
		fsKind = "xfs"
	}

	vg := perTierVGName(poolName, tierName)

	// Idempotent: if the LV exists, just make sure it's mounted.
	if ok, _ := mdLVExists(vg, mdadmLVName); ok {
		if !mdIsMounted(mountPoint) {
			if err := mdMkdirAll(mountPoint, 0o755); err != nil {
				return fmt.Errorf("create mount point: %w", err)
			}
			if err := mdMount(vg, mdadmLVName, mountPoint); err != nil {
				// "Structure needs cleaning" (EUCLEAN): the filesystem
				// was left dirty. Run fsck and retry once.
				if strings.Contains(err.Error(), "needs cleaning") {
					if fsckErr := mdRepairFS(vg, mdadmLVName); fsckErr != nil {
						return fmt.Errorf("re-mount %s: filesystem repair failed: %w", mountPoint, fsckErr)
					}
					if err2 := mdMount(vg, mdadmLVName, mountPoint); err2 != nil {
						return fmt.Errorf("re-mount %s (after fsck): %w", mountPoint, err2)
					}
				} else {
					return fmt.Errorf("re-mount %s: %w", mountPoint, err)
				}
			}
		}
		return nil
	}

	// Prepare the PV. If the target already belongs to some other VG
	// (leftover from a failed provision), scrub before re-use.
	pv, err := mdLookupPV(ref)
	if err != nil {
		return fmt.Errorf("lookup pv %s: %w", ref, err)
	}
	if pv != nil && pv.VGName != "" && pv.VGName != vg {
		_ = mdRemovePV(ref)
		_ = mdWipeSignatures(ref)
		pv = nil
	}
	if pv == nil {
		if err := mdWipeSignatures(ref); err != nil {
			return fmt.Errorf("wipefs %s: %w", ref, err)
		}
		if err := mdCreatePV(ref); err != nil {
			return fmt.Errorf("pvcreate %s: %w", ref, err)
		}
	}

	// Per-tier VG.
	if err := mdEnsureVG(vg, ref); err != nil {
		return fmt.Errorf("ensure vg %s: %w", vg, err)
	}
	if err := mdAddPVTags(ref, poolName, tierName); err != nil {
		log.Printf("tier provision: tag pv %s: %v", ref, err)
	}

	// Meta LV BEFORE data LV so the 100%FREE data allocation doesn't
	// leave zero room for metadata. Best-effort.
	if mdadmMetaProvider != nil {
		pvInfos, _ := mdListPVsInVG(vg)
		var pvSizeBytes uint64
		for _, p := range pvInfos {
			if p.Device == ref {
				pvSizeBytes = p.SizeBytes
				break
			}
		}
		if err := mdadmMetaProvider.CreateSlotMetaLV(poolName, tierName, ref, pvSizeBytes); err != nil {
			log.Printf("tier provision: create meta lv for %s/%s: %v", poolName, tierName, err)
		}
	}

	// Data LV on the remaining space.
	if err := mdCreateLVOnDevs(vg, mdadmLVName, "100%FREE", []string{ref}); err != nil {
		return fmt.Errorf("create lv %s/%s: %w", vg, mdadmLVName, err)
	}
	if err := mdFormatLV(vg, mdadmLVName, fsKind); err != nil {
		_ = mdRemoveLV(vg, mdadmLVName)
		_ = mdVGRemove(vg)
		return fmt.Errorf("format lv: %w", err)
	}

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	if err := mdMount(vg, mdadmLVName, mountPoint); err != nil {
		return fmt.Errorf("mount lv: %w", err)
	}
	if err := mdEnsureFSTabEntry(vg, mdadmLVName, mountPoint, fsKind); err != nil {
		log.Printf("tier provision: fstab entry for %s/%s: %v", vg, mdadmLVName, err)
	}
	return nil
}

// Destroy for mdadm stays with the pool-delete flow in the API layer —
// that path does baseline cleanup, SIGKILL-held-holder handling, VG
// sweep across orphaned devices, and fstab unregister in a fixed order.
// Per-slot Destroy here would duplicate all that without the context
// the pool delete has. Returning an error prevents accidental per-slot
// invocation.
func (mdadmBackend) Destroy(poolName, tierName, ref, mountPoint string) error {
	return fmt.Errorf("mdadm.Destroy is driven by the pool-delete flow, not per-slot")
}

// repairFilesystem runs fsck on the given LV. Tries xfs_repair first
// (xfs is the tier default); falls back to generic fsck -fy. Callers
// must unmount before calling.
func repairFilesystem(vg, lv string) error {
	dev := "/dev/" + vg + "/" + lv
	if err := exec.Command("xfs_repair", dev).Run(); err == nil {
		return nil
	}
	out, err := exec.Command("fsck", "-fy", dev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("fsck %s: %s: %w", dev, strings.TrimSpace(string(out)), err)
	}
	return nil
}
