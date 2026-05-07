package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// TierProvider is the subset of *db.Store that volume preflight needs
// to resolve and validate tier-bound assignments. Defined as an
// interface so unit tests can supply a fake without standing up a
// real tier subsystem.
type TierProvider interface {
	GetTierInstance(name string) (*db.TierInstance, error)
	ListTierSlots(poolName string) ([]db.TierSlot, error)
}

// Statfser returns the available bytes at a path. The default
// implementation calls statvfs(2) via golang.org/x/sys/unix; tests
// inject a fake that returns whatever they want to assert.
type Statfser func(path string) (availableBytes uint64, err error)

// TierAssignments is the operator's mapping of (plugin volume) →
// (tier name). PerVolume wins over Default when both are set; a
// volume without a per-entry falls back to Default; a volume with
// neither fails preflight.
type TierAssignments struct {
	PerVolume map[string]string
	Default   string
}

// Resolve returns the tier name chosen for volumeName, or "" if
// neither PerVolume[volumeName] nor Default is set.
func (a TierAssignments) Resolve(volumeName string) string {
	if a.PerVolume != nil {
		if name, ok := a.PerVolume[volumeName]; ok && name != "" {
			return name
		}
	}
	return a.Default
}

// VolumePlacement is one row in the preflight result: the resolved
// tier + path for one volume. Errors and warnings are per-volume so
// the UI can render them next to the picker that produced them.
type VolumePlacement struct {
	Volume   string
	Pool     string   // empty when Errors is non-empty and pool couldn't be resolved
	Slot     string
	HostPath string   // empty when Errors is non-empty
	Errors   []string // any error blocks install
	Warnings []string // surfaced to the operator but do not block
}

// PreflightResult is the aggregate of every volume's placement.
// OK is true iff every Placement has zero errors.
type PreflightResult struct {
	OK         bool
	Placements []VolumePlacement
}

// ResolveTierBoundHostPath is the canonical path computation for a
// plugin's tier-bound volume. The leading "." in ".plugins/" keeps
// plugin data out of casual SMB/NFS browsing of the share root,
// while sharing the tier's failure domain + performance profile.
func ResolveTierBoundHostPath(mountPoint, pluginName, volumeName string) string {
	return filepath.Join(mountPoint, ".plugins", pluginName, volumeName)
}

