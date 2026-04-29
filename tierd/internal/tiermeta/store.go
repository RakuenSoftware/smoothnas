package tiermeta

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a pool or slot is not in the store.
var ErrNotFound = fmt.Errorf("not found")

// Store is the in-memory metadata cache and the authority for all tier pool
// state.  Every mutating operation updates the RAM cache first, then persists
// to the appropriate on-disk metadata LVs.
//
// Disk layout per pool (per-tier VGs "tier-{pool}-{SLOT}"):
//   - tiermeta          — TierMeta for that slot (self-sufficient, includes pool context)
//   - tiermeta_complete — full PoolMeta on the slowest assigned slot's VG,
//     sized 0.1% of the sum of all slots' PVs
type Store struct {
	mu     sync.RWMutex
	pools  map[string]*PoolMeta // pool name → metadata (RAM cache)
	arrays map[string]int64     // array path → ephemeral integer ID
	nextID int64

	// Overridable for tests.
	createLV func(vg, name, size, pvDevice string) error
	removeLV func(vg, name string) error
	lvExists func(vg, name string) (bool, error)
}

// NewStore creates an empty store. Call Bootstrap() to load existing pools
// from on-disk metadata LVs before serving requests.
func NewStore() *Store {
	return &Store{
		pools:    make(map[string]*PoolMeta),
		arrays:   make(map[string]int64),
		createLV: lvmCreateLV,
		removeLV: lvmRemoveLV,
		lvExists: lvmLVExists,
	}
}

// --- LVM helpers (real implementations, overridable in tests) ---

