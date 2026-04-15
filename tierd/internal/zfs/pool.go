// Package zfs manages ZFS pools, datasets, zvols, and snapshots via the
// zpool and zfs CLI tools. All operations use exec.Command with explicit
// argument lists (no shell expansion).
package zfs

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
)

// Pool represents a ZFS storage pool.
type Pool struct {
	Name          string  `json:"name"`
	Health        string  `json:"health"`         // "ONLINE", "DEGRADED", "FAULTED", "OFFLINE"
	Size          uint64  `json:"size"`            // bytes
	Allocated     uint64  `json:"allocated"`       // bytes
	Free          uint64  `json:"free"`            // bytes
	Fragmentation int     `json:"fragmentation"`   // percentage
	SizeHuman     string  `json:"size_human"`
	AllocHuman    string  `json:"alloc_human"`
	FreeHuman     string  `json:"free_human"`
	VdevLayout    string  `json:"vdev_layout"`     // raw zpool status vdev section
	ScanStatus    string  `json:"scan_status"`     // scrub/resilver status line
	Errors        string  `json:"errors"`
}

// Vdev types we accept for pool creation.
var validVdevTypes = map[string]bool{
	"mirror": true,
	"raidz":  true, "raidz1": true,
	"raidz2": true, "raidz3": true,
	"draid1": true, "draid2": true, "draid3": true,
}

var poolNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
var diskPathRegex = regexp.MustCompile(`^/dev/(sd[a-z]+|nvme\d+n\d+)$`)

// ValidatePoolName checks pool name is alphanumeric with hyphens/underscores.
func ValidatePoolName(name string) error {
	if !poolNameRegex.MatchString(name) {
		return fmt.Errorf("invalid pool name: %s (must start with letter, alphanumeric/hyphens/underscores, max 64 chars)", name)
	}
	return nil
}

// ValidateVdevType checks if a vdev type string is acceptable.
func ValidateVdevType(vdevType string) error {
	if !validVdevTypes[strings.ToLower(vdevType)] {
		return fmt.Errorf("invalid vdev type: %s", vdevType)
	}
	return nil
}

// ValidateDiskPath checks that a path looks like a block device.
func ValidateDiskPath(path string) error {
	if !diskPathRegex.MatchString(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}
	return nil
}

// zapDisks wipes all signatures, partition tables, and RAID superblocks from
// each disk before it is used in a pool.
func zapDisks(disks []string) error {
	for _, d := range disks {
		if err := disk.WipeDevice(d); err != nil {
			return fmt.Errorf("wipe %s: %w", d, err)
		}
	}
	return nil
}

// CreatePool creates a new ZFS pool.
// vdevType can be "" for single-disk/stripe, or "mirror", "raidz", etc.
// slogDisks and l2arcDisks are optional.
func CreatePool(name, vdevType string, dataDisks, slogDisks, l2arcDisks []string) error {
	if err := ValidatePoolName(name); err != nil {
		return err
	}
	allDisks := append(append(dataDisks, slogDisks...), l2arcDisks...)
	for _, d := range allDisks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := zapDisks(allDisks); err != nil {
		return err
	}

	args := []string{"create", "-f", name}

	// Data vdev.
	if vdevType != "" {
		if err := ValidateVdevType(vdevType); err != nil {
			return err
		}
		args = append(args, vdevType)
	}
	args = append(args, dataDisks...)

	// SLOG.
	if len(slogDisks) > 0 {
		args = append(args, "log")
		if len(slogDisks) > 1 {
			args = append(args, "mirror")
		}
		args = append(args, slogDisks...)
	}

	// L2ARC.
	if len(l2arcDisks) > 0 {
		args = append(args, "cache")
		args = append(args, l2arcDisks...)
	}

	cmd := exec.Command("zpool", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "auto-loaded") || strings.Contains(msg, "modprobe zfs") {
			return fmt.Errorf("ZFS kernel module is not loaded — run 'modprobe zfs' as root, then retry (to load it on every boot: echo zfs >> /etc/modules)")
		}
		return fmt.Errorf("zpool create: %s: %w", msg, err)
	}
	return nil
}

// ListPools returns all ZFS pools.
func ListPools() ([]Pool, error) {
	out, err := exec.Command("zpool", "list", "-Hp",
		"-o", "name,health,size,alloc,free,frag",
	).Output()
	if err != nil {
		// No pools or zfs not loaded.
		return nil, nil
	}

	var pools []Pool
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}

		size, _ := strconv.ParseUint(fields[2], 10, 64)
		alloc, _ := strconv.ParseUint(fields[3], 10, 64)
		free, _ := strconv.ParseUint(fields[4], 10, 64)
		frag, _ := strconv.Atoi(fields[5])

		pools = append(pools, Pool{
			Name:          fields[0],
			Health:        fields[1],
			Size:          size,
			Allocated:     alloc,
			Free:          free,
			Fragmentation: frag,
			SizeHuman:     humanSize(size),
			AllocHuman:    humanSize(alloc),
			FreeHuman:     humanSize(free),
		})
	}
	return pools, nil
}

