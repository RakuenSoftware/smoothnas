// Package mdadm manages Linux software RAID arrays via the mdadm CLI.
//
// All operations shell out to mdadm with explicit argument lists (no shell
// expansion). Disk paths and RAID levels are validated before use.
package mdadm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	diskpkg "github.com/JBailes/SmoothNAS/tierd/internal/disk"
)

// Array represents an mdadm RAID array.
type Array struct {
	Name        string   `json:"name"`       // e.g. "md0"
	Path        string   `json:"path"`       // e.g. "/dev/md0"
	RAIDLevel   string   `json:"raid_level"` // e.g. "raid5"
	State       string   `json:"state"`      // "active", "degraded", "rebuilding", "inactive"
	Size        uint64   `json:"size"`       // bytes
	SizeHuman   string   `json:"size_human"`
	MemberDisks []string `json:"member_disks"` // device paths
	ActiveDisks int      `json:"active_disks"`
	TotalDisks  int      `json:"total_disks"`
	RebuildPct  float64  `json:"rebuild_pct"` // 0-100 if rebuilding, -1 otherwise
	Role        string   `json:"role"`        // "origin", "cache", or ""
	MountPoint  string   `json:"mount_point"` // e.g. "/mnt/md0", or "" if not mounted
}

// validRAIDLevels is the set of RAID levels we accept.
var validRAIDLevels = map[string]bool{
	"raid0": true, "0": true,
	"raid1": true, "1": true,
	"raid4": true, "4": true,
	"raid5": true, "5": true,
	"raid6": true, "6": true,
	"raid10": true, "10": true,
	"linear": true,
}

// ValidateRAIDLevel checks if a RAID level string is acceptable.
func ValidateRAIDLevel(level string) error {
	if !validRAIDLevels[strings.ToLower(level)] {
		return fmt.Errorf("invalid RAID level: %s", level)
	}
	return nil
}

// ValidateDiskPath checks that a path looks like a block device.
var diskPathRegex = regexp.MustCompile(`^/dev/(sd[a-z]+|nvme\d+n\d+)$`)

func ValidateDiskPath(path string) error {
	if !diskPathRegex.MatchString(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}
	return nil
}

// ValidateArrayPath checks that a path looks like an md device.
var arrayPathRegex = regexp.MustCompile(`^/dev/md\d+$`)

func ValidateArrayPath(path string) error {
	if !arrayPathRegex.MatchString(path) {
		return fmt.Errorf("invalid array path: %s", path)
	}
	return nil
}

// PrepareDisks wipes filesystem signatures, partition tables, and old RAID
// superblocks from the given disks so they can be used in a new array. Each
// path must pass ValidateDiskPath first.
//
// Before wiping, it releases all holders on each disk: unmounts partitions,
// deactivates LVM volume groups, stops mdadm arrays, and removes kernel
// partition mappings. Without this the device stays busy and sgdisk fails.
func PrepareDisks(disks []string) error {
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := diskpkg.RequireUnassigned(disks); err != nil {
		return err
	}
	for _, d := range disks {
		if err := releaseHolders(d); err != nil {
			return fmt.Errorf("release holders %s: %w", d, err)
		}
		if err := diskpkg.WipeDevice(d); err != nil {
			return fmt.Errorf("wipe %s: %w", d, err)
		}
	}
	return nil
}