func lvmCreateLV(vg, name, size, pvDevice string) error {
	args := []string{"-y", "-W", "y", "-Z", "y", "-L", size, "-n", name, vg}
	if pvDevice != "" {
		args = append(args, pvDevice)
	}
	cmd := exec.Command("lvcreate", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvcreate: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func lvmRemoveLV(vg, name string) error {
	dev := "/dev/" + vg + "/" + name
	cmd := exec.Command("lvremove", "-f", dev)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvremove %s: %s: %w", dev, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func lvmLVExists(vg, name string) (bool, error) {
	dev := "/dev/" + vg + "/" + name
	cmd := exec.Command("lvs", "--noheadings", "-o", "lv_name", dev)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// --- Bootstrap ---

// Bootstrap discovers all managed pools from LVM PV tags and reads their
// metadata LVs into the RAM cache.  Called once at daemon startup.
//
// Recovery order per pool:
//  1. Try to read tiermeta_complete from the slowest assigned PV.
//  2. Fall back to reading individual tiermeta_{SLOT} LVs.
//  3. If no metadata LVs exist yet (fresh upgrade), seed from LVM tags and
//     write the metadata LVs so they're present for future reads.
func (s *Store) Bootstrap() error {
	pvs, err := listManagedPVs()
	if err != nil {
		// Non-fatal: system may not have LVM installed.
		log.Printf("tiermeta bootstrap: list managed PVs: %v", err)
		return nil
	}

	// Group PVs by pool name.
	byPool := map[string][]managedPV{}
	for _, pv := range pvs {
		byPool[pv.PoolName] = append(byPool[pv.PoolName], pv)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for poolName, poolPVs := range byPool {
		// Sort PVs by rank (slowest last).
		sortByRank(poolPVs)
		slowest := poolPVs[len(poolPVs)-1]
		slowestVG := PerTierVGName(poolName, slowest.TierName)

		// 1. Try complete metadata from the slowest tier's VG.
		if exists, _ := s.lvExists(slowestVG, CompleteLVName); exists {
			if pool, err := ReadCompleteMeta(slowestVG); err == nil {
				s.pools[poolName] = pool
				continue
			}
			log.Printf("tiermeta bootstrap: read complete meta for %s: falling back to tier LVs", poolName)
		}

		// 2. Try individual per-tier LVs.
		pool := s.seedPoolFromPVs(poolName, poolPVs)
		s.pools[poolName] = pool

		// 3. Persist to LVs so future boots can use them.
		s.persistPoolLocked(pool, slowest.Device)
	}

	return nil
}

// managedPV is a thin view of an LVM PV carrying pool/tier tags.
type managedPV struct {
	Device   string
	PoolName string
	TierName string // NVME | SSD | HDD (from tag)
}

// rankForTier maps tier names to their default rank.
func rankForTier(name string) int {
	switch strings.ToUpper(name) {
	case SlotNVME:
		return 1
	case SlotSSD:
		return 2
	case SlotHDD:
		return 3
	default:
		return 99
	}
}

func sortByRank(pvs []managedPV) {
	for i := 1; i < len(pvs); i++ {
		for j := i; j > 0 && rankForTier(pvs[j].TierName) < rankForTier(pvs[j-1].TierName); j-- {
			pvs[j], pvs[j-1] = pvs[j-1], pvs[j]
		}
	}
}

// seedPoolFromPVs constructs a PoolMeta from available metadata LVs and LVM tags.
func (s *Store) seedPoolFromPVs(poolName string, pvs []managedPV) *PoolMeta {
	now := time.Now().UTC()
	pool := &PoolMeta{
		Version:    MetaVersion,
		Name:       poolName,
		Filesystem: "xfs",
		State:      PoolStateProvisioning,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	for _, pv := range pvs {
		tierVG := PerTierVGName(poolName, pv.TierName)
		// Try to read from that tier's own LV.
		if exists, _ := s.lvExists(tierVG, TierLVName); exists {
			if tm, err := ReadTierMeta(tierVG); err == nil {
				pool.Slots = append(pool.Slots, tm.Slot)
				// Absorb pool-level fields from the most-recently-updated tier.
				if tm.Namespace != nil && pool.Namespace == nil {
					pool.Namespace = tm.Namespace
				}
				if tm.RegionSizeMB > 0 && pool.RegionSizeMB == 0 {
					pool.RegionSizeMB = tm.RegionSizeMB
				}
				continue
			}
		}
		// Seed from tag info.
		pool.Slots = append(pool.Slots, SlotMeta{
			Version:   MetaVersion,
			PoolName:  poolName,
			SlotName:  strings.ToUpper(pv.TierName),
			Rank:      rankForTier(pv.TierName),
			State:     SlotStateAssigned,
			ArrayPath: pv.Device,
			PVDevice:  pv.Device,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	// Infer pool state: if any tier's data LV exists, the pool is healthy.
	for _, pv := range pvs {
		tierVG := PerTierVGName(poolName, pv.TierName)
		if dataExists, _ := s.lvExists(tierVG, "data"); dataExists {
			pool.State = PoolStateHealthy
			break
		}
	}

	return pool
}

// persistPoolLocked writes pool metadata to its LVs.  Caller must hold mu.
// LV creation is skipped silently when a PV has no free extents (e.g. for
// existing pools provisioned before tiermeta was introduced); the metadata
// is still kept in the RAM cache and will be persisted the next time a
// tiermeta LV can be created (e.g. after the data LV is shrunk or replaced).
func (s *Store) persistPoolLocked(pool *PoolMeta, slowestPVDevice string) {
	for i := range pool.Slots {
		slot := &pool.Slots[i]
		if slot.PVDevice == "" {
			continue
		}
		tierVG := PerTierVGName(pool.Name, slot.SlotName)
		// Ensure the per-tier meta LV exists.
		if exists, _ := s.lvExists(tierVG, TierLVName); !exists {
			// Don't attempt lvcreate when there's no room on the PV — the data
			// LV on existing pools typically consumes 100% of free extents.
			if pvFreeBytes(tierVG, slot.PVDevice) == 0 {
				continue
			}
			size := BytesToLVMSize(MetaLVSizeBytes(slot.PVSizeBytes))
			if err := s.createLV(tierVG, TierLVName, size, slot.PVDevice); err != nil {
				log.Printf("tiermeta: create %s/%s: %v", tierVG, TierLVName, err)
				continue
			}
		}
		tm := buildTierMeta(pool, slot)
		if err := WriteTierMeta(tierVG, tm); err != nil {
			log.Printf("tiermeta: write tier meta %s/%s: %v", tierVG, TierLVName, err)
		}
	}

	// Write complete metadata to the slowest tier's VG.
	if slowestPVDevice == "" {
		return
	}
	slowestSlot := s.slowestAssignedSlot(pool)
	if slowestSlot == nil {
		return
	}
	slowestVG := PerTierVGName(pool.Name, slowestSlot.SlotName)
	if exists, _ := s.lvExists(slowestVG, CompleteLVName); !exists {
		if pvFreeBytes(slowestVG, slowestPVDevice) == 0 {
			return
		}
		pvSizes := poolPVSizes(pool)
		size := BytesToLVMSize(CompleteMetaLVSizeBytes(pvSizes))
		if err := s.createLV(slowestVG, CompleteLVName, size, slowestPVDevice); err != nil {
			log.Printf("tiermeta: create %s/%s: %v", slowestVG, CompleteLVName, err)
			return
		}
	}
	if err := WriteCompleteMeta(slowestVG, pool); err != nil {
		log.Printf("tiermeta: write complete meta %s: %v", slowestVG, err)
	}
}

// pvFreeBytes returns the free bytes on a specific PV device within a VG.
// Returns 0 on any error (treated as no free space).
func pvFreeBytes(vg, pvDevice string) uint64 {
	out, err := exec.Command(
		"pvs", "--noheadings", "--nosuffix", "--units", "b",
		"--separator", "|", "-o", "pv_free",
		"--select", fmt.Sprintf("vg_name=%s pv_name=%s", vg, pvDevice),
	).Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0
	}
	var n uint64
	fmt.Sscanf(line, "%d", &n)
	return n
}

func poolPVSizes(pool *PoolMeta) []uint64 {
	sizes := make([]uint64, 0, len(pool.Slots))
	for _, slot := range pool.Slots {
		if slot.PVSizeBytes > 0 {
			sizes = append(sizes, slot.PVSizeBytes)
		}
	}
	return sizes
}

// listManagedPVs wraps the lvm package call. Using exec directly here keeps
// the tiermeta package free of an import cycle with lvm.
func listManagedPVs() ([]managedPV, error) {
	const (
		poolTagPrefix = "smoothnas-pool:"
		tierTagPrefix = "smoothnas-tier:"
	)

	out, err := exec.Command(
		"pvs",
		"--noheadings", "--separator", "|",
		"-o", "pv_name,vg_name,pv_tags",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("pvs: %w", err)
	}

	var pvs []managedPV
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 3 {
			continue
		}
		device := strings.TrimSpace(fields[0])
		tags := strings.Split(strings.TrimSpace(fields[2]), ",")

		poolName := ""
		tierName := ""
		for _, tag := range tags {
			tag = strings.TrimSpace(tag)
			if strings.HasPrefix(tag, poolTagPrefix) {
				poolName = strings.TrimPrefix(tag, poolTagPrefix)
			}
			if strings.HasPrefix(tag, tierTagPrefix) {
				tierName = strings.ToUpper(strings.TrimPrefix(tag, tierTagPrefix))
			}
		}
		if poolName == "" {
			continue
		}
		pvs = append(pvs, managedPV{
			Device:   device,
			PoolName: poolName,
			TierName: tierName,
		})
	}
	return pvs, nil
}

// --- Public read operations (served from RAM cache) ---

// GetPool returns a copy of the named pool's metadata from the RAM cache.
func (s *Store) GetPool(name string) (*PoolMeta, error) {
	if err := ValidatePoolName(name); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pool, ok := s.pools[name]
	if !ok {
		return nil, ErrNotFound
	}
	return copyPool(pool), nil
}

// ListPools returns copies of all pool metadata sorted by creation time then
// name (stable, deterministic).
func (s *Store) ListPools() []PoolMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PoolMeta, 0, len(s.pools))
	for _, p := range s.pools {
		out = append(out, *copyPool(p))
	}
	// Stable sort: created_at asc, name asc.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j].CreatedAt.Before(out[j-1].CreatedAt) ||
				(out[j].CreatedAt.Equal(out[j-1].CreatedAt) && out[j].Name < out[j-1].Name) {
				out[j], out[j-1] = out[j-1], out[j]
			} else {
				break
			}
		}
	}
	return out
}

// GetSlotByArrayPath returns the slot assigned to the given array device path,
// or ErrNotFound.
func (s *Store) GetSlotByArrayPath(arrayPath string) (*SlotMeta, error) {
	arrayPath = NormalizeArrayPath(arrayPath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pool := range s.pools {
		for i := range pool.Slots {
			if pool.Slots[i].ArrayPath == arrayPath {
				cp := pool.Slots[i]
				return &cp, nil
			}
		}
	}
	return nil, ErrNotFound
}

// EnsureArray registers an mdadm array path and returns a stable in-memory
// integer ID.  IDs are ephemeral — they reset across daemon restarts — but are
// stable for the lifetime of a single session, which is all the UI requires.
func (s *Store) EnsureArray(path string) int64 {
	path = NormalizeArrayPath(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.arrays[path]; ok {
		return id
	}
	s.nextID++
	s.arrays[path] = s.nextID
	return s.nextID
}

// --- Mutating operations ---

// CreatePool adds a new pool to the RAM cache in provisioning state.  Metadata
// LVs are created lazily in CreateSlotMetaLV when the first array is assigned.
func (s *Store) CreatePool(name, filesystem string, defs []TierDefinition) error {
	if err := ValidatePoolName(name); err != nil {
		return err
	}
	if filesystem == "" {
		filesystem = "xfs"
	}
	if len(defs) == 0 {
		defs = DefaultTierDefinitions()
	}
	if err := ValidateTierDefinitions(defs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.pools[name]; exists {
		return fmt.Errorf("tier %s already exists", name)
	}
	now := time.Now().UTC()
	slots := make([]SlotMeta, 0, len(defs))
	for _, d := range defs {
		slots = append(slots, SlotMeta{
			Version:   MetaVersion,
			PoolName:  name,
			SlotName:  d.Name,
			Rank:      d.Rank,
			State:     SlotStateEmpty,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	s.pools[name] = &PoolMeta{
		Version:    MetaVersion,
		Name:       name,
		Filesystem: filesystem,
		State:      PoolStateProvisioning,
		CreatedAt:  now,
		UpdatedAt:  now,
		Slots:      slots,
	}
	return nil
}

// CreateSlotMetaLV creates the on-disk metadata LV for a tier slot in that
// slot's own VG (tier-{pool}-{SLOT}).  It must be called after the slot's PV
// is initialised in the VG (i.e. after pvcreate + vgcreate/vgextend), so that
// lvcreate can constrain allocation to that PV.
//
// If this is the slowest (highest-rank) assigned slot, the tiermeta_complete
// LV is also created on that tier's VG.
func (s *Store) CreateSlotMetaLV(poolName, slotName, pvDevice string, pvSizeBytes uint64) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	tierVG := PerTierVGName(poolName, slotName)

	if exists, err := s.lvExists(tierVG, TierLVName); err != nil {
		return fmt.Errorf("check %s/%s: %w", tierVG, TierLVName, err)
	} else if !exists {
		size := BytesToLVMSize(MetaLVSizeBytes(pvSizeBytes))
		if err := s.createLV(tierVG, TierLVName, size, pvDevice); err != nil {
			return fmt.Errorf("create %s/%s: %w", tierVG, TierLVName, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}

	// Update PVSizeBytes in the slot record so CompleteMetaLVSizeBytes can
	// sum them correctly.
	for i := range pool.Slots {
		if pool.Slots[i].SlotName == slotName {
			pool.Slots[i].PVSizeBytes = pvSizeBytes
			pool.Slots[i].PVDevice = pvDevice
		}
	}

	slowestSlot := s.slowestAssignedSlot(pool)
	if slowestSlot == nil || slowestSlot.SlotName != slotName {
		return nil // complete LV stays on the current slowest tier
	}

	// This slot is now the slowest — (re)create the complete LV on its VG.
	pvSizes := poolPVSizes(pool)
	completeSize := BytesToLVMSize(CompleteMetaLVSizeBytes(pvSizes))

	if exists, _ := s.lvExists(tierVG, CompleteLVName); !exists {
		if err := s.createLV(tierVG, CompleteLVName, completeSize, pvDevice); err != nil {
			return fmt.Errorf("create %s/%s: %w", tierVG, CompleteLVName, err)
		}
	}
	return nil
}

// slowestAssignedSlot returns a pointer to the highest-rank assigned slot,
// or nil if no slots have a PV assigned.  Caller must hold mu.
func (s *Store) slowestAssignedSlot(pool *PoolMeta) *SlotMeta {
	maxRank := -1
	var slowest *SlotMeta
	for i := range pool.Slots {
		sl := &pool.Slots[i]
		if sl.PVDevice != "" && sl.Rank > maxRank {
			maxRank = sl.Rank
			slowest = sl
		}
	}
	return slowest
}

// slowestAssignedPV returns the PVDevice of the highest-rank assigned slot,
// or "" if no slots are assigned.  Caller must hold mu.
func (s *Store) slowestAssignedPV(pool *PoolMeta) string {
	sl := s.slowestAssignedSlot(pool)
	if sl == nil {
		return ""
	}
	return sl.PVDevice
}

// AssignSlot marks a slot as assigned and persists the metadata.
func (s *Store) AssignSlot(poolName, slotName, arrayPath, pvDevice string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return fmt.Errorf("slot name required")
	}
	arrayPath = NormalizeArrayPath(arrayPath)
	if arrayPath == "" {
		return fmt.Errorf("array path required")
	}
	pvDevice = NormalizeArrayPath(pvDevice)

	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	idx := s.slotIndex(pool, slotName)
	if idx < 0 {
		return fmt.Errorf("slot %s not found in pool %s", slotName, poolName)
	}
	slot := &pool.Slots[idx]
	if slot.State != SlotStateEmpty {
		return fmt.Errorf("slot %s/%s is already in state %s", poolName, slotName, slot.State)
	}
	// Check uniqueness across all pools.
	for _, p := range s.pools {
		for _, sl := range p.Slots {
			if sl.ArrayPath == arrayPath {
				return fmt.Errorf("array %s is already assigned to %s/%s", arrayPath, p.Name, sl.SlotName)
			}
		}
	}

	now := time.Now().UTC()
	slot.State = SlotStateAssigned
	slot.ArrayPath = arrayPath
	slot.PVDevice = pvDevice
	slot.UpdatedAt = now
	pool.UpdatedAt = now

	return s.persistAfterMutateLocked(pool)
}

// ClearSlot removes an array assignment from a slot and persists.
func (s *Store) ClearSlot(poolName, slotName string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	idx := s.slotIndex(pool, slotName)
	if idx < 0 {
		return ErrNotFound
	}
	now := time.Now().UTC()
	pool.Slots[idx].State = SlotStateEmpty
	pool.Slots[idx].ArrayPath = ""
	pool.Slots[idx].PVDevice = ""
	pool.Slots[idx].Target = nil
	pool.Slots[idx].MovementLog = nil
	pool.Slots[idx].UpdatedAt = now
	pool.UpdatedAt = now

	return s.persistAfterMutateLocked(pool)
}

// UpdateSlotState transitions a slot to a new state and persists.
func (s *Store) UpdateSlotState(poolName, slotName, next string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	idx := s.slotIndex(pool, slotName)
	if idx < 0 {
		return ErrNotFound
	}
	current := pool.Slots[idx].State
	if current == next {
		return nil
	}
	if !CanTransitionSlot(current, next) {
		return fmt.Errorf("invalid slot state transition %q -> %q", current, next)
	}
	if next == SlotStateEmpty {
		pool.Slots[idx].ArrayPath = ""
		pool.Slots[idx].PVDevice = ""
		pool.Slots[idx].Target = nil
		pool.Slots[idx].MovementLog = nil
	}
	now := time.Now().UTC()
	pool.Slots[idx].State = next
	pool.Slots[idx].UpdatedAt = now
	pool.UpdatedAt = now

	return s.persistAfterMutateLocked(pool)
}

// UpdatePoolState transitions the pool to a new state and persists.
func (s *Store) UpdatePoolState(poolName, next, errorReason string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	if pool.State == next {
		return nil
	}
	if !CanTransitionPool(pool.State, next) {
		return fmt.Errorf("invalid tier state transition %q -> %q", pool.State, next)
	}
	pool.State = next
	pool.ErrorReason = errorReason
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// SetPoolError transitions to the error state with a reason string.
func (s *Store) SetPoolError(poolName, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("error reason is required")
	}
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	if pool.State != PoolStateError && !CanTransitionPool(pool.State, PoolStateError) {
		return fmt.Errorf("invalid tier state transition %q -> %q", pool.State, PoolStateError)
	}
	pool.State = PoolStateError
	pool.ErrorReason = reason
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// SetDestroyingReason records an error_reason while the pool is in destroying
// state (used to capture partial teardown failures).
func (s *Store) SetDestroyingReason(poolName, reason string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	if pool.State != PoolStateDestroying {
		return fmt.Errorf("pool %s is in state %s, not %s", poolName, pool.State, PoolStateDestroying)
	}
	pool.ErrorReason = reason
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// MarkReconciled updates the last_reconciled_at timestamp and persists.
func (s *Store) MarkReconciled(poolName string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	pool.LastReconciledAt = &now
	pool.UpdatedAt = now

	return s.persistAfterMutateLocked(pool)
}

// UpsertTarget sets the TierTargetMeta for a slot and persists.
// Call this after the mdadm target LV has been created and the DB row written.
func (s *Store) UpsertTarget(poolName, slotName string, target *TierTargetMeta) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	idx := s.slotIndex(pool, slotName)
	if idx < 0 {
		return fmt.Errorf("slot %s not found in pool %s", slotName, poolName)
	}
	pool.Slots[idx].Target = target
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// UpsertNamespace sets the NamespaceMeta for a pool and persists.
// Call this after the namespace daemon has been started.
func (s *Store) UpsertNamespace(poolName string, ns *NamespaceMeta) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	pool.Namespace = ns
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// AppendMovementLog appends a movement log entry to the named slot and persists.
// The caller determines which slot (source or destination) to append to.
func (s *Store) AppendMovementLog(poolName, slotName string, entry MovementLogEntry) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	idx := s.slotIndex(pool, slotName)
	if idx < 0 {
		return fmt.Errorf("slot %s not found in pool %s", slotName, poolName)
	}
	pool.Slots[idx].MovementLog = append(pool.Slots[idx].MovementLog, entry)
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// RemoveMovementLogEntry removes the entry with the given ID from a slot's
// movement log and persists.
func (s *Store) RemoveMovementLogEntry(poolName, slotName string, entryID int64) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return ErrNotFound
	}
	idx := s.slotIndex(pool, slotName)
	if idx < 0 {
		return fmt.Errorf("slot %s not found in pool %s", slotName, poolName)
	}
	existing := pool.Slots[idx].MovementLog
	filtered := existing[:0]
	for _, e := range existing {
		if e.ID != entryID {
			filtered = append(filtered, e)
		}
	}
	pool.Slots[idx].MovementLog = filtered
	pool.UpdatedAt = time.Now().UTC()

	return s.persistAfterMutateLocked(pool)
}

// DeletePool removes a pool's metadata LVs and purges it from the RAM cache.
// Per-tier metadata LVs are removed from each tier's own VG.
func (s *Store) DeletePool(poolName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[poolName]
	if !ok {
		return
	}

	// Remove per-tier metadata LVs from each tier's own VG.
	for _, slot := range pool.Slots {
		tierVG := PerTierVGName(poolName, slot.SlotName)
		if exists, _ := s.lvExists(tierVG, TierLVName); exists {
			if err := s.removeLV(tierVG, TierLVName); err != nil {
				log.Printf("tiermeta: remove %s/%s: %v", tierVG, TierLVName, err)
			}
		}
		if exists, _ := s.lvExists(tierVG, CompleteLVName); exists {
			if err := s.removeLV(tierVG, CompleteLVName); err != nil {
				log.Printf("tiermeta: remove %s/%s: %v", tierVG, CompleteLVName, err)
			}
		}
	}

	delete(s.pools, poolName)
}

// --- Internal helpers ---

// copyPool returns a deep copy of pool including its Slots slice.
func copyPool(pool *PoolMeta) *PoolMeta {
	cp := *pool
	cp.Slots = make([]SlotMeta, len(pool.Slots))
	copy(cp.Slots, pool.Slots)
	return &cp
}

// buildTierMeta constructs a TierMeta from the pool context and a specific slot.
func buildTierMeta(pool *PoolMeta, slot *SlotMeta) *TierMeta {
	return &TierMeta{
		Version:      MetaVersion,
		PoolName:     pool.Name,
		Filesystem:   pool.Filesystem,
		PoolState:    pool.State,
		RegionSizeMB: pool.RegionSizeMB,
		Namespace:    pool.Namespace,
		Slot:         *slot,
		UpdatedAt:    pool.UpdatedAt,
	}
}

// slotIndex returns the index of slotName in pool.Slots, or -1.
func (s *Store) slotIndex(pool *PoolMeta, slotName string) int {
	for i := range pool.Slots {
		if pool.Slots[i].SlotName == slotName {
			return i
		}
	}
	return -1
}

// persistAfterMutateLocked writes updated metadata to all relevant LVs.
// Caller must hold mu (write lock).
func (s *Store) persistAfterMutateLocked(pool *PoolMeta) error {
	// Write each assigned slot's TierMeta to its own per-tier VG.
	for i := range pool.Slots {
		slot := &pool.Slots[i]
		if slot.PVDevice == "" {
			continue
		}
		tierVG := PerTierVGName(pool.Name, slot.SlotName)
		if exists, _ := s.lvExists(tierVG, TierLVName); !exists {
			continue // LV not yet created; CreateSlotMetaLV will do it
		}
		tm := buildTierMeta(pool, slot)
		if err := WriteTierMeta(tierVG, tm); err != nil {
			log.Printf("tiermeta: write tier meta %s/%s: %v", tierVG, TierLVName, err)
		}
	}

	// Write complete metadata to the slowest tier's VG.
	slowestSlot := s.slowestAssignedSlot(pool)
	if slowestSlot == nil {
		return nil
	}
	slowestVG := PerTierVGName(pool.Name, slowestSlot.SlotName)
	if exists, _ := s.lvExists(slowestVG, CompleteLVName); !exists {
		return nil // not yet created
	}
	if err := WriteCompleteMeta(slowestVG, pool); err != nil {
		log.Printf("tiermeta: write complete meta %s: %v", slowestVG, err)
	}
	return nil
}
