// Package disk provides block device discovery via lsblk.
package disk

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var runPowerCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Disk represents a physical block device.
type Disk struct {
	Name       string `json:"name"`       // e.g. "sda", "nvme0n1"
	Path       string `json:"path"`       // e.g. "/dev/sda"
	Type       string `json:"type"`       // "disk" (we filter to this)
	Size       uint64 `json:"size"`       // bytes
	SizeHuman  string `json:"size_human"` // e.g. "1.8T"
	Model      string `json:"model"`      // drive model string
	Serial     string `json:"serial"`     // drive serial number
	Transport  string `json:"transport"`  // "sata", "nvme", "sas", "usb", etc.
	Rotational bool   `json:"rotational"` // true = HDD, false = SSD/NVMe
	DriveType  string `json:"drive_type"` // "HDD", "SSD", or "NVMe"
	Mountpoint string `json:"mountpoint"` // non-empty if any partition is mounted
	Assignment string `json:"assignment"` // "unassigned", "os", "mdadm-array", "zfs-pool", etc.
}

type PowerStatus struct {
	State         string `json:"state"`
	Eligible      bool   `json:"eligible"`
	IneligibleWhy string `json:"ineligible_reason,omitempty"`
	TimerMinutes  int    `json:"timer_minutes"`
}

// lsblkDevice matches the JSON output of lsblk --json.
type lsblkDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Type       string        `json:"type"`
	Size       json.Number   `json:"size"`
	Model      string        `json:"model"`
	Serial     string        `json:"serial"`
	Tran       string        `json:"tran"`
	Rota       bool          `json:"rota"`
	Mountpoint string        `json:"mountpoint"`
	FSType     string        `json:"fstype"`
	Children   []lsblkDevice `json:"children"`
}

type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

var evalSymlinks = filepath.EvalSymlinks

// List enumerates all physical block devices on the system.
func List() ([]Disk, error) {
	out, err := exec.Command(
		"lsblk", "--json", "--bytes", "--output",
		"NAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,ROTA,MOUNTPOINT,FSTYPE",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}

	var parsed lsblkOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}

	assigned := assignedDisks()

	var disks []Disk
	for _, dev := range parsed.Blockdevices {
		if dev.Type != "disk" {
			continue
		}

		size := parseSize(string(dev.Size))
		diskPath := normalizeDiskPath(dev.Path)
		assignment := assignmentForDevice(dev, diskPath, assigned)

		d := Disk{
			Name:       dev.Name,
			Path:       dev.Path,
			Type:       dev.Type,
			Size:       size,
			SizeHuman:  humanSize(size),
			Model:      strings.TrimSpace(dev.Model),
			Serial:     strings.TrimSpace(dev.Serial),
			Transport:  dev.Tran,
			Rotational: dev.Rota,
			DriveType:  classifyDrive(dev),
			Mountpoint: findMountpoint(dev),
			Assignment: assignment,
		}

		disks = append(disks, d)
	}

	return disks, nil
}

func assignmentForDevice(dev lsblkDevice, diskPath string, assigned map[string]string) string {
	if isOSDisk(dev) {
		return "os"
	}
	if label, ok := assigned[diskPath]; ok {
		return label
	}
	if label := assignmentFromFSType(dev); label != "" {
		return label
	}
	return "unassigned"
}

// classifyDrive determines if a device is HDD, SSD, or NVMe.
func classifyDrive(dev lsblkDevice) string {
	if dev.Tran == "nvme" {
		return "NVMe"
	}
	if dev.Rota {
		return "HDD"
	}
	return "SSD"
}

// findMountpoint checks the device and its children for any mountpoint.
func findMountpoint(dev lsblkDevice) string {
	if dev.Mountpoint != "" {
		return dev.Mountpoint
	}
	for _, child := range dev.Children {
		if child.Mountpoint != "" {
			return child.Mountpoint
		}
	}
	return ""
}

func assignmentFromFSType(dev lsblkDevice) string {
	switch dev.FSType {
	case "linux_raid_member":
		return "mdadm"
	case "zfs_member":
		return "zfs-pool"
	case "LVM2_member":
		return "lvm"
	}
	for _, child := range dev.Children {
		if label := assignmentFromFSType(child); label != "" {
			return label
		}
	}
	return ""
}

