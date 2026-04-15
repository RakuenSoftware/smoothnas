package zfs

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Zvol represents a ZFS volume (block device).
type Zvol struct {
	Name      string `json:"name"`       // e.g. "tank/iscsi-lun0"
	Pool      string `json:"pool"`
	Size      uint64 `json:"size"`       // bytes
	Used      uint64 `json:"used"`       // bytes
	BlockSize string `json:"block_size"` // e.g. "8K"
	SizeHuman string `json:"size_human"`
	UsedHuman string `json:"used_human"`
}

// CreateZvol creates a ZFS volume.
func CreateZvol(name, size, blockSize string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	args := []string{"create", "-V", size}
	if blockSize != "" {
		args = append(args, "-b", blockSize)
	}
	args = append(args, name)

	cmd := exec.Command("zfs", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs create zvol: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ListZvols returns all zvols, optionally filtered by pool.
func ListZvols(pool string) ([]Zvol, error) {
	args := []string{"list", "-Hp", "-t", "volume",
		"-o", "name,volsize,used,volblocksize",
	}
	if pool != "" {
		args = append(args, "-r", pool)
	}

	out, err := exec.Command("zfs", args...).Output()
	if err != nil {
		return nil, nil
	}

	var zvols []Zvol
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		size, _ := strconv.ParseUint(fields[1], 10, 64)
		used, _ := strconv.ParseUint(fields[2], 10, 64)
		poolName := strings.SplitN(fields[0], "/", 2)[0]

		zvols = append(zvols, Zvol{
			Name:      fields[0],
			Pool:      poolName,
			Size:      size,
			Used:      used,
			BlockSize: fields[3],
			SizeHuman: humanSize(size),
			UsedHuman: humanSize(used),
		})
	}
	return zvols, nil
}

// ResizeZvol resizes a ZFS volume.
func ResizeZvol(name, newSize string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "set", "volsize="+newSize, name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs set volsize: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// DestroyZvol destroys a ZFS volume.
func DestroyZvol(name string) error {
	if err := ValidateDatasetName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "destroy", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs destroy zvol: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// --- Build helpers ---

// BuildCreateZvolArgs returns the zfs create argument list for a zvol.
func BuildCreateZvolArgs(name, size, blockSize string) ([]string, error) {
	if err := ValidateDatasetName(name); err != nil {
		return nil, err
	}
	args := []string{"create", "-V", size}
	if blockSize != "" {
		args = append(args, "-b", blockSize)
	}
	args = append(args, name)
	return args, nil
}