// PreflightTierAssignments runs every install-time gate against the
// supplied manifest + assignments. Returns a PreflightResult that
// can be rendered by the UI verbatim. Does not write to disk and
// does not modify the database.
//
// Gates per the phase 03 proposal:
//
//  1. Tier exists                — GetTierInstance returns a row
//  2. Slot exists                — slot is one of the seeded defaults
//                                  (NVME/SSD/HDD) or appears in ListTierSlots
//  3. Tier mounted               — pool.State is healthy or degraded
//                                  (a degraded pool is still writable)
//  4. Free space                 — statfs(mountpoint).avail ≥ minSize
//                                  (warn-only; some plugins legitimately
//                                  ignore minSize)
//  5. No path conflict           — the would-be host path does not yet
//                                  exist (a leftover from a prior install)
//
// Flat-mode volumes are reported as a single Placement with the
// flat host path filled in and zero errors — they bypass the gates
// since they don't depend on a tier.
func PreflightTierAssignments(tp TierProvider, statfs Statfser, m *Manifest, assignments TierAssignments, pluginsRoot string) (*PreflightResult, error) {
	if m == nil {
		return nil, fmt.Errorf("preflight: nil manifest")
	}
	if statfs == nil {
		statfs = StatAvail
	}

	out := &PreflightResult{OK: true}
	for _, vol := range m.Volumes {
		p := VolumePlacement{
			Volume: vol.Name,
			Slot:   vol.Slot,
		}

		switch vol.Mode {
		case VolumeModeFlat:
			// Flat volumes always live under DefaultPluginsRoot.
			// pluginsRoot lets tests override.
			p.HostPath = filepath.Join(pluginsRoot, m.Metadata.Name, vol.Name)
			out.Placements = append(out.Placements, p)
			continue

		case VolumeModeTierBound:
			poolName := assignments.Resolve(vol.Name)
			if poolName == "" {
				p.Errors = append(p.Errors,
					fmt.Sprintf("no tier assignment for volume %q (set --tier or pass tier_assignments)", vol.Name))
				out.OK = false
				out.Placements = append(out.Placements, p)
				continue
			}
			p.Pool = poolName

			pool, err := tp.GetTierInstance(poolName)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					p.Errors = append(p.Errors, fmt.Sprintf("tier %q does not exist", poolName))
				} else {
					return nil, fmt.Errorf("get tier %s: %w", poolName, err)
				}
				out.OK = false
				out.Placements = append(out.Placements, p)
				continue
			}

			if !slotExists(tp, poolName, vol.Slot) {
				p.Errors = append(p.Errors,
					fmt.Sprintf("slot %q does not exist on tier %q", vol.Slot, poolName))
				out.OK = false
			}

			if !poolReady(pool) {
				p.Errors = append(p.Errors,
					fmt.Sprintf("tier %q is in state %q; install requires healthy or degraded",
						poolName, pool.State))
				out.OK = false
			}

			hostPath := ResolveTierBoundHostPath(pool.MountPoint, m.Metadata.Name, vol.Name)
			p.HostPath = hostPath

			if _, err := os.Stat(hostPath); err == nil {
				p.Errors = append(p.Errors,
					fmt.Sprintf("path %s already exists; remove it or pick a different plugin name", hostPath))
				out.OK = false
			}

			// Free-space check is warn-only — some plugins legitimately
			// declare a large minSize as a hint, not a hard requirement.
			if minBytes, parsed := parseSize(vol.MinSize); parsed && pool.MountPoint != "" {
				avail, err := statfs(pool.MountPoint)
				if err == nil && avail < minBytes {
					p.Warnings = append(p.Warnings,
						fmt.Sprintf("tier %q has %s available; volume requests at least %s",
							poolName, formatBytes(avail), vol.MinSize))
				}
			}

			out.Placements = append(out.Placements, p)

		default:
			return nil, fmt.Errorf("preflight: unknown volume mode %q for %q", vol.Mode, vol.Name)
		}
	}
	return out, nil
}

// slotExists checks that slot is one of the seeded defaults (which
// are always present on a freshly-created tier) or appears in the
// tier_slots table for the pool. The seeded defaults are accepted
// even when ListTierSlots returns empty so a brand-new tier — not
// yet populated by the tiering subsystem — still passes preflight
// for an NVME/SSD/HDD-typed plugin.
func slotExists(tp TierProvider, poolName, slot string) bool {
	switch slot {
	case "NVME", "SSD", "HDD":
		return true
	}
	rows, err := tp.ListTierSlots(poolName)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.Name == slot {
			return true
		}
	}
	return false
}

// poolReady is true when the tier is in a state operators can write
// to. A degraded pool is allowed because the operator may
// legitimately install during a rebuild; states like provisioning,
// destroying, error, and unmounted are not.
func poolReady(p *db.TierInstance) bool {
	switch p.State {
	case db.TierPoolStateHealthy, db.TierPoolStateDegraded:
		return true
	}
	return false
}

// parseSize parses sizes like "50G", "100M", "2T" into bytes. Returns
// (0, false) when the string is empty, unparseable, or has trailing
// junk (e.g. "5x"). Conservative — this is for warning thresholds,
// not allocation, so a parse failure just skips the warning.
func parseSize(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	n := len(s)
	suffix := s[n-1]
	// Pure-digit input → treat as raw bytes.
	if suffix >= '0' && suffix <= '9' {
		var v uint64
		if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
			return 0, false
		}
		// Sscanf accepts trailing junk; verify the whole string parsed.
		if fmt.Sprintf("%d", v) != s {
			return 0, false
		}
		return v, true
	}
	// Suffixed form — must have at least one digit before the suffix.
	if n < 2 {
		return 0, false
	}
	mult := uint64(1)
	switch suffix {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	default:
		return 0, false
	}
	digits := s[:n-1]
	var v uint64
	if _, err := fmt.Sscanf(digits, "%d", &v); err != nil {
		return 0, false
	}
	if fmt.Sprintf("%d", v) != digits {
		return 0, false
	}
	return v * mult, true
}

// formatBytes renders n as a human-readable size. Used in warnings
// only, so precision is intentionally loose.
func formatBytes(n uint64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%dT", n>>40)
	case n >= 1<<30:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dK", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}
