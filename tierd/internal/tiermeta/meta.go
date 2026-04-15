// Package tiermeta manages per-tier metadata stored directly on each tier's
// own storage (metadata LVs), with a complete copy on the slowest tier and a
// full in-memory cache for fast reads.
package tiermeta

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const MetaVersion = 1

// Slot name constants — mirror the values previously in db.TierSlot*.
const (
	SlotNVME = "NVME"
	SlotSSD  = "SSD"
	SlotHDD  = "HDD"
)

// Pool state constants — mirror the values previously in db.TierPoolState*.
const (
	PoolStateProvisioning = "provisioning"
	PoolStateHealthy      = "healthy"
	PoolStateDegraded     = "degraded"
	PoolStateUnmounted    = "unmounted"
	PoolStateError        = "error"
	PoolStateDestroying   = "destroying"
)

// Slot state constants — mirror the values previously in db.TierSlotState*.
const (
	SlotStateEmpty    = "empty"
	SlotStateAssigned = "assigned"
	SlotStateDegraded = "degraded"
	SlotStateMissing  = "missing"
)

// MountRoot is the root directory for tier mount points.
// Overridden in tests.
var MountRoot = "/mnt"

// TierDefinition carries the name and rank of a tier slot used at pool
// creation time.
type TierDefinition struct {
	Name string
	Rank int
}

// DefaultTierDefinitions returns the standard three-tier set.
func DefaultTierDefinitions() []TierDefinition {
	return []TierDefinition{
		{Name: SlotNVME, Rank: 1},
		{Name: SlotSSD, Rank: 2},
		{Name: SlotHDD, Rank: 3},
	}
}

// TierTargetMeta captures the LVM-backed storage target for one tier slot.
// It combines what the DB stores across tier_targets and mdadm_managed_targets.
type TierTargetMeta struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	PlacementDomain  string `json:"placement_domain,omitempty"`
	Rank             int    `json:"rank"`
	TargetFillPct    int    `json:"target_fill_pct,omitempty"`
	FullThresholdPct int    `json:"full_threshold_pct,omitempty"`
	Health           string `json:"health,omitempty"`
	VGName           string `json:"vg_name"`
	LVName           string `json:"lv_name"`
	MountPath        string `json:"mount_path"`
}

// MovementLogEntry records one in-flight or recently completed file movement
// that involves this tier (as source or destination).
type MovementLogEntry struct {
	ID             int64  `json:"id"`
	ObjectID       string `json:"object_id"`
	NamespaceID    string `json:"namespace_id"`
	SourceTargetID string `json:"source_target_id"`
	DestTargetID   string `json:"dest_target_id"`
	ObjectKey      string `json:"object_key"`
	State          string `json:"state"`
	FailureReason  string `json:"failure_reason,omitempty"`
	StartedAt      string `json:"started_at"`
	UpdatedAt      string `json:"updated_at"`
}

// NamespaceMeta captures the FUSE namespace daemon state for a pool.
type NamespaceMeta struct {
	ID          string `json:"id"`
	SocketPath  string `json:"socket_path"`
	MountPath   string `json:"mount_path"`
	DaemonPID   int    `json:"daemon_pid"`
	DaemonState string `json:"daemon_state"`
}

// SlotMeta is stored on a tier slot's own metadata LV.
// It captures everything needed to reconstruct the slot's state without the
// SQLite database.
type SlotMeta struct {
	Version     int       `json:"version"`
	PoolName    string    `json:"pool_name"`
	SlotName    string    `json:"slot_name"` // NVME | SSD | HDD
	Rank        int       `json:"rank"`
	State       string    `json:"state"` // empty | assigned | degraded | missing
	ArrayPath   string    `json:"array_path,omitempty"`
	PVDevice    string    `json:"pv_device,omitempty"`
	PVSizeBytes uint64    `json:"pv_size_bytes,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Extended fields — populated once the slot is provisioned.
	Target      *TierTargetMeta    `json:"target,omitempty"`
	MovementLog []MovementLogEntry `json:"movement_log,omitempty"`
}

// PoolMeta is the complete metadata for a tier pool.  It is stored on the
// slowest (highest-rank) assigned tier's "tiermeta_complete" LV and kept
// in-memory for fast reads.
type PoolMeta struct {
	Version          int        `json:"version"`
	Name             string     `json:"name"`
	Filesystem       string     `json:"filesystem"`
	State            string     `json:"state"`
	ErrorReason      string     `json:"error_reason,omitempty"`
	RegionSizeMB     int        `json:"region_size_mb,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LastReconciledAt *time.Time `json:"last_reconciled_at,omitempty"`
	Namespace        *NamespaceMeta `json:"namespace,omitempty"`
	Slots            []SlotMeta `json:"slots"`
}

