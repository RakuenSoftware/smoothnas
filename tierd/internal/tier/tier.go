package tier

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiermeta"
)

var (
	listPVsInVG         = lvm.ListPVsInVG
	listManagedPVs      = lvm.ListManagedPVs
	lookupPV            = lvm.LookupPV
	wipeSignatures      = lvm.WipeSignatures
	createPV            = lvm.CreatePV
	addPVTags           = lvm.AddPVTags
	ensureVG            = lvm.EnsureVG
	vgReduce            = lvm.VGReduce
	vgRemove            = lvm.VGRemove
	vgRemoveIfEmpty     = lvm.VGRemoveIfEmpty
	removePVLabel       = lvm.RemovePV
	removeVGPlaceholder = lvm.VGRemovePlaceholder
	lvExists            = lvm.LVExists
	lvHealthy           = lvm.LVHealthy
	createLVOnPVs       = lvm.CreateLVOnDevices
	extendLVOnPV        = lvm.ExtendLV
	pvResize            = lvm.PVResize
	repairFilesystem    = lvm.RepairFilesystem
	formatLV            = lvm.FormatLV
	growFilesystem      = lvm.GrowFilesystem
	ensureFSTabEntry    = lvm.EnsureFSTabEntry
	verifyLVSegments    = lvm.VerifyLVSegmentOrder
	isMounted           = lvm.IsMounted
	mountedByDev        = lvm.MountedByDevice
	mountLV             = lvm.Mount
	unmountLV           = lvm.Unmount
	removeLV            = lvm.RemoveLV
	listLVSegments      = lvm.ListLVSegments
	vgExtentSizeBytes   = lvm.VGExtentSizeBytes
	lvSizeBytes         = lvm.LVSizeBytes
	mkdirAll            = os.MkdirAll
)

// Manager coordinates named tier instances and their dedicated LVM backing.
type Manager struct {
	store *db.Store
	meta  *tiermeta.Store // optional; nil means no LV-backed metadata
}

func NewManager(store *db.Store) *Manager {
	return &Manager{store: store}
}

// SetMetaStore attaches a tiermeta.Store for write-through LV-backed metadata.
func (m *Manager) SetMetaStore(meta *tiermeta.Store) {
	m.meta = meta
}

func tierVGName(tierName string) string {
	return "tier-" + tierName
}

func deviceRanks(assignments []db.TierArrayAssignment) map[string]int {
	ranks := make(map[string]int, len(assignments))
	for _, assignment := range assignments {
		ranks[assignment.ArrayPath] = assignment.Rank
	}
	return ranks
}

func orderedDevices(assignments []db.TierArrayAssignment) []string {
	devices := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		devices = append(devices, assignment.ArrayPath)
	}
	return devices
}

func rollbackAssignedPVs(vg string, assignments []db.TierArrayAssignment) {
	for i := len(assignments) - 1; i >= 0; i-- {
		_ = vgReduce(vg, assignments[i].ArrayPath)
		_ = removePVLabel(assignments[i].ArrayPath)
	}
}

// PerTierVGName returns the VG name for a specific tier within a pool.
// Format: tier-{poolName}-{tierName}, e.g. "tier-media-NVME".
func PerTierVGName(poolName, tierName string) string {
	return "tier-" + poolName + "-" + tierName
}

// PerTierBackingMount returns the backing mount path for a specific tier.
// The path is outside /mnt/{pool} so it is not shadowed by the FUSE mount.
func PerTierBackingMount(poolName, tierName string) string {
	return "/mnt/.tierd-backing/" + poolName + "/" + tierName
}

