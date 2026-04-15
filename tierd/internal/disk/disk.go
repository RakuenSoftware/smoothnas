// Package disk provides block device discovery via lsblk.
package disk

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Disk represents a physical block device.
type Disk struct {
	Name       string `json:"name"`        // e.g. "sda", "nvme0n1"
	Path       string `json:"path"`        // e.g. "/dev/sda"
	Type       string `json:"type"`        // "disk" (we filter to this)
	Size       uint64 `json:"size"`        // bytes
	SizeHuman  string `json:"size_human"`  // e.g. "1.8T"
	Model      string `json:"model"`       // drive model string
	Serial     string `json:"serial"`      // drive serial number
	Transport  string `json:"transport"`   // "sata", "nvme", "sas", "usb", etc.
	Rotational bool   `json:"rotational"`  // true = HDD, false = SSD/NVMe
	DriveType  string `json:"drive_type"`  // "HDD", "SSD", or "NVMe"
	Mountpoint string `json:"mountpoint"`  // non-empty if any partition is mounted
	Assignment string `json:"assignment"`  // "unassigned", "os", "mdadm-array", "zfs-pool", etc.
}

// lsblkDevice matches the JSON output of lsblk --json.
type lsblkDevice struct {
	Name       string         `json:"name"`
	Path       string         `json:"path"`
	Type       string         `json:"type"`
	Size       json.Number    `json:"size"`
	Model      string         `json:"model"`
	Serial     string         `json:"serial"`
	Tran       string         `json:"tran"`
	Rota       bool           `json:"rota"`
	Mountpoint string         `json:"mountpoint"`
	Children   []lsblkDevice  `json:"children"`
}

type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

// List enumerates all physical block devices on the system.
func List() ([]Disk, error) {
	out, err := exec.Command(
		"lsblk", "--json", "--bytes", "--output",
		"NAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,ROTA,MOUNTPOINT",
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
		assignment := "unassigned"
		if isOSDisk(dev) {
			assignment = "os"
		} else if label, ok := assigned[dev.Path]; ok {
			assignment = label
		}

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

// assignedDisks returns a map of base disk path → assignment label for any disk
// that is a member of a ZFS pool or mdadm array.
func assignedDisks() map[string]string {
	result := make(map[string]string)

	// ZFS pool members: parse `zpool status -P` for /dev/ paths.
	if out, err := exec.Command("zpool", "status", "-P").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if strings.HasPrefix(fields[0], "/dev/") {
				result[baseDiskPath(fields[0])] = "zfs-pool"
			}
		}
	}

	// mdadm array members: parse /proc/mdstat for sdX[N] / nvmeXnYpZ[N] tokens.
	if data, err := os.ReadFile("/proc/mdstat"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, " : ") {
				continue
			}
			for _, tok := range strings.Fields(line) {
				if idx := strings.Index(tok, "["); idx > 0 {
					name := tok[:idx]
					if strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "hd") {
						result[baseDiskPath("/dev/"+name)] = "mdadm"
					}
				}
			}
		}
	}

	return result
}

// baseDiskPath strips any partition suffix from a device path to return the
// whole-disk path. e.g. /dev/sda1 → /dev/sda, /dev/nvme0n1p2 → /dev/nvme0n1.
func baseDiskPath(path string) string {
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

// WipeDevice removes all filesystem signatures, partition tables, and RAID
// superblocks from a block device. All three steps run unconditionally so that
// a failure in one does not prevent the others from running. partx removal is
// best-effort. The caller is responsible for validating the path.
func WipeDevice(path string) error {
	var errs []string

	// Wipe all filesystem, LVM, and other signatures.
	if out, err := exec.Command("wipefs", "-a", path).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Sprintf("wipefs -a: %s: %v", strings.TrimSpace(string(out)), err))
	}

	// Zap partition tables (MBR + GPT including backup copy).
	if out, err := exec.Command("/sbin/sgdisk", "--zap-all", path).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Sprintf("sgdisk --zap-all: %s: %v", strings.TrimSpace(string(out)), err))
	}

	// Zero any mdadm superblock.
	if out, err := exec.Command("mdadm", "--zero-superblock", path).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Sprintf("mdadm --zero-superblock: %s: %v", strings.TrimSpace(string(out)), err))
	}

	// Remove partition mappings from the kernel (best-effort; may not exist).
	exec.Command("partx", "-d", path).Run()

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
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
