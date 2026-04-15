// Package lvm manages the LVM primitives used by named tier instances:
// physical volumes, volume groups, logical volumes, filesystems, and mounts.
package lvm

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidateFilesystem reports an error if fs is not a supported filesystem.
func ValidateFilesystem(fs string) error {
	switch fs {
	case "xfs", "ext4":
		return nil
	default:
		return fmt.Errorf("unsupported filesystem %q: must be xfs or ext4", fs)
	}
}

// mountPointRe restricts mount points to /mnt/<safe-path>. Dots are allowed
// in path segments (e.g. hidden directories like .backing) but ".." traversal
// is checked separately.
var mountPointRe = regexp.MustCompile(`^/mnt/[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)*$`)

// ValidateMountPoint rejects mount points outside /mnt/, containing unsafe
// characters, or containing ".." path traversal components.
func ValidateMountPoint(mp string) error {
	if !mountPointRe.MatchString(mp) {
		return fmt.Errorf("mount point must be under /mnt/ and contain only alphanumeric, hyphen, underscore, or dot characters")
	}
	for _, seg := range strings.Split(mp, "/") {
		if seg == ".." {
			return fmt.Errorf("mount point must not contain .. path components")
		}
	}
	return nil
}

// devicePathRe restricts device paths to mdadm/nvme/sd* block devices.
var devicePathRe = regexp.MustCompile(`^/dev/(md[0-9]+|sd[a-z]+|nvme[0-9]+n[0-9]+)$`)

// ValidateDevicePath rejects device paths that are not mdadm arrays, NVMe
// namespaces, or SATA/SAS disks.
func ValidateDevicePath(path string) error {
	if !devicePathRe.MatchString(path) {
		return fmt.Errorf("invalid device path %q", path)
	}
	return nil
}

// lvNameRe restricts LV and tier-related names to safe characters.
var lvNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateName rejects LV/VG names containing shell metacharacters.
func ValidateName(name string) error {
	if name == "" || !lvNameRe.MatchString(name) {
		return fmt.Errorf("name %q must be alphanumeric with hyphens or underscores", name)
	}
	return nil
}