// releaseHolders tears down anything keeping a disk busy: mounted
// filesystems, LVM VGs, mdadm arrays, and kernel partition mappings.
func releaseHolders(disk string) error {
	// Discover partitions via lsblk.
	out, err := exec.Command("lsblk", "-ln", "-o", "PATH,TYPE,MOUNTPOINT", disk).Output()
	if err != nil {
		return nil // lsblk failure is non-fatal; sgdisk will report the real error
	}

	type part struct {
		path       string
		mountpoint string
	}
	var parts []part

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devType := fields[1]
		if devType != "part" {
			continue
		}
		p := part{path: fields[0]}
		if len(fields) >= 3 {
			p.mountpoint = fields[2]
		}
		parts = append(parts, p)
	}

	// Unmount any mounted partitions.
	for _, p := range parts {
		if p.mountpoint != "" {
			exec.Command("umount", "-l", p.mountpoint).Run()
		}
	}

	// Deactivate LVM on the disk's partitions (ignore errors — may not be LVM).
	for _, p := range parts {
		// pvs lists VGs on a given PV.
		pvOut, err := exec.Command("pvs", "--noheadings", "-o", "vg_name", p.path).Output()
		if err != nil {
			continue
		}
		vg := strings.TrimSpace(string(pvOut))
		if vg != "" {
			exec.Command("vgchange", "-an", vg).Run()
		}
	}

	// Stop any mdadm arrays that use the disk or its partitions.
	allDevs := append([]string{disk}, func() []string {
		s := make([]string, len(parts))
		for i, p := range parts {
			s[i] = p.path
		}
		return s
	}()...)

	mdOut, _ := exec.Command("cat", "/proc/mdstat").Output()
	for _, mdLine := range strings.Split(string(mdOut), "\n") {
		if !strings.Contains(mdLine, " : ") {
			continue
		}
		mdName := strings.Fields(mdLine)[0] // e.g. "md0"
		for _, dev := range allDevs {
			devBase := strings.TrimPrefix(dev, "/dev/")
			if strings.Contains(mdLine, devBase+"[") {
				exec.Command("mdadm", "--stop", "/dev/"+mdName).Run()
				break
			}
		}
	}

	// Zap partition tables on each partition, then remove partition mappings.
	for _, p := range parts {
		diskpkg.WipeDevice(p.path) //nolint:errcheck // best-effort during holder teardown
	}

	// Ask kernel to drop partition mappings.
	exec.Command("partx", "-d", disk).Run()

	return nil
}

// Create creates a new mdadm array (prepare + assemble + save config).
func Create(name, level string, disks []string) error {
	if err := ValidateRAIDLevel(level); err != nil {
		return err
	}
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
	}
	if err := PrepareDisks(disks); err != nil {
		return fmt.Errorf("prepare disks: %w", err)
	}
	if err := Assemble(name, level, disks); err != nil {
		return err
	}
	return SaveConf()
}

// Assemble runs the mdadm --create command. Callers must validate inputs
// and call PrepareDisks beforehand.
func Assemble(name, level string, disks []string) error {
	path := "/dev/" + name

	args := []string{
		"--create", path,
		"--level=" + level,
		"--raid-devices=" + strconv.Itoa(len(disks)),
		"--metadata=1.2",
		"--homehost=smoothnas",
		"--name=" + name,
		"--run",
	}
	if len(disks) == 1 {
		args = append(args, "--force")
	}
	args = append(args, disks...)

	cmd := exec.Command("mdadm", args...)
	cmd.Stdin = strings.NewReader("yes\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mdadm create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SaveConf regenerates /etc/mdadm/mdadm.conf from the current array state.
func SaveConf() error {
	return updateConf()
}

// List returns all mdadm arrays by parsing /proc/mdstat and mdadm --detail.
func List() ([]Array, error) {
	f, err := os.Open("/proc/mdstat")
	if err != nil {
		// No mdstat means no arrays (or no md module). Not an error.
		return nil, nil
	}
	defer f.Close()

	var names []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "md") && strings.Contains(line, " : ") {
			parts := strings.SplitN(line, " ", 2)
			names = append(names, parts[0])
		}
	}

	var arrays []Array
	for _, name := range names {
		a, err := Detail("/dev/" + name)
		if err != nil {
			continue
		}
		arrays = append(arrays, *a)
	}
	return arrays, nil
}

// Detail returns detailed info about a specific array via mdadm --detail --export and --detail.
func Detail(path string) (*Array, error) {
	if err := ValidateArrayPath(path); err != nil {
		return nil, err
	}

	out, err := exec.Command("mdadm", "--detail", path).Output()
	if err != nil {
		return nil, fmt.Errorf("mdadm detail %s: %w", path, err)
	}

	a, err := parseDetail(path, string(out))
	if err != nil {
		return nil, err
	}
	a.MountPoint = findMountPoint(path)
	return a, nil
}

