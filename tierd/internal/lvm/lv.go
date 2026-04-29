package lvm

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var fstabPath = "/etc/fstab"

// Volume represents a single logical volume in a pool VG,
// along with its placement tier (derived from the PVs that back its
// extents) and any mount/filesystem metadata.
type Volume struct {
	Name       string `json:"name"`
	VGName     string `json:"vg_name"`
	Size       string `json:"size"`
	MountPoint string `json:"mount_point,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	Mounted    bool   `json:"mounted"`
	// ActualTier is the tier whose PV backs this LV's extents. Empty when
	// the LV spans multiple tiers (should not happen in Phase 1) or when
	// the placement cannot be resolved.
	ActualTier string `json:"actual_tier,omitempty"`
}

// BuildCreateLVArgs returns the lvcreate arg list for creating an LV in vg
// with the given size, optionally pinned to a specific PV device. A
// percentage size ("100%FREE", "50%VG") is passed via -l; an absolute size
// ("20G", "500M") is passed via -L. When pvDevice is non-empty, it is
// appended as the final positional argument so LVM constrains allocation to
// that PV.
func BuildCreateLVArgs(vg, name, size, pvDevice string) []string {
	pvDevices := []string{}
	if pvDevice != "" {
		pvDevices = append(pvDevices, pvDevice)
	}
	return BuildCreateLVArgsForPVs(vg, name, size, pvDevices)
}

// BuildCreateLVArgsForPVs returns the lvcreate arg list for creating an LV in
// vg with placement constrained to the provided PV device paths in order.
func BuildCreateLVArgsForPVs(vg, name, size string, pvDevices []string) []string {
	flag := "-L"
	if strings.Contains(size, "%") {
		flag = "-l"
	}
	args := []string{"-y", "-W", "y", "-Z", "y", flag, size, "-n", name, vg}
	args = append(args, pvDevices...)
	return args
}

// CreateLV creates a logical volume in the given VG. When pvDevice is
// non-empty, allocation is constrained to that PV (the tier enforcement
// boundary).
func CreateLV(vg, name, size, pvDevice string) error {
	pvDevices := []string{}
	if pvDevice != "" {
		pvDevices = append(pvDevices, pvDevice)
	}
	return CreateLVOnDevices(vg, name, size, pvDevices)
}

// CreateLVOnDevices creates a linear logical volume constrained to the given
// PV device paths in the order provided.
func CreateLVOnDevices(vg, name, size string, pvDevices []string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	for _, pvDevice := range pvDevices {
		if err := ValidateDevicePath(pvDevice); err != nil {
			return err
		}
	}
	args := BuildCreateLVArgsForPVs(vg, name, size, pvDevices)
	cmd := exec.Command("lvcreate", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvcreate: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// findTool resolves a binary name by searching PATH first, then the sbin
// directories that systemd service units may omit from $PATH.
func findTool(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin"} {
		candidate := dir + "/" + name
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return name // fall through; exec will produce a clear "not found" error
}

// RepairFilesystem runs fsck -y on an LV to repair a dirty filesystem.
// This is safe to call before a mount when the kernel rejects the mount with
// EUCLEAN ("Structure needs cleaning"), which can happen after an unclean
// shutdown or a partially-completed prior provision.
func RepairFilesystem(vg, name string) error {
	dev := "/dev/" + vg + "/" + name
	cmd := exec.Command("fsck", "-y", dev)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fsck %s: %s: %w", dev, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// FormatLV runs mkfs on an LV with the given filesystem.
func FormatLV(vg, name, fs string) error {
	if err := ValidateFilesystem(fs); err != nil {
		return err
	}
	dev := "/dev/" + vg + "/" + name
	var cmd *exec.Cmd
	switch fs {
	case "xfs":
		// -m reflink=0,rmapbt=0 drops two per-AG B-trees we do not use on
		// tier backing filesystems. At multi-TB scales those B-trees reserve
		// ~2% of the device up-front (statvfs reports it as "used"), which
		// looks alarming on a freshly-created empty tier — e.g. ~640 GB on a
		// 33 TB tier. Reflink is for CoW / snapshot clones; rmapbt is for
		// xfs_scrub repair. Neither is needed here.
		cmd = exec.Command(findTool("mkfs.xfs"), "-f",
			"-m", "reflink=0,rmapbt=0", dev)
	case "ext4":
		cmd = exec.Command(findTool("mkfs.ext4"), "-F", dev)
	default:
		return fmt.Errorf("unsupported filesystem %q", fs)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.%s: %s: %w", fs, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// LVExists reports whether an LV named `name` exists in `vg`.
func LVExists(vg, name string) (bool, error) {
	cmd := exec.Command("lvs", "--noheadings", vg+"/"+name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// LVHasFilesystem reports whether the given LV already carries a filesystem
// superblock (as detected by blkid). Used to skip mkfs on already-formatted
// volumes.
func LVHasFilesystem(vg, name string) bool {
	dev := "/dev/" + vg + "/" + name
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", dev).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// ExtendLVFull extends the LV to consume all remaining free space in its VG
// and resizes the filesystem in place. Best-effort: if there is no free space
// the command returns an error which the caller may choose to ignore.
func ExtendLVFull(vg, name string) error {
	cmd := exec.Command("lvextend", "-l", "+100%FREE", "-r", vg+"/"+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvextend %s/%s: %s: %w", vg, name, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// IsMounted reports whether path is an active mount point.
// It checks the host mount namespace first (tierd runs with PrivateTmp=true)
// and falls back to the process namespace for non-namespaced environments.
func IsMounted(path string) bool {
	if exec.Command("nsenter", "-t", "1", "-m", "--", "mountpoint", "-q", path).Run() == nil {
		return true
	}
	return exec.Command("mountpoint", "-q", path).Run() == nil
}

// DeactivateLV force-deactivates a logical volume so it can be removed even
// when the filesystem is still in use (e.g. after a lazy unmount).
func DeactivateLV(vg, name string) error {
	cmd := exec.Command("lvchange", "-an", "--force", vg+"/"+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvchange -an: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveLV destroys a logical volume.
func RemoveLV(vg, name string) error {
	cmd := exec.Command("lvremove", "-f", vg+"/"+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvremove: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ExtendLV grows a logical volume by `size` bytes (or %) on the given PV.
// When pvDevice is non-empty it is appended so new extents are allocated
// from that PV, keeping placement on the intended tier.
func ExtendLV(vg, name, size, pvDevice string) error {
	args := []string{}
	if strings.Contains(size, "%") {
		args = append(args, "-l", size)
	} else {
		args = append(args, "-L", size)
	}
	args = append(args, vg+"/"+name)
	if pvDevice != "" {
		args = append(args, pvDevice)
	}
	cmd := exec.Command("lvextend", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvextend: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// GrowFilesystem expands the filesystem on an already-grown LV.
func GrowFilesystem(vg, name, mountPoint, fs string) error {
	if err := ValidateFilesystem(fs); err != nil {
		return err
	}
	dev := "/dev/" + vg + "/" + name
	var cmd *exec.Cmd
	switch fs {
	case "xfs":
		cmd = exec.Command(findTool("xfs_growfs"), mountPoint)
	case "ext4":
		cmd = exec.Command(findTool("resize2fs"), dev)
	default:
		return fmt.Errorf("unsupported filesystem %q", fs)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("grow filesystem %s on %s: %s: %w", fs, dev, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// EnsureFSTabEntry makes sure the LV is persisted in /etc/fstab.
func EnsureFSTabEntry(vg, name, mountPoint, fs string) error {
	if err := ValidateFilesystem(fs); err != nil {
		return err
	}
	if err := ValidateMountPoint(mountPoint); err != nil {
		return err
	}
	dev := "/dev/" + vg + "/" + name
	entry := fmt.Sprintf("%s %s %s defaults,nofail 0 0", dev, mountPoint, fs)

	data, err := os.ReadFile(fstabPath)
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 2 && (fields[0] == dev || fields[1] == mountPoint) {
				if line == entry {
					return nil
				}
				lines[i] = entry
				output := strings.Join(lines, "\n")
				if !strings.HasSuffix(output, "\n") {
					output += "\n"
				}
				if err := os.WriteFile(fstabPath, []byte(output), 0644); err != nil {
					return fmt.Errorf("write /etc/fstab: %w", err)
				}
				return nil
			}
		}
	}

	f, err := os.OpenFile(fstabPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open /etc/fstab: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("append /etc/fstab: %w", err)
	}
	return nil
}

// RemoveFSTabEntry removes any fstab entry for the given LV or mount point.
func RemoveFSTabEntry(vg, name, mountPoint string) error {
	if err := ValidateMountPoint(mountPoint); err != nil {
		return err
	}
	dev := "/dev/" + vg + "/" + name

	data, err := os.ReadFile(fstabPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read /etc/fstab: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == dev || fields[1] == mountPoint) {
			continue
		}
		filtered = append(filtered, line)
	}
	output := strings.Join(filtered, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	if err := os.WriteFile(fstabPath, []byte(output), 0644); err != nil {
		return fmt.Errorf("write /etc/fstab: %w", err)
	}
	return nil
}

// VerifyLVSegmentOrder checks that LV segments are ordered from lower rank
// (faster) PVs to higher rank (slower) PVs.
func VerifyLVSegmentOrder(vg, name string, deviceRanks map[string]int) error {
	segments, err := ListLVSegments(vg, name)
	if err != nil {
		return err
	}
	prevRank := -1
	for _, segment := range segments {
		rank, ok := deviceRanks[segment.PVPath]
		if !ok {
			return fmt.Errorf("segment device %s is not assigned to the pool", segment.PVPath)
		}
		if prevRank > rank {
			return fmt.Errorf("segment_order_violation")
		}
		prevRank = rank
	}
	return nil
}

// ResizeLV resizes a logical volume and its filesystem to an absolute size.
// Used for shrinks (lvextend cannot shrink); for grows in a tier-aware
// context, prefer ExtendLV with an explicit PV.
func ResizeLV(vg, name, size string) error {
	cmd := exec.Command("lvresize", "-r", "-L", size, vg+"/"+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvresize: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Mount mounts a logical volume at the given mount point, creating the
// directory if it does not already exist.
func Mount(vg, name, mountPoint string) error {
	if err := ValidateMountPoint(mountPoint); err != nil {
		return err
	}
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPoint, err)
	}
	dev := "/dev/" + vg + "/" + name
	// Use nsenter to escape tierd's private mount namespace (PrivateTmp=true)
	// so the mount is visible to all processes on the host.
	cmd := exec.Command("nsenter", "-t", "1", "-m", "--", "mount", dev, mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fallback: try plain mount (works when not inside a private namespace).
		cmd2 := exec.Command("mount", dev, mountPoint)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("mount: %s: %w", strings.TrimSpace(string(out)+"\n"+string(out2)), err2)
		}
	}
	return nil
}

// Unmount unmounts the filesystem at the given mount point.
// It uses nsenter to reach the host mount namespace (tierd runs with
// PrivateTmp=true) and falls back to plain umount for non-namespaced envs.
func Unmount(mountPoint string) error {
	cmd := exec.Command("nsenter", "-t", "1", "-m", "--", "umount", mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		cmd2 := exec.Command("umount", mountPoint)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("umount: %s: %w", strings.TrimSpace(string(out)+"\n"+string(out2)), err2)
		}
	}
	return nil
}

// LazyUnmount detaches a busy mount point while allowing active references to
// drain naturally. It uses nsenter to reach the host mount namespace (tierd
// runs with PrivateTmp=true) and falls back to plain umount -l for
// non-namespaced envs.
func LazyUnmount(mountPoint string) error {
	cmd := exec.Command("nsenter", "-t", "1", "-m", "--", "umount", "-l", mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		cmd2 := exec.Command("umount", "-l", mountPoint)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("umount -l: %s: %w", strings.TrimSpace(string(out)+"\n"+string(out2)), err2)
		}
	}
	return nil
}

// ListLVs returns all LVs in the given VG. Each LV's ActualTier is resolved
// from its segment PV placement via pvByTier, which maps PV device paths to
// tier names.
func ListLVs(vg string, pvByTier map[string]string) ([]Volume, error) {
	out, err := exec.Command(
		"lvs",
		"--noheadings", "--nosuffix", "--separator", "|",
		"-o", "lv_name,vg_name,lv_size,devices",
		vg,
	).Output()
	if err != nil {
		return nil, nil
	}

	mounts := readMounts()

	var volumes []Volume
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			continue
		}
		v := Volume{
			Name:   strings.TrimSpace(fields[0]),
			VGName: strings.TrimSpace(fields[1]),
			Size:   strings.TrimSpace(fields[2]),
		}
		v.ActualTier = tierFromDevices(fields[3], pvByTier)

		dev := "/dev/" + v.VGName + "/" + v.Name
		if mp, ok := mounts[dev]; ok {
			v.MountPoint = mp
			v.Mounted = true
		}
		volumes = append(volumes, v)
	}
	return volumes, nil
}

// tierFromDevices parses the lvs "devices" field and returns the tier that
// backs the LV's extents. The field looks like "/dev/md0(0)" for a single PV
// or "/dev/md0(0),/dev/md1(100)" when multiple PVs are involved. Phase 1
// does not permit mixed-tier LVs, so we return the tier only when all PVs
// map to the same tier; otherwise we return "" (unknown).
func tierFromDevices(devicesField string, pvByTier map[string]string) string {
	devicesField = strings.TrimSpace(devicesField)
	if devicesField == "" || len(pvByTier) == 0 {
		return ""
	}

	var seen string
	for _, part := range strings.Split(devicesField, ",") {
		part = strings.TrimSpace(part)
		// Strip "(N)" suffix indicating the starting PE.
		if i := strings.Index(part, "("); i > 0 {
			part = part[:i]
		}
		tier := pvByTier[part]
		if tier == "" {
			return ""
		}
		if seen == "" {
			seen = tier
		} else if seen != tier {
			return "" // mixed placement
		}
	}
	return seen
}

// LVSizeBytes returns the logical size of an LV in bytes by shelling out to
// `lvs -o lv_size --units b --nosuffix`. Returns 0 on lookup failure.
func LVSizeBytes(vg, name string) (uint64, error) {
	out, err := exec.Command(
		"lvs", "--noheadings", "--nosuffix", "--units", "b",
		"-o", "lv_size", vg+"/"+name,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("lvs size %s/%s: %w", vg, name, err)
	}
	return parseUint(strings.TrimSpace(string(out))), nil
}

// LVHealthy reports whether the LV has no missing or partial extents. An empty
// lv_health_status field means the LV is fully accessible; any non-empty value
// (e.g. "partial") indicates extent loss.
func LVHealthy(vg, name string) (bool, error) {
	out, err := exec.Command(
		"lvs", "--noheadings", "-o", "lv_health_status", vg+"/"+name,
	).Output()
	if err != nil {
		return false, fmt.Errorf("lvs health %s/%s: %w", vg, name, err)
	}
	return strings.TrimSpace(string(out)) == "", nil
}

// PVResize informs LVM that the underlying block device has grown and updates
// the PV metadata to claim the new space.
func PVResize(device string) error {
	cmd := exec.Command("pvresize", device)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pvresize %s: %s: %w", device, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// MountedByDevice returns the block device currently mounted at mountPoint, or
// an empty string when nothing is mounted there.
func MountedByDevice(mountPoint string) string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mountPoint {
			return fields[0]
		}
	}
	return ""
}

// readMounts reads /proc/mounts and returns a device→mountpoint map.
func readMounts() map[string]string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			result[fields[0]] = fields[1]
		}
	}
	return result
}