// DetailPool returns detailed status for a pool.
func DetailPool(name string) (*Pool, error) {
	if err := ValidatePoolName(name); err != nil {
		return nil, err
	}

	// Get numeric stats.
	out, err := exec.Command("zpool", "list", "-Hp",
		"-o", "name,health,size,alloc,free,frag", name,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("zpool list %s: %w", name, err)
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 6 {
		return nil, fmt.Errorf("unexpected zpool list output")
	}

	size, _ := strconv.ParseUint(fields[2], 10, 64)
	alloc, _ := strconv.ParseUint(fields[3], 10, 64)
	free, _ := strconv.ParseUint(fields[4], 10, 64)
	frag, _ := strconv.Atoi(fields[5])

	pool := &Pool{
		Name:          fields[0],
		Health:        fields[1],
		Size:          size,
		Allocated:     alloc,
		Free:          free,
		Fragmentation: frag,
		SizeHuman:     humanSize(size),
		AllocHuman:    humanSize(alloc),
		FreeHuman:     humanSize(free),
	}

	// Get vdev layout and scan status from zpool status.
	statusOut, err := exec.Command("zpool", "status", name).Output()
	if err == nil {
		pool.VdevLayout, pool.ScanStatus, pool.Errors = parsePoolStatus(string(statusOut))
	}

	return pool, nil
}

// DestroyPool destroys a ZFS pool.
func DestroyPool(name string) error {
	if err := ValidatePoolName(name); err != nil {
		return err
	}

	cmd := exec.Command("zpool", "destroy", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool destroy: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// AddVdev adds a data vdev to an existing pool.
func AddVdev(poolName, vdevType string, disks []string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := zapDisks(disks); err != nil {
		return err
	}

	args := []string{"add", "-f", poolName}
	if vdevType != "" {
		if err := ValidateVdevType(vdevType); err != nil {
			return err
		}
		args = append(args, vdevType)
	}
	args = append(args, disks...)

	cmd := exec.Command("zpool", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool add: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// AddSLOG adds SLOG device(s) to a pool.
func AddSLOG(poolName string, disks []string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := zapDisks(disks); err != nil {
		return err
	}

	args := []string{"add", "-f", poolName, "log"}
	if len(disks) > 1 {
		args = append(args, "mirror")
	}
	args = append(args, disks...)

	cmd := exec.Command("zpool", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool add slog: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveSLOG removes the SLOG from a pool.
func RemoveSLOG(poolName string, disks []string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	for _, d := range disks {
		args := []string{"remove", poolName, d}
		cmd := exec.Command("zpool", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("zpool remove slog %s: %s: %w", d, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// AddL2ARC adds L2ARC cache device(s) to a pool.
func AddL2ARC(poolName string, disks []string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := zapDisks(disks); err != nil {
		return err
	}

	args := []string{"add", "-f", poolName, "cache"}
	args = append(args, disks...)

	cmd := exec.Command("zpool", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool add l2arc: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveL2ARC removes L2ARC cache device(s) from a pool.
func RemoveL2ARC(poolName string, disks []string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	for _, d := range disks {
		args := []string{"remove", poolName, d}
		cmd := exec.Command("zpool", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("zpool remove l2arc %s: %s: %w", d, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// ReplaceDisk replaces a disk in a pool vdev (triggers resilver).
func ReplaceDisk(poolName, oldDisk, newDisk string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	if err := ValidateDiskPath(oldDisk); err != nil {
		return err
	}
	if err := ValidateDiskPath(newDisk); err != nil {
		return err
	}
	if err := zapDisks([]string{newDisk}); err != nil {
		return err
	}

	cmd := exec.Command("zpool", "replace", "-f", poolName, oldDisk, newDisk)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool replace: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Scrub starts a scrub on a pool.
func Scrub(poolName string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}

	cmd := exec.Command("zpool", "scrub", poolName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool scrub: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// --- Build helpers (exported for testing) ---

// BuildCreateArgs returns the zpool create argument list.
func BuildCreateArgs(name, vdevType string, dataDisks, slogDisks, l2arcDisks []string) ([]string, error) {
	if err := ValidatePoolName(name); err != nil {
		return nil, err
	}
	allDisks := append(append(dataDisks, slogDisks...), l2arcDisks...)
	for _, d := range allDisks {
		if err := ValidateDiskPath(d); err != nil {
			return nil, err
		}
	}
	if vdevType != "" {
		if err := ValidateVdevType(vdevType); err != nil {
			return nil, err
		}
	}

	args := []string{"create", "-f", name}
	if vdevType != "" {
		args = append(args, vdevType)
	}
	args = append(args, dataDisks...)

	if len(slogDisks) > 0 {
		args = append(args, "log")
		if len(slogDisks) > 1 {
			args = append(args, "mirror")
		}
		args = append(args, slogDisks...)
	}

	if len(l2arcDisks) > 0 {
		args = append(args, "cache")
		args = append(args, l2arcDisks...)
	}

	return args, nil
}

// ParsePoolStatusOutput parses zpool status output. Exported for testing.
func ParsePoolStatusOutput(output string) (vdevLayout, scanStatus, errors string) {
	return parsePoolStatus(output)
}

// --- internal ---

func parsePoolStatus(output string) (vdevLayout, scanStatus, errors string) {
	lines := strings.Split(output, "\n")
	var inConfig, inScan bool
	var configLines, scanLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "config:") {
			inConfig = true
			inScan = false
			continue
		}
		if strings.HasPrefix(trimmed, "scan:") {
			inScan = true
			inConfig = false
			scanLines = append(scanLines, strings.TrimPrefix(trimmed, "scan:"))
			continue
		}
		if strings.HasPrefix(trimmed, "errors:") {
			inConfig = false
			inScan = false
			errors = strings.TrimPrefix(trimmed, "errors:")
			errors = strings.TrimSpace(errors)
			continue
		}

		if inConfig && trimmed != "" {
			configLines = append(configLines, line)
		}
		if inScan && trimmed != "" && !strings.HasPrefix(trimmed, "config:") {
			scanLines = append(scanLines, line)
		}
	}

	vdevLayout = strings.Join(configLines, "\n")
	scanStatus = strings.TrimSpace(strings.Join(scanLines, " "))
	return
}

func humanSize(bytes uint64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1fT", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.1fG", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1fM", float64(bytes)/float64(MB))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