// TierMeta is the payload written to each tier's own metadata LV
// (LV name "tiermeta" inside VG "tier-{pool}-{SLOT}").
// It carries enough pool context and this tier's complete data so that the
// tier can boot and operate independently, even if other tiers are unavailable.
type TierMeta struct {
	Version      int            `json:"version"`
	PoolName     string         `json:"pool_name"`
	Filesystem   string         `json:"filesystem"`
	PoolState    string         `json:"pool_state"`
	RegionSizeMB int            `json:"region_size_mb,omitempty"`
	Namespace    *NamespaceMeta `json:"namespace,omitempty"`
	Slot         SlotMeta       `json:"slot"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// MountPoint returns the canonical mount point for the pool.
func (p *PoolMeta) MountPoint() string {
	return MountRoot + "/" + p.Name
}

// PerTierVGName returns the LVM volume group name for a specific tier slot
// within a pool.  Format: "tier-{pool}-{SLOT}", e.g. "tier-media-NVME".
func PerTierVGName(poolName, tierName string) string {
	return "tier-" + poolName + "-" + tierName
}

// VGName returns the legacy monolithic VG name for a pool.
// Kept for backward compatibility with tests; new code uses PerTierVGName.
func VGName(poolName string) string {
	return "tier-" + poolName
}

// TierLVName is the LV name for the per-tier metadata LV inside each tier's
// own VG (e.g. VG "tier-media-NVME" → LV "tiermeta").
const TierLVName = "tiermeta"

// SlotLVName returns the legacy per-slot LV name used before per-tier VGs.
// Kept for backward compatibility with tests; new code uses TierLVName.
func SlotLVName(slotName string) string {
	return "tiermeta_" + slotName
}

// CompleteLVName is the LV that holds the full PoolMeta on the slowest tier.
const CompleteLVName = "tiermeta_complete"

// DevicePath returns the /dev/{vg}/{lv} path that LVM creates as a symlink.
func DevicePath(vg, lv string) string {
	return "/dev/" + vg + "/" + lv
}

// --- Validation ---

var (
	poolNameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)
	poolNameStartRe = regexp.MustCompile(`^[a-z0-9]`)
	poolReserved    = map[string]struct{}{
		"data": {}, "root": {}, "home": {}, "boot": {}, "tmp": {},
		"var": {}, "sys": {}, "proc": {}, "run": {}, "dev": {},
		"mnt": {}, "srv": {}, "opt": {}, "etc": {}, "lost+found": {},
	}
)

const (
	maxPoolNameLen = 31
	vgPrefix       = "tier-"
	maxVGNameLen   = 127
)

// ValidatePoolName returns an error if name is not a legal tier pool name.
func ValidatePoolName(name string) error {
	if name == "" {
		return fmt.Errorf("tier name is required")
	}
	if _, reserved := poolReserved[name]; reserved {
		return fmt.Errorf("tier name %q is reserved", name)
	}
	if len(name) > maxPoolNameLen {
		return fmt.Errorf("tier name %q must be 31 characters or fewer", name)
	}
	if !poolNameStartRe.MatchString(name) {
		return fmt.Errorf("tier name %q must start with a lowercase letter or digit", name)
	}
	if !poolNameRe.MatchString(name) {
		return fmt.Errorf("tier name %q must contain only lowercase letters, digits, hyphens, or underscores", name)
	}
	if len(vgPrefix)+len(name) > maxVGNameLen {
		return fmt.Errorf("volume group name %q exceeds the 127 character LVM limit", vgPrefix+name)
	}
	return nil
}

// ValidateTierDefinitions returns an error if the definitions contain
// duplicate names or ranks.
func ValidateTierDefinitions(defs []TierDefinition) error {
	if len(defs) == 0 {
		return nil
	}
	seenNames := map[string]struct{}{}
	seenRanks := map[int]struct{}{}
	for _, d := range defs {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			return fmt.Errorf("tier name is required")
		}
		if d.Rank <= 0 {
			return fmt.Errorf("tier rank for %q must be a positive integer", name)
		}
		if _, exists := seenNames[name]; exists {
			return fmt.Errorf("duplicate tier name %q", name)
		}
		if _, exists := seenRanks[d.Rank]; exists {
			return fmt.Errorf("duplicate tier rank %d", d.Rank)
		}
		seenNames[name] = struct{}{}
		seenRanks[d.Rank] = struct{}{}
	}
	return nil
}

// NormalizeArrayPath prepends /dev/ when missing.
func NormalizeArrayPath(array string) string {
	array = strings.TrimSpace(array)
	if array == "" {
		return ""
	}
	if strings.HasPrefix(array, "/dev/") {
		return array
	}
	return "/dev/" + array
}

// IsValidSlot reports whether slot is one of the three canonical names.
func IsValidSlot(slot string) bool {
	return slot == SlotNVME || slot == SlotSSD || slot == SlotHDD
}

// --- State transitions ---

var validPoolTransitions = map[string]map[string]struct{}{
	PoolStateProvisioning: {
		PoolStateHealthy:    {},
		PoolStateError:      {},
		PoolStateDestroying: {},
	},
	PoolStateHealthy: {
		PoolStateProvisioning: {},
		PoolStateDegraded:     {},
		PoolStateError:        {},
		PoolStateUnmounted:    {},
		PoolStateDestroying:   {},
	},
	PoolStateDegraded: {
		PoolStateProvisioning: {},
		PoolStateHealthy:      {},
		PoolStateError:        {},
		PoolStateDestroying:   {},
	},
	PoolStateUnmounted: {
		PoolStateProvisioning: {},
		PoolStateError:        {},
		PoolStateDestroying:   {},
	},
	PoolStateError: {
		PoolStateProvisioning: {},
		PoolStateDestroying:   {},
	},
}

var validSlotTransitions = map[string]map[string]struct{}{
	SlotStateEmpty: {
		SlotStateAssigned: {},
	},
	SlotStateAssigned: {
		SlotStateEmpty:    {},
		SlotStateDegraded: {},
		SlotStateMissing:  {},
	},
	SlotStateDegraded: {
		SlotStateAssigned: {},
	},
	SlotStateMissing: {
		SlotStateAssigned: {},
	},
}

func canTransition(valid map[string]map[string]struct{}, from, to string) bool {
	if from == to {
		return true
	}
	next, ok := valid[from]
	if !ok {
		return false
	}
	_, ok = next[to]
	return ok
}

// CanTransitionPool reports whether transitioning a pool from→to is valid.
func CanTransitionPool(from, to string) bool {
	return canTransition(validPoolTransitions, from, to)
}

// CanTransitionSlot reports whether transitioning a slot from→to is valid.
func CanTransitionSlot(from, to string) bool {
	return canTransition(validSlotTransitions, from, to)
}