// findMountPoint returns the mount point for a device path, or "" if not mounted.
func findMountPoint(devPath string) string {
	out, err := exec.Command("findmnt", "-n", "-o", "TARGET", devPath).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseDetail parses the output of mdadm --detail.
func parseDetail(path, output string) (*Array, error) {
	a := &Array{
		Path:       path,
		RebuildPct: -1,
	}

	// Extract name from path.
	parts := strings.Split(path, "/")
	a.Name = parts[len(parts)-1]

	lines := strings.Split(output, "\n")
	var memberSection bool

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if kv := parseKV(line, "Raid Level"); kv != "" {
			a.RAIDLevel = kv
		}
		if kv := parseKV(line, "Array Size"); kv != "" {
			// "Array Size : 1234567 (1.18 GiB 1.26 GB)"
			numStr := strings.Fields(kv)[0]
			if n, err := strconv.ParseUint(numStr, 10, 64); err == nil {
				a.Size = n * 1024 // mdadm reports KB
			}
		}
		if kv := parseKV(line, "State"); kv != "" {
			a.State = strings.ToLower(strings.TrimSpace(strings.Split(kv, ",")[0]))
		}
		if kv := parseKV(line, "Active Devices"); kv != "" {
			a.ActiveDisks, _ = strconv.Atoi(kv)
		}
		if kv := parseKV(line, "Total Devices"); kv != "" {
			a.TotalDisks, _ = strconv.Atoi(kv)
		}
		if kv := parseKV(line, "Rebuild Status"); kv != "" {
			// "12% complete"
			pctStr := strings.TrimSuffix(kv, "% complete")
			pctStr = strings.TrimSpace(pctStr)
			if pct, err := strconv.ParseFloat(pctStr, 64); err == nil {
				a.RebuildPct = pct
			}
		}

		// Member disk section starts after "Number   Major   Minor   RaidDevice State"
		if strings.HasPrefix(line, "Number") && strings.Contains(line, "RaidDevice") {
			memberSection = true
			continue
		}
		if memberSection && line != "" {
			fields := strings.Fields(line)
			if len(fields) >= 7 {
				a.MemberDisks = append(a.MemberDisks, fields[6])
			}
		}
	}

	a.SizeHuman = humanSize(a.Size)
	return a, nil
}

// Stop stops and destroys an array. Before stopping it releases anything
// holding the array device — mounted filesystems, LVM VGs, or ZFS pools —
// since mdadm --stop needs exclusive access. wipefs at the end clears
// residual signatures so the array doesn't get auto-reassembled.
func Stop(path string) error {
	if err := ValidateArrayPath(path); err != nil {
		return err
	}

	var lastOut []byte
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		releaseArrayHolders(path)

		cmd := exec.Command("mdadm", "--stop", path)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return updateConf()
		}

		lastOut = out
		lastErr = err
		forceReleaseArrayHolders(path)
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("mdadm stop: %s: %w", strings.TrimSpace(string(lastOut)), lastErr)
}

// releaseArrayHolders tears down anything keeping an md device busy:
// ZFS pools backed by it, LVM VGs with it as a PV, and mounted filesystems
// on the array or its partitions. All steps are best-effort — the caller's
// mdadm --stop will report the real failure if something still holds on.
func releaseArrayHolders(arrayPath string) {
	// Destroy any ZFS pool that uses this array as a vdev. The caller is
	// destroying the array, so dependent pools must yield rather than keep the
	// md device busy. zpool status -LP prints resolved device paths; match the
	// exact array path.
	if out, err := exec.Command("zpool", "list", "-H", "-o", "name").Output(); err == nil {
		for _, pool := range strings.Fields(string(out)) {
			if pool == "" {
				continue
			}
			statusOut, err := exec.Command("zpool", "status", "-LP", pool).Output()
			if err != nil {
				continue
			}
			if strings.Contains(string(statusOut), arrayPath) {
				releaseZPoolHolders(pool)
				if err := exec.Command("zpool", "destroy", "-f", pool).Run(); err != nil {
					log.Printf("mdadm: zpool destroy -f %s before array stop: %v", pool, err)
					exec.Command("zpool", "export", "-f", pool).Run()
				}
			}
		}
	}

	for _, dev := range arrayDevices(arrayPath) {
		exec.Command("swapoff", dev).Run()
	}

	killDeviceHolders(arrayPath)
	unmountArrayMounts(arrayPath, false)

	// Deactivate LVM VG if the array is a PV.
	for _, dev := range arrayDevices(arrayPath) {
		if pvOut, err := exec.Command("pvs", "--noheadings", "-o", "vg_name", dev).Output(); err == nil {
			if vg := strings.TrimSpace(string(pvOut)); vg != "" {
				exec.Command("vgchange", "-an", "--force", vg).Run()
				exec.Command("pvremove", "-ff", "-y", dev).Run()
			}
		}
	}

	// Clear signatures on the array so a stale FS/PV/ZFS label doesn't
	// cause it to be claimed again before the stop lands.
	exec.Command("wipefs", "-a", arrayPath).Run()
}

