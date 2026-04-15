package zfs

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Dataset represents a ZFS dataset (filesystem).
type Dataset struct {
	Name             string `json:"name"`              // e.g. "tank/data"
	Pool             string `json:"pool"`
	MountPoint       string `json:"mount_point"`
	Mounted          bool   `json:"mounted"`
	Used             uint64 `json:"used"`              // bytes
	Available        uint64 `json:"available"`         // bytes
	Quota            uint64 `json:"quota"`             // bytes, 0 = none
	Reservation      uint64 `json:"reservation"`       // bytes, 0 = none
	Compression      string `json:"compression"`       // "lz4", "zstd", "off", etc.
	CompressRatio    string `json:"compress_ratio"`    // e.g. "1.50x"
	UsedHuman        string `json:"used_human"`
	AvailableHuman   string `json:"available_human"`
}

var datasetNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_/-]{0,255}$`)

// ValidateDatasetName checks that a dataset name is safe.
func ValidateDatasetName(name string) error {
	if !datasetNameRegex.MatchString(name) {
		return fmt.Errorf("invalid dataset name: %s", name)
	}
	return nil
}

// ValidateCompression checks that a compression algorithm is valid.
func ValidateCompression(algo string) error {
	valid := map[string]bool{
		"on": true, "off": true,
		"lz4": true, "zstd": true, "zstd-fast": true,
		"gzip": true, "lzjb": true, "zle": true,
	}
	if !valid[strings.ToLower(algo)] {
		return fmt.Errorf("invalid compression: %s", algo)
	}
	return nil
}

// CreateDataset creates a ZFS dataset.
func CreateDataset(name, mountPoint, compression string, quota, reservation uint64) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	args := []string{"create"}

	if mountPoint != "" {
		args = append(args, "-o", "mountpoint="+mountPoint)
	}
	if compression != "" {
		if err := ValidateCompression(compression); err != nil {
			return err
		}
		args = append(args, "-o", "compression="+compression)
	}
	if quota > 0 {
		args = append(args, "-o", "quota="+strconv.FormatUint(quota, 10))
	}
	if reservation > 0 {
		args = append(args, "-o", "reservation="+strconv.FormatUint(reservation, 10))
	}

	args = append(args, name)

	cmd := exec.Command("zfs", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ListDatasets returns all datasets, optionally filtered by pool.
func ListDatasets(pool string) ([]Dataset, error) {
	args := []string{"list", "-Hp", "-t", "filesystem",
		"-o", "name,mountpoint,mounted,used,avail,quota,reservation,compression,compressratio",
	}
	if pool != "" {
		args = append(args, "-r", pool)
	}

	out, err := exec.Command("zfs", args...).Output()
	if err != nil {
		return nil, nil
	}

	var datasets []Dataset
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}

		used, _ := strconv.ParseUint(fields[3], 10, 64)
		avail, _ := strconv.ParseUint(fields[4], 10, 64)
		quota, _ := strconv.ParseUint(fields[5], 10, 64)
		resv, _ := strconv.ParseUint(fields[6], 10, 64)

		// Extract pool name from dataset name.
		poolName := strings.SplitN(fields[0], "/", 2)[0]

		datasets = append(datasets, Dataset{
			Name:           fields[0],
			Pool:           poolName,
			MountPoint:     fields[1],
			Mounted:        fields[2] == "yes",
			Used:           used,
			Available:      avail,
			Quota:          quota,
			Reservation:    resv,
			Compression:    fields[7],
			CompressRatio:  fields[8],
			UsedHuman:      humanSize(used),
			AvailableHuman: humanSize(avail),
		})
	}
	return datasets, nil
}

// UpdateDataset changes properties on an existing dataset.
func UpdateDataset(name string, props map[string]string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	allowedProps := map[string]bool{
		"mountpoint": true, "compression": true,
		"quota": true, "reservation": true,
	}

	for key, value := range props {
		if !allowedProps[key] {
			return fmt.Errorf("property not allowed: %s", key)
		}
		if key == "compression" {
			if err := ValidateCompression(value); err != nil {
				return err
			}
		}
		cmd := exec.Command("zfs", "set", key+"="+value, name)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("zfs set %s=%s: %s: %w", key, value, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// DestroyDataset destroys a ZFS dataset.
func DestroyDataset(name string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "destroy", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs destroy: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// MountDataset mounts a ZFS dataset.
func MountDataset(name string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "mount", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs mount: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// UnmountDataset unmounts a ZFS dataset.
func UnmountDataset(name string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "unmount", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs unmount: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// --- Build helpers (exported for testing) ---

// BuildCreateDatasetArgs returns the zfs create argument list.
func BuildCreateDatasetArgs(name, mountPoint, compression string, quota, reservation uint64) ([]string, error) {
	if err := ValidateDatasetName(name); err != nil {
		return nil, err
	}

	args := []string{"create"}
	if mountPoint != "" {
		args = append(args, "-o", "mountpoint="+mountPoint)
	}
	if compression != "" {
		if err := ValidateCompression(compression); err != nil {
			return nil, err
		}
		args = append(args, "-o", "compression="+compression)
	}
	if quota > 0 {
		args = append(args, "-o", "quota="+strconv.FormatUint(quota, 10))
	}
	if reservation > 0 {
		args = append(args, "-o", "reservation="+strconv.FormatUint(reservation, 10))
	}
	args = append(args, name)
	return args, nil
}