// ProvisionPerTierStorage creates an independent VG/LV for a single tier slot.
// Unlike ProvisionStorage (which creates a monolithic LV spanning all PVs),
// this creates a dedicated VG and LV per tier so each tier's I/O is isolated.
// The FUSE daemon then routes file opens to the correct tier.
func (m *Manager) ProvisionPerTierStorage(poolName, tierName string) error {
	pool, err := m.store.GetTierInstance(poolName)
	if err != nil {
		return fmt.Errorf("get tier pool: %w", err)
	}

	slot, err := m.store.GetTierSlot(poolName, tierName)
	if err != nil {
		return fmt.Errorf("get tier slot %s/%s: %w", poolName, tierName, err)
	}
	if slot.State == db.TierSlotStateEmpty || slot.ArrayPath == "" {
		return fmt.Errorf("tier slot %s/%s has no array assigned", poolName, tierName)
	}

	vg := PerTierVGName(poolName, tierName)
	mountPoint := PerTierBackingMount(poolName, tierName)
	const lvName = "data"

	// Check if the VG already exists (idempotent).
	if ok, _ := lvExists(vg, lvName); ok {
		// Already provisioned — ensure it's mounted.
		if !isMounted(mountPoint) {
			_ = os.MkdirAll(mountPoint, 0755)
			if err := mountLV(vg, lvName, mountPoint); err != nil {
				// "Structure needs cleaning" (EUCLEAN) means the filesystem was
				// left dirty — run fsck to repair it and retry the mount once.
				if strings.Contains(err.Error(), "needs cleaning") {
					if fsckErr := repairFilesystem(vg, lvName); fsckErr != nil {
						return fmt.Errorf("re-mount %s: filesystem repair failed: %w", mountPoint, fsckErr)
					}
					if err2 := mountLV(vg, lvName, mountPoint); err2 != nil {
						return fmt.Errorf("re-mount %s (after fsck): %w", mountPoint, err2)
					}
				} else {
					return fmt.Errorf("re-mount %s: %w", mountPoint, err)
				}
			}
		}
		return nil
	}

	// Prepare the PV.
	pv, err := lookupPV(slot.ArrayPath)
	if err != nil {
		return fmt.Errorf("lookup pv %s: %w", slot.ArrayPath, err)
	}
	if pv != nil && pv.VGName != "" && pv.VGName != vg {
		_ = removePVLabel(slot.ArrayPath)
		_ = wipeSignatures(slot.ArrayPath)
		pv = nil
	}
	if pv == nil {
		if err := wipeSignatures(slot.ArrayPath); err != nil {
			return fmt.Errorf("wipefs %s: %w", slot.ArrayPath, err)
		}
		if err := createPV(slot.ArrayPath); err != nil {
			return fmt.Errorf("pvcreate %s: %w", slot.ArrayPath, err)
		}
	}

	// Create dedicated VG for this tier.
	if err := ensureVG(vg, slot.ArrayPath); err != nil {
		return fmt.Errorf("ensure vg %s: %w", vg, err)
	}
	if err := addPVTags(slot.ArrayPath, poolName, tierName); err != nil {
		log.Printf("tier provision: tag pv %s: %v", slot.ArrayPath, err)
	}

	// Carve out the tiermeta LV before allocating the data LV so there is
	// guaranteed free space.  The meta LV is sized at 0.1% of the PV.
	// Non-fatal: if this fails the data LV still gets created.
	if m.meta != nil {
		pvInfos, _ := listPVsInVG(vg)
		var pvSizeBytes uint64
		for _, p := range pvInfos {
			if p.Device == slot.ArrayPath {
				pvSizeBytes = p.SizeBytes
				break
			}
		}
		if err := m.meta.CreateSlotMetaLV(poolName, tierName, slot.ArrayPath, pvSizeBytes); err != nil {
			log.Printf("tier provision: create meta lv for %s/%s: %v", poolName, tierName, err)
		}
	}

	// Create data LV using all remaining available space.
	createArgs := []string{slot.ArrayPath}
	if err := createLVOnPVs(vg, lvName, "100%FREE", createArgs); err != nil {
		return fmt.Errorf("create lv %s/%s: %w", vg, lvName, err)
	}

	// Format.
	if err := formatLV(vg, lvName, pool.Filesystem); err != nil {
		_ = removeLV(vg, lvName)
		_ = vgRemove(vg)
		return fmt.Errorf("format lv: %w", err)
	}

	// Mount.
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	if err := mountLV(vg, lvName, mountPoint); err != nil {
		return fmt.Errorf("mount lv: %w", err)
	}
	if err := ensureFSTabEntry(vg, lvName, mountPoint, pool.Filesystem); err != nil {
		log.Printf("tier provision: fstab entry for %s/%s: %v", vg, lvName, err)
	}

	log.Printf("tier provision: per-tier storage ready at %s (vg=%s)", mountPoint, vg)

	// Record the slot assignment in the meta store.
	if m.meta != nil {
		if err := m.meta.AssignSlot(poolName, tierName, slot.ArrayPath, slot.ArrayPath); err != nil {
			// May already be assigned if pool was pre-seeded from Bootstrap.
			log.Printf("tier provision: meta assign slot %s/%s: %v", poolName, tierName, err)
		}
	}

	return nil
}