func forceReleaseArrayHolders(arrayPath string) {
	unmountArrayMounts(arrayPath, true)
	for _, dev := range arrayDevices(arrayPath) {
		killDeviceHolders(dev)
		exec.Command("swapoff", dev).Run()
		exec.Command("blockdev", "--flushbufs", dev).Run()
	}
}

func arrayDevices(arrayPath string) []string {
	out, err := exec.Command("lsblk", "-ln", "-o", "PATH", arrayPath).Output()
	if err != nil {
		return []string{arrayPath}
	}
	seen := map[string]bool{}
	var devices []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dev := strings.TrimSpace(line)
		if dev == "" || seen[dev] {
			continue
		}
		seen[dev] = true
		devices = append(devices, dev)
	}
	if !seen[arrayPath] {
		devices = append([]string{arrayPath}, devices...)
	}
	return devices
}

func unmountArrayMounts(arrayPath string, kill bool) {
	lsOut, _ := exec.Command("lsblk", "-ln", "-o", "MOUNTPOINT", arrayPath).Output()
	mounts := strings.Fields(strings.TrimSpace(string(lsOut)))
	for i := len(mounts) - 1; i >= 0; i-- {
		mount := mounts[i]
		if kill {
			killDeviceHolders(mount)
		}
		exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-f", mount).Run()
		exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-l", mount).Run()
		exec.Command("umount", "-f", mount).Run()
		exec.Command("umount", "-l", mount).Run()
	}
}

func releaseZPoolHolders(pool string) {
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
			killDeviceHolders(mount)
		}
		exec.Command("zfs", "unmount", "-f", dataset).Run()
	}
}

func killDeviceHolders(path string) {
	if _, err := exec.LookPath("fuser"); err != nil {
		return
	}
	out, err := exec.Command("fuser", "-km", path).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return
		}
		log.Printf("mdadm: fuser -km %s: %v (out=%q)", path, err, strings.TrimSpace(string(out)))
	}
}

