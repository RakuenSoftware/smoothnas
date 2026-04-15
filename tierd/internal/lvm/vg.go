package lvm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	placeholderTag    = "smoothnas-placeholder"
	placeholderRunDir = "/run/smoothnas/vg"
	placeholderSize   = 8 * 1024 * 1024 // 8 MiB — enough for LVM PV metadata
)

const (
	poolTagPrefix = "smoothnas-pool:"
	tierTagPrefix = "smoothnas-tier:"
)

// CreatePV initialises a block device as an LVM physical volume. It is
// idempotent: if the device is already a PV, -ff forces re-init only when
// empty metadata is present.
func CreatePV(devicePath string) error {
	if err := ValidateDevicePath(devicePath); err != nil {
		return err
	}
	cmd := exec.Command("pvcreate", "-f", devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pvcreate %s: %s: %w", devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// WipeSignatures clears filesystem and partition signatures from a device
// before it is reinitialized as an LVM PV.
func WipeSignatures(devicePath string) error {
	if err := ValidateDevicePath(devicePath); err != nil {
		return err
	}
	cmd := exec.Command("wipefs", "-a", devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wipefs -a %s: %s: %w", devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// PVLookup reports whether a device is already known to LVM and, if so, which
// volume group it belongs to. VGName is empty when the PV is not yet attached
// to a VG.
type PVLookup struct {
	Device string
	VGName string
}

// LookupPV returns the LVM state for a specific device, or nil when the device
// is not an LVM PV yet.
func LookupPV(devicePath string) (*PVLookup, error) {
	if err := ValidateDevicePath(devicePath); err != nil {
		return nil, err
	}
	out, err := exec.Command(
		"pvs",
		"--noheadings", "--separator", "|",
		"-o", "pv_name,vg_name",
		devicePath,
	).CombinedOutput()
	if err != nil {
		return nil, nil
	}
	return parsePVLookup(string(out)), nil
}

func parsePVLookup(out string) *PVLookup {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 1 {
			continue
		}
		pv := &PVLookup{Device: strings.TrimSpace(fields[0])}
		if len(fields) > 1 {
			pv.VGName = strings.TrimSpace(fields[1])
		}
		if pv.Device != "" {
			return pv
		}
	}
	return nil
}

// RemovePV clears the LVM label from a device.
func RemovePV(devicePath string) error {
	if err := ValidateDevicePath(devicePath); err != nil {
		return err
	}
	cmd := exec.Command("pvremove", "-ff", "-y", devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pvremove %s: %s: %w", devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// PVFreeExtents returns the number of free physical extents on a PV. Used
// by the migration engine to confirm a destination has headroom for an
// incoming pvmove before kicking it off.
func PVFreeExtents(devicePath string) (uint64, error) {
	if err := ValidateDevicePath(devicePath); err != nil {
		return 0, err
	}
	out, err := exec.Command(
		"pvs", "--noheadings", "--nosuffix",
		"-o", "pv_free_count", devicePath,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("pvs %s: %w", devicePath, err)
	}
	return parseUint(string(out)), nil
}

// PVAllocatedExtents returns the number of allocated physical extents on a PV.
func PVAllocatedExtents(devicePath string) (uint64, error) {
	if err := ValidateDevicePath(devicePath); err != nil {
		return 0, err
	}
	out, err := exec.Command("pvdisplay", "-c", devicePath).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("pvdisplay -c %s: %s: %w", devicePath, strings.TrimSpace(string(out)), err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected pvdisplay output for %s", devicePath)
	}
	allocatedField := strings.TrimSpace(fields[len(fields)-2])
	allocated, err := strconv.ParseUint(allocatedField, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse allocated extents %q for %s: %w", allocatedField, devicePath, err)
	}
	return allocated, nil
}

// VGExists reports whether the given volume group already exists.
func VGExists(vgName string) (bool, error) {
	cmd := exec.Command("vgs", "--noheadings", "-o", "vg_name", vgName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// vgs returns non-zero when the VG does not exist.
		return false, nil
	}
	return strings.TrimSpace(string(out)) == vgName, nil
}

// PVInVG reports whether devicePath is already a member of vgName.
func PVInVG(devicePath, vgName string) (bool, error) {
	out, err := exec.Command(
		"pvs", "--noheadings", "-o", "vg_name",
		"--select", "pv_name="+devicePath,
	).Output()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == vgName, nil
}

// EnsureVG creates vgName with devicePath as its first PV if the VG does not
// yet exist, or extends it if the device is not already a member. The device
// must already be pvcreate'd before calling this function.
func EnsureVG(vgName, devicePath string) error {
	exists, err := VGExists(vgName)
	if err != nil {
		return err
	}
	if !exists {
		cmd := exec.Command("vgcreate", vgName, devicePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("vgcreate %s: %s: %w", vgName, strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	// Only extend if the device is not already in this VG.
	already, err := PVInVG(devicePath, vgName)
	if err != nil {
		return err
	}
	if already {
		return nil
	}
	return VGExtend(vgName, devicePath)
}

// VGExtend adds a PV to an existing volume group.
func VGExtend(vgName, devicePath string) error {
	cmd := exec.Command("vgextend", vgName, devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vgextend %s %s: %s: %w", vgName, devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// VGReduce removes a PV from a volume group. The PV must have no extents in
// use; callers should verify this before invoking.
func VGReduce(vgName, devicePath string) error {
	cmd := exec.Command("vgreduce", vgName, devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vgreduce %s %s: %s: %w", vgName, devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// VGRemove force-removes a volume group. Callers must ensure it has no active
// logical volumes they care about.
func VGRemove(vgName string) error {
	cmd := exec.Command("vgremove", "-f", vgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vgremove %s: %s: %w", vgName, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// VGRemoveIfEmpty removes the volume group entirely when it has no remaining
// PVs. Used when the last tier is cleared from the appliance.
func VGRemoveIfEmpty(vgName string) error {
	// Check if the VG has any PVs left.
	out, err := exec.Command("vgs", "--noheadings", "-o", "pv_count", vgName).CombinedOutput()
	if err != nil {
		// VG does not exist — nothing to do.
		return nil
	}
	if strings.TrimSpace(string(out)) != "0" {
		return nil
	}
	return VGRemove(vgName)
}

// PoolTag returns the managed-pool identity tag stored on a PV.
func PoolTag(poolName string) string {
	return poolTagPrefix + poolName
}

// TierTag returns the managed-tier identity tag stored on a PV.
func TierTag(tierName string) string {
	return tierTagPrefix + strings.ToLower(tierName)
}

// AddPVTags adds the managed pool and tier identity tags to a PV.
func AddPVTags(devicePath, poolName, tierName string) error {
	if err := ValidateDevicePath(devicePath); err != nil {
		return err
	}
	poolTag := PoolTag(poolName)
	tierTag := TierTag(tierName)
	cmd := exec.Command("pvchange", "--addtag", poolTag, "--addtag", tierTag, devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pvchange --addtag %s --addtag %s %s: %s: %w", poolTag, tierTag, devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemovePVTags removes the managed pool and tier identity tags from a PV.
func RemovePVTags(devicePath, poolName, tierName string) error {
	if err := ValidateDevicePath(devicePath); err != nil {
		return err
	}
	poolTag := PoolTag(poolName)
	tierTag := TierTag(tierName)
	cmd := exec.Command("pvchange", "--deltag", poolTag, "--deltag", tierTag, devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pvchange --deltag %s --deltag %s %s: %s: %w", poolTag, tierTag, devicePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ManagedPV describes a PV that belongs to a SmoothNAS-managed tier pool.
type ManagedPV struct {
	Device   string `json:"device"`
	VGName   string `json:"vg_name"`
	PoolName string `json:"pool_name"`
	TierName string `json:"tier_name"`
}

// ListManagedPVs returns all PVs that advertise a SmoothNAS pool tag. This is
// the discovery surface used by boot-time reconciliation.
func ListManagedPVs() ([]ManagedPV, error) {
	out, err := exec.Command(
		"pvs",
		"--noheadings", "--separator", "|",
		"-o", "pv_name,vg_name,pv_tags",
	).Output()
	if err != nil {
		return nil, err
	}
	return parseManagedPVs(string(out)), nil
}

func parseManagedPVs(out string) []ManagedPV {
	var managed []ManagedPV
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 3 {
			continue
		}
		device := strings.TrimSpace(fields[0])
		vgName := strings.TrimSpace(fields[1])
		tags := parseTagList(fields[2])
		poolName := managedTagValue(tags, poolTagPrefix)
		if poolName == "" {
			continue
		}
		managed = append(managed, ManagedPV{
			Device:   device,
			VGName:   vgName,
			PoolName: poolName,
			TierName: managedTagValue(tags, tierTagPrefix),
		})
	}
	return managed
}

func parseTagList(tags string) []string {
	rawTags := strings.Split(strings.TrimSpace(tags), ",")
	parsed := make([]string, 0, len(rawTags))
	for _, tag := range rawTags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			parsed = append(parsed, tag)
		}
	}
	return parsed
}

func managedTagValue(tags []string, prefix string) string {
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			return strings.TrimPrefix(tag, prefix)
		}
	}
	return ""
}

// PVInfo is a snapshot of a single PV in a pool VG.
type PVInfo struct {
	Device    string `json:"device"`
	SizeBytes uint64 `json:"size_bytes"`
	FreeBytes uint64 `json:"free_bytes"`
	UsedBytes uint64 `json:"used_bytes"`
	Tags      string `json:"tags"` // comma-separated tags
}

// ListPVsInVG returns the PVs that belong to the given VG along with their
// size, free space, and tags. Sizes are in bytes (the --units b flag).
func ListPVsInVG(vgName string) ([]PVInfo, error) {
	out, err := exec.Command(
		"pvs",
		"--noheadings", "--nosuffix", "--units", "b",
		"--separator", "|",
		"-o", "pv_name,pv_size,pv_free,pv_tags",
		"--select", "vg_name="+vgName,
	).Output()
	if err != nil {
		// Not being able to query is treated as an empty list, e.g. when the
		// VG does not exist yet.
		return nil, nil
	}

	var pvs []PVInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			continue
		}
		pv := PVInfo{
			Device:    strings.TrimSpace(fields[0]),
			SizeBytes: parseUint(fields[1]),
			FreeBytes: parseUint(fields[2]),
			Tags:      strings.TrimSpace(fields[3]),
		}
		if pv.SizeBytes >= pv.FreeBytes {
			pv.UsedBytes = pv.SizeBytes - pv.FreeBytes
		}
		pvs = append(pvs, pv)
	}
	return pvs, nil
}

// parseUint parses an unsigned integer from a byte-count string, returning 0
// on parse failure.
func parseUint(s string) uint64 {
	var n uint64
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}

// cleanupStaleLoopDevices detaches any loop devices whose backing file
// matches imgPath (including deleted backing files). This prevents LVM
// duplicate-PV errors when a previous destroy left an orphaned loop device.
func cleanupStaleLoopDevices(imgPath string) {
	out, err := exec.Command("losetup", "-j", imgPath, "--noheadings", "-O", "NAME").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dev := strings.TrimSpace(line)
		if dev == "" {
			continue
		}
		exec.Command("pvremove", "-ff", "-y", dev).Run()
		exec.Command("losetup", "-d", dev).Run()
	}
}

// VGCreateEmpty creates an empty volume group for vgName using a temporary
// loopback-backed placeholder PV. The placeholder PV is tagged with
// smoothnas-placeholder so ProvisionStorage can remove it when the first real
// PV is added. Backing files live under /run/smoothnas/vg/ (tmpfs on most
// systems) and are cleaned up automatically on reboot; boot-time
// reconciliation recreates VG state from the real PV tags at that point.
//
// VGCreateEmpty is idempotent: if the VG already exists it returns nil.
func VGCreateEmpty(vgName string) error {
	exists, err := VGExists(vgName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	if err := os.MkdirAll(placeholderRunDir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", placeholderRunDir, err)
	}
	imgPath := filepath.Join(placeholderRunDir, vgName+".img")

	// Clean up any orphaned loop devices from a previous failed destroy
	// before creating a new placeholder, to avoid LVM duplicate-PV errors.
	cleanupStaleLoopDevices(imgPath)

	f, err := os.Create(imgPath)
	if err != nil {
		return fmt.Errorf("create placeholder image: %w", err)
	}
	if err := f.Truncate(placeholderSize); err != nil {
		f.Close()
		os.Remove(imgPath)
		return fmt.Errorf("size placeholder image: %w", err)
	}
	f.Close()

	out, err := exec.Command("losetup", "-f", "--show", imgPath).Output()
	if err != nil {
		os.Remove(imgPath)
		return fmt.Errorf("losetup: %w", err)
	}
	loopDev := strings.TrimSpace(string(out))

	cleanup := func() {
		exec.Command("losetup", "-d", loopDev).Run()
		os.Remove(imgPath)
	}

	if out, err := exec.Command("pvcreate", loopDev).CombinedOutput(); err != nil {
		cleanup()
		return fmt.Errorf("pvcreate placeholder: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if out, err := exec.Command("vgcreate", vgName, loopDev).CombinedOutput(); err != nil {
		exec.Command("pvremove", "-ff", "-y", loopDev).Run()
		cleanup()
		return fmt.Errorf("vgcreate %s: %s: %w", vgName, strings.TrimSpace(string(out)), err)
	}

	if out, err := exec.Command("pvchange", "--addtag", placeholderTag, loopDev).CombinedOutput(); err != nil {
		exec.Command("vgremove", "-ff", "-y", vgName).Run()
		cleanup()
		return fmt.Errorf("pvchange placeholder tag: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// VGRemovePlaceholder removes the loopback-backed placeholder PV from vgName
// if one is present, making the VG ready for real PVs. It is a no-op when the
// VG has no placeholder PV or does not exist.
func VGRemovePlaceholder(vgName string) error {
	// List all PVs in this VG with their tags so we can find the placeholder.
	out, err := exec.Command(
		"pvs",
		"--noheadings", "--separator", "|",
		"-o", "pv_name,pv_tags",
		"--select", "vg_name="+vgName,
	).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return nil
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 2)
		if len(fields) < 2 {
			continue
		}
		loopDev := strings.TrimSpace(fields[0])
		tags := strings.TrimSpace(fields[1])
		if !strings.Contains(tags, placeholderTag) {
			continue
		}

		// Resolve the backing image path before the loop device is detached.
		backOut, _ := exec.Command(
			"losetup", "--noheadings", "-o", "BACK-FILE", loopDev,
		).Output()
		imgPath := strings.TrimSpace(string(backOut))

		exec.Command("vgreduce", vgName, loopDev).Run()
		exec.Command("pvremove", "-ff", "-y", loopDev).Run()
		exec.Command("losetup", "-d", loopDev).Run()
		if imgPath != "" {
			os.Remove(imgPath)
		}
	}
	return nil
}