// Reconcile is the boot-time reconciliation pass. It discovers all managed PVs
// via LVM tags, cross-references them with the DB, updates slot/pool states for
// any missing PVs, mounts healthy and accessible degraded pools, ensures fstab
// entries, runs segment verification, and stamps last_reconciled_at.
func (m *Manager) Reconcile() {
	managedPVs, err := listManagedPVs()
	if err != nil {
		log.Printf("tier reconcile: list managed pvs: %v", err)
	}
	discoveredByPool := make(map[string]map[string]struct{})
	for _, pv := range managedPVs {
		if discoveredByPool[pv.PoolName] == nil {
			discoveredByPool[pv.PoolName] = make(map[string]struct{})
		}
		discoveredByPool[pv.PoolName][pv.Device] = struct{}{}
	}

	pools, err := m.store.ListTierInstances()
	if err != nil {
		log.Printf("tier reconcile: list instances: %v", err)
		return
	}
	for _, pool := range pools {
		m.reconcilePool(pool, discoveredByPool[pool.Name])
	}
}

func (m *Manager) reconcilePool(pool db.TierInstance, discoveredPVs map[string]struct{}) {
	if pool.State == db.TierPoolStateDestroying {
		return
	}

	assignments, err := m.store.GetTierAssignments(pool.Name)
	if err != nil {
		log.Printf("tier reconcile: get assignments for %s: %v", pool.Name, err)
		return
	}

	// Reconcile slot states against discovered PVs.
	anyMissing := false
	anyRestored := false
	for _, a := range assignments {
		if a.ArrayPath == "" {
			continue
		}
		_, found := discoveredPVs[a.ArrayPath]
		switch a.State {
		case db.TierSlotStateAssigned, db.TierSlotStateDegraded:
			if !found {
				if err := m.store.TransitionTierSlotState(pool.Name, a.Slot, db.TierSlotStateMissing); err != nil {
					log.Printf("tier reconcile: mark slot %s/%s missing: %v", pool.Name, a.Slot, err)
				}
				anyMissing = true
			}
		case db.TierSlotStateMissing:
			if found {
				if err := m.store.TransitionTierSlotState(pool.Name, a.Slot, db.TierSlotStateAssigned); err != nil {
					log.Printf("tier reconcile: restore slot %s/%s: %v", pool.Name, a.Slot, err)
				} else {
					anyRestored = true
				}
			} else {
				anyMissing = true
			}
		}
	}

	// Update pool state to reflect slot changes.
	if anyMissing && pool.State == db.TierPoolStateHealthy {
		if err := m.store.TransitionTierInstanceState(pool.Name, db.TierPoolStateDegraded); err != nil {
			log.Printf("tier reconcile: degrade pool %s: %v", pool.Name, err)
		} else {
			pool.State = db.TierPoolStateDegraded
		}
	} else if !anyMissing && anyRestored && pool.State == db.TierPoolStateDegraded {
		if err := m.store.TransitionTierInstanceState(pool.Name, db.TierPoolStateHealthy); err != nil {
			log.Printf("tier reconcile: restore pool %s: %v", pool.Name, err)
		} else {
			pool.State = db.TierPoolStateHealthy
		}
	}

	if pool.State != db.TierPoolStateHealthy && pool.State != db.TierPoolStateDegraded {
		_ = m.store.MarkTierReconciled(pool.Name)
		return
	}

	vg := tierVGName(pool.Name)
	const lvName = "data"
	mountPoint := db.TierMountPoint(pool.Name)

	ok, err := lvExists(vg, lvName)
	if err != nil || !ok {
		log.Printf("tier reconcile: LV %s/%s not found, skipping", vg, lvName)
		_ = m.store.MarkTierReconciled(pool.Name)
		return
	}

	// Validate the mount path.
	if info, statErr := os.Stat(mountPoint); statErr == nil && !info.IsDir() {
		log.Printf("tier reconcile: %s exists as a file, cannot mount", mountPoint)
		_ = m.store.SetTierInstanceError(pool.Name, "mount_path_is_file")
		_ = m.store.MarkTierReconciled(pool.Name)
		return
	}

	expectedDev := "/dev/" + vg + "/" + lvName
	if isMounted(mountPoint) {
		if existing := mountedByDev(mountPoint); existing != "" && existing != expectedDev {
			log.Printf("tier reconcile: %s mounted by %s, expected %s", mountPoint, existing, expectedDev)
			_ = m.store.SetTierInstanceError(pool.Name, "mount_path_conflict")
			_ = m.store.MarkTierReconciled(pool.Name)
			return
		}
		// Already mounted correctly — fall through to fstab + segment verify.
	} else {
		// Degraded pools: only mount when the LV has no missing extents.
		if pool.State == db.TierPoolStateDegraded {
			healthy, err := lvHealthy(vg, lvName)
			if err != nil || !healthy {
				log.Printf("tier reconcile: degraded pool %s LV not healthy, leaving unmounted", pool.Name)
				_ = m.store.SetTierExpansionError(pool.Name, "degraded_unsafe_to_mount")
				_ = m.store.MarkTierReconciled(pool.Name)
				return
			}
		}
		if err := os.MkdirAll(mountPoint, 0755); err != nil {
			log.Printf("tier reconcile: mkdir %s: %v", mountPoint, err)
			_ = m.store.MarkTierReconciled(pool.Name)
			return
		}
		if err := mountLV(vg, lvName, mountPoint); err != nil {
			log.Printf("tier reconcile: mount %s: %v", pool.Name, err)
			_ = m.store.MarkTierReconciled(pool.Name)
			return
		}
	}

	// Ensure fstab entry.
	if err := ensureFSTabEntry(vg, lvName, mountPoint, pool.Filesystem); err != nil {
		log.Printf("tier reconcile: ensure fstab %s: %v", pool.Name, err)
	}

	// Segment verification.
	if err := verifyLVSegments(vg, lvName, deviceRanks(assignments)); err != nil {
		log.Printf("tier reconcile: segment verify %s: %v", pool.Name, err)
		_ = m.store.SetTierInstanceError(pool.Name, "segment_order_violation")
	}

	_ = m.store.MarkTierReconciled(pool.Name)
}

