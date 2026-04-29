// Package zfs manages ZFS pools, datasets, zvols, and snapshots via the
// zpool and zfs CLI tools. All operations use exec.Command with explicit
// argument lists (no shell expansion).
package zfs

import (
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
)

// Pool represents a ZFS storage pool.
type Pool struct {
	Name          string `json:"name"`
	Health        string `json:"health"`        // "ONLINE", "DEGRADED", "FAULTED", "OFFLINE"
	Size          uint64 `json:"size"`          // bytes
	Allocated     uint64 `json:"allocated"`     // bytes
	Free          uint64 `json:"free"`          // bytes
	Fragmentation int    `json:"fragmentation"` // percentage
	SizeHuman     string `json:"size_human"`
	AllocHuman    string `json:"alloc_human"`
	FreeHuman     string `json:"free_human"`
	VdevLayout    string `json:"vdev_layout"` // raw zpool status vdev section
	ScanStatus    string `json:"scan_status"` // scrub/resilver status line
	Errors        string `json:"errors"`
}

// ImportablePool is a pool found by `zpool import` that is not currently imported.
type ImportablePool struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	State  string `json:"state"`
	Status string `json:"status"`
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
	if name == "import" {
		return fmt.Errorf("pool name %q is reserved", name)
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

var importDevicePathRegex = regexp.MustCompile(`^/dev/[A-Za-z0-9._:/-]+$`)

func ValidateImportDevicePath(path string) error {
	if path == "" {
		return nil
	}
	if !importDevicePathRegex.MatchString(path) || strings.Contains(path, "..") || filepath.Clean(path) != path {
		return fmt.Errorf("invalid import device path: %s", path)
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
	if vdevType == "stripe" {
		vdevType = ""
	}
	allDisks := append(append(dataDisks, slogDisks...), l2arcDisks...)
	for _, d := range allDisks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := disk.RequireUnassigned(allDisks); err != nil {
		return err
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

// ListImportablePools returns ZFS pools discoverable on local disks but not imported.
func ListImportablePools() ([]ImportablePool, error) {
	out, err := exec.Command("zpool", "import").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" || strings.Contains(msg, "no pools available") {
			return []ImportablePool{}, nil
		}
		return nil, fmt.Errorf("zpool import: %s: %w", msg, err)
	}
	return ParseImportablePools(string(out)), nil
}

// ImportPool imports an existing ZFS pool discovered on local disks.
func ImportPool(name string) error {
	if err := ValidatePoolName(name); err != nil {
		return err
	}
	cmd := exec.Command("zpool", "import", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zpool import %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// WipeZFSMemberDisks intentionally wipes disks currently identified as ZFS pool members.
func WipeZFSMemberDisks(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("disks required")
	}
	activeMembers := activeZPoolMemberDisks()
	disks, err := disk.List()
	if err != nil {
		return fmt.Errorf("list disks: %w", err)
	}
	assigned := make(map[string]string, len(disks))
	for _, d := range disks {
		assigned[d.Path] = d.Assignment
	}
	for _, path := range paths {
		if err := ValidateDiskPath(path); err != nil {
			return err
		}
		if assigned[path] != "zfs-pool" {
			return fmt.Errorf("disk %s is not a ZFS pool member", path)
		}
		if activeMembers[path] {
			return fmt.Errorf("disk %s belongs to an imported ZFS pool; destroy or export the pool before wiping it", path)
		}
	}
	for _, path := range paths {
		if err := disk.WipeDevice(path); err != nil {
			return fmt.Errorf("wipe %s: %w", path, err)
		}
	}
	return nil
}

func activeZPoolMemberDisks() map[string]bool {
	members := make(map[string]bool)
	out, err := exec.Command("zpool", "status", "-P").Output()
	if err != nil {
		return members
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		members[wholeDiskPath(fields[0])] = true
	}
	return members
}

func wholeDiskPath(path string) string {
	if strings.HasPrefix(path, "/dev/nvme") {
		if idx := strings.LastIndex(path, "p"); idx > len("/dev/nvme") {
			if _, err := strconv.Atoi(path[idx+1:]); err == nil {
				return path[:idx]
			}
		}
		return path
	}
	for len(path) > len("/dev/sd") {
		last := path[len(path)-1]
		if last < '0' || last > '9' {
			break
		}
		path = path[:len(path)-1]
	}
	return path
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

// ParseImportablePools parses the human-readable `zpool import` output.
func ParseImportablePools(output string) []ImportablePool {
	var pools []ImportablePool
	var current *ImportablePool
	var status []string

	flush := func() {
		if current == nil {
			return
		}
		current.Status = strings.Join(status, " ")
		pools = append(pools, *current)
		current = nil
		status = nil
	}

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "pool:"):
			flush()
			current = &ImportablePool{Name: strings.TrimSpace(strings.TrimPrefix(trimmed, "pool:"))}
		case current != nil && strings.HasPrefix(trimmed, "id:"):
			current.ID = strings.TrimSpace(strings.TrimPrefix(trimmed, "id:"))
		case current != nil && strings.HasPrefix(trimmed, "state:"):
			current.State = strings.TrimSpace(strings.TrimPrefix(trimmed, "state:"))
		case current != nil && strings.HasPrefix(trimmed, "status:"):
			status = append(status, strings.TrimSpace(strings.TrimPrefix(trimmed, "status:")))
		case current != nil && strings.HasPrefix(trimmed, "action:"):
			// The action line starts a new logical section; do not mix it into status.
		case current != nil && len(status) > 0 && trimmed != "" && !strings.Contains(trimmed, ":"):
			status = append(status, trimmed)
		}
	}
	flush()

	return pools
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

// SetAtimeOff disables atime updates for a pool and inherited datasets.
func SetAtimeOff(poolName string) error {
	if err := ValidatePoolName(poolName); err != nil {
		return err
	}
	cmd := exec.Command("zfs", "set", "atime=off", poolName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs set atime=off %s: %s: %w", poolName, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// DestroyPool destroys a ZFS pool.
func DestroyPool(name string) error {
	if err := ValidatePoolName(name); err != nil {
		return err
	}

	memberDevices := poolMemberDevices(name)
	var lastOut []byte
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		releasePoolHolders(name)

		cmd := exec.Command("zpool", "destroy", "-f", name)
		out, err := cmd.CombinedOutput()
		if err == nil || zpoolDestroyAlreadyGone(out) {
			wipeFormerPoolMembers(memberDevices)
			return nil
		}

		lastOut = out
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("zpool destroy: %s: %w", strings.TrimSpace(string(lastOut)), lastErr)
}

func poolMemberDevices(pool string) []string {
	out, err := exec.Command("zpool", "status", "-LP", pool).Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var devices []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		dev := fields[0]
		if seen[dev] {
			continue
		}
		seen[dev] = true
		devices = append(devices, dev)
	}
	return devices
}

func wipeFormerPoolMembers(devices []string) {
	for _, dev := range devices {
		if err := disk.WipeDevice(dev); err != nil {
			log.Printf("zfs: wipe former pool member %s: %v", dev, err)
		}
	}
}

func zpoolDestroyAlreadyGone(out []byte) bool {
	msg := strings.ToLower(string(out))
	return strings.Contains(msg, "no such pool") || strings.Contains(msg, "no pools available")
}

// MemberDevices returns the whole-disk devices currently reported as members
// of an imported ZFS pool.
func MemberDevices(pool string) []string {
	return poolMemberDevices(pool)
}

func releasePoolHolders(pool string) {
	releasePoolFilesystemHolders(pool)
	releasePoolVolumeHolders(pool)
}

func releasePoolFilesystemHolders(pool string) {
	out, err := exec.Command("zfs", "list", "-H", "-r", "-t", "filesystem", "-o", "name,mountpoint", pool).Output()
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		fields := strings.Fields(lines[i])
		if len(fields) < 2 {
			continue
		}
		dataset, mount := fields[0], fields[1]
		if mount != "-" && mount != "legacy" && mount != "none" && mount != "/" {
			killPathHolders(mount)
			exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-f", mount).Run()
			exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-l", mount).Run()
			exec.Command("umount", "-f", mount).Run()
			exec.Command("umount", "-l", mount).Run()
		}
		exec.Command("zfs", "unmount", "-f", dataset).Run()
	}
}

func releasePoolVolumeHolders(pool string) {
	out, err := exec.Command("zfs", "list", "-H", "-r", "-t", "volume", "-o", "name", pool).Output()
	if err != nil {
		return
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		volume := strings.TrimSpace(line)
		if volume == "" {
			continue
		}
		for _, dev := range zvolDevicePaths(volume) {
			killPathHolders(dev)
			exec.Command("swapoff", dev).Run()
			exec.Command("blockdev", "--flushbufs", dev).Run()
		}
	}
}

func zvolDevicePaths(volume string) []string {
	return []string{
		"/dev/zvol/" + volume,
		"/dev/zvol/dsk/" + volume,
	}
}

func killPathHolders(path string) {
	if _, err := exec.LookPath("fuser"); err != nil {
		return
	}
	out, err := exec.Command("fuser", "-km", path).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return
		}
		log.Printf("zfs: fuser -km %s: %v (out=%q)", path, err, strings.TrimSpace(string(out)))
	}
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
