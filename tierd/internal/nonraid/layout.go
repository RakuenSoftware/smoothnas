package nonraid

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	BackendKind = "nonraid"

	RoleData   = "data"
	RoleParity = "parity"

	StateConfigured = "configured"
	StateActive     = "active"
	StateError      = "error"

	DefaultFilesystem = "xfs"
	DefaultMountBase  = "/mnt"
	BackingBase       = "/mnt/.nonraid"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// Device describes a candidate disk before it is committed to an array.
type Device struct {
	Path   string
	Serial string
	Size   uint64
}

// DevicePlan is the persisted shape for one data or parity device.
type DevicePlan struct {
	Role              string `json:"role"`
	Slot              int    `json:"slot"`
	DevicePath        string `json:"device_path"`
	VirtualDevicePath string `json:"virtual_device_path,omitempty"`
	Serial            string `json:"serial,omitempty"`
	SizeBytes         uint64 `json:"size_bytes"`
	UsableBytes       uint64 `json:"usable_bytes"`
	MountPath         string `json:"mount_path,omitempty"`
	State             string `json:"state"`
}

// Plan is the validated result of a requested nonRaid array layout.
type Plan struct {
	Name           string       `json:"name"`
	State          string       `json:"state"`
	Filesystem     string       `json:"filesystem"`
	MountPath      string       `json:"mount_path"`
	ParityCount    int          `json:"parity_count"`
	MinParityBytes uint64       `json:"min_parity_bytes"`
	CapacityBytes  uint64       `json:"capacity_bytes"`
	UUID           string       `json:"uuid,omitempty"`
	Devices        []DevicePlan `json:"devices"`
}

// BuildPlan validates an Unraid-style layout: files live on individual data
// disks, parity disks protect those disks, and each data disk must fit within
// the smallest parity disk.
func BuildPlan(name, filesystem, mountBase string, data, parity []Device) (*Plan, error) {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid nonRaid array name %q", name)
	}
	if filesystem == "" {
		filesystem = DefaultFilesystem
	}
	if filesystem != DefaultFilesystem {
		return nil, fmt.Errorf("unsupported nonRaid data filesystem %q", filesystem)
	}
	if mountBase == "" {
		mountBase = DefaultMountBase
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("nonRaid requires at least one data disk")
	}
	if len(parity) < 1 || len(parity) > 3 {
		return nil, fmt.Errorf("nonRaid requires between 1 and 3 parity disks")
	}
	if err := validateDistinctDevices(data, parity); err != nil {
		return nil, err
	}

	minParity := parity[0].Size
	for _, dev := range parity[1:] {
		if dev.Size < minParity {
			minParity = dev.Size
		}
	}
	if minParity == 0 {
		return nil, fmt.Errorf("parity disk size is unknown")
	}

	plan := &Plan{
		Name:           name,
		State:          StateConfigured,
		Filesystem:     filesystem,
		MountPath:      filepath.Join(mountBase, name),
		ParityCount:    len(parity),
		MinParityBytes: minParity,
	}

	for i, dev := range data {
		if dev.Size == 0 {
			return nil, fmt.Errorf("data disk %s size is unknown", dev.Path)
		}
		if dev.Size > minParity {
			return nil, fmt.Errorf("data disk %s is larger than smallest parity disk", dev.Path)
		}
		plan.CapacityBytes += dev.Size
		plan.Devices = append(plan.Devices, DevicePlan{
			Role:        RoleData,
			Slot:        i + 1,
			DevicePath:  dev.Path,
			Serial:      dev.Serial,
			SizeBytes:   dev.Size,
			UsableBytes: dev.Size,
			MountPath:   filepath.Join(BackingBase, name, fmt.Sprintf("disk%d", i+1)),
			State:       StateConfigured,
		})
	}
	for i, dev := range parity {
		plan.Devices = append(plan.Devices, DevicePlan{
			Role:        RoleParity,
			Slot:        i + 1,
			DevicePath:  dev.Path,
			Serial:      dev.Serial,
			SizeBytes:   dev.Size,
			UsableBytes: minParity,
			MountPath:   "",
			State:       StateConfigured,
		})
	}

	return plan, nil
}

func validateDistinctDevices(data, parity []Device) error {
	seen := make(map[string]struct{}, len(data)+len(parity))
	for _, group := range [][]Device{data, parity} {
		for _, dev := range group {
			path := strings.TrimSpace(dev.Path)
			if path == "" {
				return fmt.Errorf("disk path is required")
			}
			if _, ok := seen[path]; ok {
				return fmt.Errorf("disk %s is assigned more than once", path)
			}
			seen[path] = struct{}{}
		}
	}
	return nil
}