// assignedDisks returns a map of base disk path → assignment label for any disk
// that is a member of a ZFS pool or mdadm array.
func assignedDisks() map[string]string {
	result := make(map[string]string)

	// ZFS pool members: parse `zpool status -P` for /dev/ paths.
	if out, err := exec.Command("zpool", "status", "-P").Output(); err == nil {
		mergeAssignedZPoolMembers(result, string(out))
	}

	// mdadm array members: parse /proc/mdstat for sdX[N] / nvmeXnYpZ[N] tokens.
	if data, err := os.ReadFile("/proc/mdstat"); err == nil {
		mergeAssignedMDADMMembers(result, string(data))
	}

	return result
}

func mergeAssignedZPoolMembers(result map[string]string, status string) {
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "/dev/") {
			result[normalizeDiskPath(fields[0])] = "zfs-pool"
		}
	}
}

func mergeAssignedMDADMMembers(result map[string]string, mdstat string) {
	for _, line := range strings.Split(mdstat, "\n") {
		if !strings.Contains(line, " : ") {
			continue
		}
		for _, tok := range strings.Fields(line) {
			if idx := strings.Index(tok, "["); idx > 0 {
				name := tok[:idx]
				if strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "hd") {
					result[normalizeDiskPath("/dev/"+name)] = "mdadm"
				}
			}
		}
	}
}

func normalizeDiskPath(path string) string {
	if resolved, err := evalSymlinks(path); err == nil {
		path = resolved
	}
	return BaseDiskPath(path)
}

// baseDiskPath strips any partition suffix from a device path to return the
// whole-disk path. e.g. /dev/sda1 → /dev/sda, /dev/nvme0n1p2 → /dev/nvme0n1.
func BaseDiskPath(path string) string {
	if strings.HasPrefix(path, "/dev/nvme") {
		// NVMe partition suffix is p\d+
		if idx := strings.LastIndex(path, "p"); idx > len("/dev/nvme") {
			if isDigits(path[idx+1:]) {
				return path[:idx]
			}
		}
		return path
	}
	// SATA/SAS: strip trailing digits
	i := len(path)
	for i > 0 && path[i-1] >= '0' && path[i-1] <= '9' {
		i--
	}
	return path[:i]
}

// isOSDisk checks if any partition on this device is mounted at /, /boot, or /home.
func isOSDisk(dev lsblkDevice) bool {
	osMounts := map[string]bool{"/": true, "/boot": true, "/boot/efi": true, "/home": true}

	if osMounts[dev.Mountpoint] {
		return true
	}
	for _, child := range dev.Children {
		if osMounts[child.Mountpoint] {
			return true
		}
	}
	return false
}

// parseSize converts a string size (bytes) to uint64.
func parseSize(s string) uint64 {
	var n uint64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + uint64(c-'0')
		}
	}
	return n
}

// humanSize formats bytes into a human-readable string.
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

// RequireUnassigned rejects any disk path that is already in use by the OS,
// an mdadm array, or a ZFS pool.
func RequireUnassigned(paths []string) error {
	disks, err := List()
	if err != nil {
		return fmt.Errorf("list disks: %w", err)
	}
	return requireUnassignedFromDisks(disks, paths)
}

func requireUnassignedFromDisks(disks []Disk, paths []string) error {
	known := make(map[string]Disk, len(disks))
	for _, d := range disks {
		known[normalizeDiskPath(d.Path)] = d
	}
	for _, path := range paths {
		d, ok := known[normalizeDiskPath(path)]
		if !ok {
			return fmt.Errorf("disk %s not found", path)
		}
		if d.Assignment != "unassigned" {
			return fmt.Errorf("disk %s is already assigned as %s", path, d.Assignment)
		}
	}
	return nil
}

// WipeDevice removes all filesystem signatures, partition tables, and RAID
// superblocks from a block device. All three steps run unconditionally so that
// a failure in one does not prevent the others from running. partx removal is
// best-effort. The caller is responsible for validating the path.
func WipeDevice(path string) error {
	var errs []string
	for attempt := 0; attempt < 2; attempt++ {
		errs = wipeDeviceOnce(path)
		if len(errs) == 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func wipeDeviceOnce(path string) []string {
	var errs []string
	releaseHoldersForWipe(path)

	// Wipe all filesystem, LVM, and other signatures.
	if out, err := exec.Command("wipefs", "-a", path).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Sprintf("wipefs -a: %s: %v", strings.TrimSpace(string(out)), err))
	}

	// Zap partition tables (MBR + GPT including backup copy).
	if out, err := exec.Command("/sbin/sgdisk", "--zap-all", path).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Sprintf("sgdisk --zap-all: %s: %v", strings.TrimSpace(string(out)), err))
	}

	// Zero any mdadm superblock. A missing md superblock is the normal case
	// for most disks and should not make wipe fail.
	if out, err := exec.Command("mdadm", "--zero-superblock", path).CombinedOutput(); err != nil {
		if !isNoMDSuperblock(string(out)) {
			errs = append(errs, fmt.Sprintf("mdadm --zero-superblock: %s: %v", strings.TrimSpace(string(out)), err))
		}
	}

	// Remove partition mappings from the kernel (best-effort; may not exist).
	exec.Command("partx", "-d", path).Run()
	exec.Command("blockdev", "--rereadpt", path).Run()

	return errs
}