// ZeroSuperblocks removes mdadm superblocks from the given disks.
func ZeroSuperblocks(disks []string) error {
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return err
		}
		cmd := exec.Command("mdadm", "--zero-superblock", d)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("zero superblock %s: %s: %w", d, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// AddDisk adds a disk to an existing array (grow).
func AddDisk(arrayPath, diskPath string) error {
	if err := ValidateArrayPath(arrayPath); err != nil {
		return err
	}
	if err := ValidateDiskPath(diskPath); err != nil {
		return err
	}

	cmd := exec.Command("mdadm", "--manage", arrayPath, "--add", diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mdadm add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Grow the array to use the new disk.
	a, err := Detail(arrayPath)
	if err != nil {
		return err
	}
	growCmd := exec.Command("mdadm", "--grow", arrayPath,
		"--raid-devices="+strconv.Itoa(a.TotalDisks))
	if out, err := growCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mdadm grow: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return updateConf()
}

// FailDisk marks a disk as failed in the array.
func FailDisk(arrayPath, diskPath string) error {
	if err := ValidateArrayPath(arrayPath); err != nil {
		return err
	}
	if err := ValidateDiskPath(diskPath); err != nil {
		return err
	}

	cmd := exec.Command("mdadm", "--manage", arrayPath, "--fail", diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mdadm fail: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveDisk removes a (failed) disk from the array.
func RemoveDisk(arrayPath, diskPath string) error {
	if err := ValidateArrayPath(arrayPath); err != nil {
		return err
	}
	if err := ValidateDiskPath(diskPath); err != nil {
		return err
	}

	cmd := exec.Command("mdadm", "--manage", arrayPath, "--remove", diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mdadm remove: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ReplaceDisk removes the old disk and adds the new one, triggering a rebuild.
func ReplaceDisk(arrayPath, oldDisk, newDisk string) error {
	if err := FailDisk(arrayPath, oldDisk); err != nil {
		return fmt.Errorf("fail old disk: %w", err)
	}
	if err := RemoveDisk(arrayPath, oldDisk); err != nil {
		return fmt.Errorf("remove old disk: %w", err)
	}
	cmd := exec.Command("mdadm", "--manage", arrayPath, "--add", newDisk)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("add new disk: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// IsParityRAID reports whether the given mdadm RAID level is a parity
// level (raid4/5/6) and therefore benefits from a larger stripe cache.
func IsParityRAID(level string) bool {
	switch strings.ToLower(level) {
	case "raid4", "4", "raid5", "5", "raid6", "6":
		return true
	}
	return false
}

// DefaultStripeCachePages is the stripe_cache_size we want for parity
// RAID arrays. The kernel default of 256 pages gives terrible small
// random write performance because it forces the array to do
// read-modify-write almost immediately. 8192 pages costs roughly
// 8192 * num_disks * 4 KiB ≈ 96 MiB of RAM on a 3-disk array and
// dramatically improves small random write throughput.
const DefaultStripeCachePages = 8192

// SetStripeCacheSize sets stripe_cache_size (in pages) for an mdadm
// RAID4/5/6 array. No-op (and not an error) for non-parity levels and
// for arrays whose md sysfs node has no stripe_cache_size file.
func SetStripeCacheSize(arrayPath string, pages int) error {
	if err := ValidateArrayPath(arrayPath); err != nil {
		return err
	}
	name := strings.TrimPrefix(arrayPath, "/dev/")
	p := "/sys/block/" + name + "/md/stripe_cache_size"
	if _, err := os.Stat(p); err != nil {
		// File doesn't exist for non-parity RAID — nothing to do.
		return nil
	}
	return os.WriteFile(p, []byte(strconv.Itoa(pages)), 0644)
}

// readStripeCacheSize returns the current stripe_cache_size for an
// array, or 0 if it cannot be read.
func readStripeCacheSize(arrayName string) int {
	data, err := os.ReadFile("/sys/block/" + arrayName + "/md/stripe_cache_size")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

// EnsureStripeCacheSize walks every active mdadm parity RAID array and
// raises stripe_cache_size to at least minPages. Existing arrays created
// by older tierd versions are left at the kernel default of 256, which
// silently caps small random write throughput. Calling this on startup
// heals those installs.
func EnsureStripeCacheSize(minPages int) {
	arrays, err := List()
	if err != nil {
		return
	}
	for _, a := range arrays {
		if !IsParityRAID(a.RAIDLevel) {
			continue
		}
		cur := readStripeCacheSize(a.Name)
		if cur >= minPages {
			continue
		}
		if err := SetStripeCacheSize(a.Path, minPages); err != nil {
			continue
		}
	}
}

// Scrub starts a data scrub (check) on the array.
func Scrub(path string) error {
	if err := ValidateArrayPath(path); err != nil {
		return err
	}

	// Extract md name for sysfs path.
	name := strings.TrimPrefix(path, "/dev/")
	sysPath := "/sys/block/" + name + "/md/sync_action"

	if err := os.WriteFile(sysPath, []byte("check"), 0644); err != nil {
		return fmt.Errorf("start scrub: %w", err)
	}
	return nil
}

// updateConf regenerates /etc/mdadm/mdadm.conf from the current array state.
func updateConf() error {
	out, err := exec.Command("mdadm", "--detail", "--scan").Output()
	if err != nil {
		return fmt.Errorf("mdadm scan: %w", err)
	}

	conf := "# Auto-generated by tierd. Do not edit.\nMAILADDR root\n" + string(out)
	if err := os.WriteFile("/etc/mdadm/mdadm.conf", []byte(conf), 0644); err != nil {
		// Non-fatal: directory may not exist yet.
		return nil
	}

	// Update initramfs so arrays assemble on boot.
	exec.Command("update-initramfs", "-u").Run()
	return nil
}

// --- Build helpers (exported for testing) ---

// BuildCreateArgs returns the argument list for mdadm --create without executing.
func BuildCreateArgs(name, level string, disks []string) ([]string, error) {
	if err := ValidateRAIDLevel(level); err != nil {
		return nil, err
	}
	for _, d := range disks {
		if err := ValidateDiskPath(d); err != nil {
			return nil, err
		}
	}

	args := []string{
		"--create", "/dev/" + name,
		"--level=" + level,
		"--raid-devices=" + strconv.Itoa(len(disks)),
		"--metadata=1.2",
		"--homehost=smoothnas",
		"--name=" + name,
		"--run",
	}
	if len(disks) == 1 {
		args = append(args, "--force")
	}
	return append(args, disks...), nil
}

// ParseDetailOutput parses mdadm --detail output. Exported for testing.
func ParseDetailOutput(path, output string) (*Array, error) {
	return parseDetail(path, output)
}

// --- helpers ---

func parseKV(line, key string) string {
	prefix := key + " :"
	if !strings.Contains(line, prefix) {
		return ""
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
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

// MarshalJSON is not needed; we use the default encoder via struct tags.
// This placeholder satisfies the json import.
var _ = json.Marshal