// ExpandStorageForArray is called when an mdadm array backing a pool tier has
// grown. It runs pvresize → lvextend → growfs → segment verify and updates
// updated_at on success. On any failure it records error_reason without
// changing pool state so the LV remains functional at its prior size.
func (m *Manager) ExpandStorageForArray(arrayPath string) {
	assignment, err := m.store.GetTierAssignmentByArrayPath(arrayPath)
	if err != nil {
		if err != db.ErrNotFound {
			log.Printf("tier expand: lookup array %s: %v", arrayPath, err)
		}
		return
	}

	poolName := assignment.TierName // TierName holds the pool name
	pvDevice := assignment.ArrayPath
	vg := tierVGName(poolName)
	const lvName = "data"

	pool, err := m.store.GetTierInstance(poolName)
	if err != nil {
		log.Printf("tier expand: get pool %s: %v", poolName, err)
		return
	}

	setStepError := func(step string) {
		reason := "auto_expansion_failed: " + step
		if err := m.store.SetTierExpansionError(poolName, reason); err != nil {
			log.Printf("tier expand: record error for %s: %v", poolName, err)
		}
	}

	if err := pvResize(pvDevice); err != nil {
		log.Printf("tier expand: pvresize %s: %v", pvDevice, err)
		setStepError("pvresize")
		return
	}

	if err := extendLVOnPV(vg, lvName, "+100%FREE", pvDevice); err != nil {
		log.Printf("tier expand: lvextend %s/%s on %s: %v", vg, lvName, pvDevice, err)
		setStepError("lvextend")
		return
	}

	mountPoint := db.TierMountPoint(poolName)
	if err := growFilesystem(vg, lvName, mountPoint, pool.Filesystem); err != nil {
		log.Printf("tier expand: growfs %s: %v", poolName, err)
		setStepError("growfs")
		return
	}

	assignments, err := m.store.GetTierAssignments(poolName)
	if err != nil {
		log.Printf("tier expand: get assignments %s: %v", poolName, err)
		setStepError("segment_verify")
		return
	}
	if err := verifyLVSegments(vg, lvName, deviceRanks(assignments)); err != nil {
		log.Printf("tier expand: segment verify %s: %v", poolName, err)
		_ = m.store.SetTierInstanceError(poolName, "segment_order_violation")
		return
	}

	if err := m.store.TouchTierPool(poolName); err != nil {
		log.Printf("tier expand: touch pool %s: %v", poolName, err)
	}
}

// TeardownStorage unmounts and destroys the LVM backing for a named tier.
func (m *Manager) TeardownStorage(tierName string) error {
	mountPoint := db.TierMountPoint(tierName)
	if isMounted(mountPoint) {
		if err := unmountLV(mountPoint); err != nil {
			return fmt.Errorf("unmount %s: %w", mountPoint, err)
		}
	}

	vg := tierVGName(tierName)
	const lvName = "data"
	if exists, _ := lvExists(vg, lvName); exists {
		if err := removeLV(vg, lvName); err != nil {
			return fmt.Errorf("remove lv: %w", err)
		}
	}

	pvs, _ := listPVsInVG(vg)
	if len(pvs) > 0 {
		if err := vgRemove(vg); err == nil {
			for _, pv := range pvs {
				_ = removePVLabel(pv.Device)
			}
			return nil
		}
		return fmt.Errorf("remove vg %s: still has %d PVs", vg, len(pvs))
	}
	_ = vgRemoveIfEmpty(vg)
	return nil
}