func isNoMDSuperblock(out string) bool {
	msg := strings.ToLower(out)
	return strings.Contains(msg, "unrecognised md component") ||
		strings.Contains(msg, "no md superblock") ||
		strings.Contains(msg, "couldn't open") ||
		strings.Contains(msg, "not an md array")
}

func releaseHoldersForWipe(path string) {
	for _, dev := range deviceTree(path) {
		exec.Command("swapoff", dev).Run()
	}
	destroyZPoolsUsing(path)
	stopMDArraysUsing(path)
	deactivateLVMUsing(path)
	unmountDeviceTree(path, true)
	for _, dev := range deviceTree(path) {
		killPathHolders(dev)
		exec.Command("blockdev", "--flushbufs", dev).Run()
	}
	time.Sleep(250 * time.Millisecond)
}

func deviceTree(path string) []string {
	out, err := exec.Command("lsblk", "-ln", "-o", "PATH", path).Output()
	if err != nil {
		return []string{path}
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
	if !seen[path] {
		devices = append([]string{path}, devices...)
	}
	return devices
}

func unmountDeviceTree(path string, kill bool) {
	out, _ := exec.Command("lsblk", "-ln", "-o", "MOUNTPOINT", path).Output()
	mounts := strings.Fields(strings.TrimSpace(string(out)))
	for i := len(mounts) - 1; i >= 0; i-- {
		mount := mounts[i]
		if kill {
			killPathHolders(mount)
		}
		exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-f", mount).Run()
		exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-l", mount).Run()
		exec.Command("umount", "-f", mount).Run()
		exec.Command("umount", "-l", mount).Run()
	}
}

func destroyZPoolsUsing(path string) {
	pools, err := exec.Command("zpool", "list", "-H", "-o", "name").Output()
	if err != nil {
		return
	}
	for _, pool := range strings.Fields(string(pools)) {
		status, err := exec.Command("zpool", "status", "-LP", pool).Output()
		if err != nil || !zpoolStatusMentionsAny(string(status), deviceTree(path)) {
			continue
		}
		releaseZPoolMounts(pool)
		if out, err := exec.Command("zpool", "destroy", "-f", pool).CombinedOutput(); err != nil {
			log.Printf("disk wipe: zpool destroy -f %s: %v (out=%q)", pool, err, strings.TrimSpace(string(out)))
			exec.Command("zpool", "export", "-f", pool).Run()
		}
	}
}

func zpoolStatusMentionsAny(status string, devices []string) bool {
	for _, dev := range devices {
		if dev != "" && strings.Contains(status, dev) {
			return true
		}
	}
	return false
}

func releaseZPoolMounts(pool string) {
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
		}
		exec.Command("zfs", "unmount", "-f", dataset).Run()
	}
}

var mdMemberRE = regexp.MustCompile(`^([A-Za-z0-9_./-]+)\[[0-9]+]`)

func stopMDArraysUsing(path string) {
	mdstat, err := os.ReadFile("/proc/mdstat")
	if err != nil {
		return
	}
	devices := map[string]bool{}
	for _, dev := range deviceTree(path) {
		devices[strings.TrimPrefix(dev, "/dev/")] = true
	}
	for _, line := range strings.Split(string(mdstat), "\n") {
		if !strings.Contains(line, " : ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		mdName := fields[0]
		for _, tok := range fields[1:] {
			m := mdMemberRE.FindStringSubmatch(tok)
			if len(m) == 2 && devices[m[1]] {
				exec.Command("mdadm", "--stop", "/dev/"+mdName).Run()
				break
			}
		}
	}
}

func deactivateLVMUsing(path string) {
	for _, dev := range deviceTree(path) {
		out, err := exec.Command("pvs", "--noheadings", "-o", "vg_name", dev).Output()
		if err != nil {
			continue
		}
		for _, vg := range strings.Fields(string(out)) {
			exec.Command("vgchange", "-an", "--force", vg).Run()
			exec.Command("pvremove", "-ff", "-y", dev).Run()
		}
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
		log.Printf("disk wipe: fuser -km %s: %v (out=%q)", path, err, strings.TrimSpace(string(out)))
	}
}

// Wipe removes all filesystem signatures, partition tables, and RAID
// superblocks from a disk. The disk must not be an OS disk or currently
// assigned to an array/pool.
func Wipe(path string) error {
	if !isValidDiskPath(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}
	return WipeDevice(path)
}

func PowerStatusFor(d Disk, timerMinutes int) PowerStatus {
	status := PowerStatus{
		State:         "unknown",
		Eligible:      true,
		TimerMinutes:  timerMinutes,
		IneligibleWhy: "",
	}
	if !d.Rotational {
		status.Eligible = false
		status.IneligibleWhy = "not rotational media"
	} else if d.Assignment == "os" {
		status.Eligible = false
		status.IneligibleWhy = "OS disk"
	}
	if state, err := QueryPowerState(d.Path); err == nil {
		status.State = state
	}
	return status
}

func QueryPowerState(path string) (string, error) {
	if !isValidDiskPath(path) {
		return "", fmt.Errorf("invalid disk path: %s", path)
	}
	out, err := runPowerCommand("hdparm", "-C", path)
	if err != nil {
		return "", fmt.Errorf("hdparm -C: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return ParsePowerState(string(out)), nil
}

func ParsePowerState(out string) string {
	text := strings.ToLower(out)
	switch {
	case strings.Contains(text, "standby"):
		return "standby"
	case strings.Contains(text, "sleeping"):
		return "sleeping"
	case strings.Contains(text, "active/idle"), strings.Contains(text, "active or idle"):
		return "active"
	case strings.Contains(text, "idle"):
		return "idle"
	default:
		return "unknown"
	}
}

func StandbyTimerValue(minutes int) (int, error) {
	if minutes == 0 {
		return 0, nil
	}
	if minutes < 0 || minutes > 330 {
		return 0, fmt.Errorf("idle_minutes must be between 0 and 330")
	}
	seconds := minutes * 60
	if seconds <= 240*5 {
		value := seconds / 5
		if seconds%5 != 0 {
			value++
		}
		if value < 1 {
			value = 1
		}
		return value, nil
	}
	value := 240 + minutes/30
	if minutes%30 != 0 {
		value++
	}
	if value < 241 {
		value = 241
	}
	if value > 251 {
		value = 251
	}
	return value, nil
}

func SetSpindownTimer(path string, minutes int) error {
	if !isValidDiskPath(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}
	value, err := StandbyTimerValue(minutes)
	if err != nil {
		return err
	}
	if out, err := runPowerCommand("hdparm", "-S", strconv.Itoa(value), path); err != nil {
		return fmt.Errorf("hdparm -S: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func DisableAPM(path string) error {
	if !isValidDiskPath(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}
	if out, err := runPowerCommand("hdparm", "-B", "255", path); err != nil {
		return fmt.Errorf("hdparm -B: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func StandbyNow(path string) error {
	if !isValidDiskPath(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}
	if out, err := runPowerCommand("hdparm", "-y", path); err != nil {
		return fmt.Errorf("hdparm -y: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Identify blinks a disk's activity LED for physical identification.
// Uses ledctl if available, falls back to a brief read pattern.
func Identify(path string) error {
	if !isValidDiskPath(path) {
		return fmt.Errorf("invalid disk path: %s", path)
	}

	// Try ledctl first (for SAS/SCSI enclosures).
	if _, err := exec.LookPath("ledctl"); err == nil {
		cmd := exec.Command("ledctl", "locate="+path)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}

	// Fallback: no-op with a message. Real LED control depends on hardware.
	return fmt.Errorf("LED identification not supported for %s (no enclosure management)", path)
}

// isValidDiskPath validates a disk path against the allowlist.
func isValidDiskPath(path string) bool {
	// /dev/sd[a-z]+
	if strings.HasPrefix(path, "/dev/sd") {
		rest := path[7:]
		if len(rest) == 0 {
			return false
		}
		for _, c := range rest {
			if c < 'a' || c > 'z' {
				return false
			}
		}
		return true
	}

	// /dev/nvme[0-9]+n[0-9]+
	if strings.HasPrefix(path, "/dev/nvme") {
		rest := path[9:]
		parts := strings.SplitN(rest, "n", 2)
		if len(parts) != 2 {
			return false
		}
		return isDigits(parts[0]) && isDigits(parts[1])
	}

	return false
}

func isDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
