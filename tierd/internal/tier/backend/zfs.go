package backend

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var zfsFSTabPath = "/etc/fstab"

// zfsBackend provisions tier slot storage as a ZFS dataset on an
// existing zpool. `ref` is the zpool name (e.g. "tank"); the dataset
// name is derived deterministically from pool/tier so provision is
// idempotent.
//
// The zpool must already exist — creating the pool itself is handled
// by the Arrays UI (POST /api/pools). This backend only manages the
// dataset within it.
type zfsBackend struct{}

func init() { Register(&zfsBackend{}) }

func (zfsBackend) Kind() string { return "zfs" }

// datasetName returns the tier slot's dataset path inside the zpool.
// Format: <zpool>/tierd/<pool>/<tier>. The "tierd" segment namespaces
// tierd-managed datasets so they never collide with anything the
// operator creates directly on the pool.
func datasetName(zpool, poolName, tierName string) string {
	return zpool + "/tierd/" + poolName + "/" + tierName
}

// zfsExec runs zfs with the given args and wraps any failure with
// stderr for diagnosis.
func zfsExec(args ...string) error {
	out, err := exec.Command("zfs", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("zfs %s: %s: %w",
			strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// datasetExists reports whether the given dataset is present on the
// host. Distinguishes "missing" (normal, first provision) from other
// errors (pool missing, permissions) so callers can react sensibly.
func datasetExists(name string) (bool, error) {
	out, err := exec.Command("zfs", "list", "-H", "-o", "name", name).CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)) == name, nil
	}
	// `zfs list` exits non-zero with "does not exist" when the dataset
	// is absent — treat that as a clean false, not an error.
	if strings.Contains(string(out), "does not exist") {
		return false, nil
	}
	return false, fmt.Errorf("zfs list %s: %s: %w", name, strings.TrimSpace(string(out)), err)
}

func managedDatasetProperties() []string {
	return []string{
		"mountpoint=legacy",
		"compression=lz4",
		"atime=off",
		"recordsize=1M",
		"logbias=throughput",
		"xattr=sa",
	}
}

func createDatasetArgs(ds string) []string {
	args := []string{"create", "-p"}
	for _, prop := range managedDatasetProperties() {
		args = append(args, "-o", prop)
	}
	return append(args, ds)
}

func ensureDatasetProperties(ds string) error {
	for _, prop := range managedDatasetProperties() {
		if err := zfsExec("set", prop, ds); err != nil {
			return err
		}
	}
	return nil
}

func ensureZPoolImported(name string) error {
	out, err := exec.Command("zpool", "list", "-H", "-o", "name", name).CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) == name {
		return nil
	}
	out, err = exec.Command("zpool", "import", "-f", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("zpool import %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (zfsBackend) Provision(poolName, tierName, ref, mountPoint string, _ ProvisionOpts) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("zfs backing_ref (zpool name) required")
	}
	if err := ensureZPoolImported(ref); err != nil {
		return err
	}
	ds := datasetName(ref, poolName, tierName)

	exists, err := datasetExists(ds)
	if err != nil {
		return err
	}
	if !exists {
		// The nested parents (tierd/<pool>) aren't auto-created by zfs;
		// -p makes `zfs create` build the intermediate datasets too.
		// mountpoint=legacy lets us control the mount via the kernel
		// rather than letting ZFS auto-mount at an unpredictable path.
		if err := zfsExec(createDatasetArgs(ds)...); err != nil {
			return err
		}
	} else {
		// If the dataset survives a reboot, keep tier-managed
		// performance and mount properties in the expected state.
		if err := ensureDatasetProperties(ds); err != nil {
			return err
		}
	}

	// Idempotent mount. `mount -t zfs legacy` path comes via /dev/zfs.
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPoint, err)
	}
	if isAlreadyMounted(mountPoint) {
		return ensureLegacyFSTabEntry(ds, mountPoint)
	}
	out, err := exec.Command("mount", "-t", "zfs", ds, mountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s at %s: %s: %w",
			ds, mountPoint, strings.TrimSpace(string(out)), err)
	}
	return ensureLegacyFSTabEntry(ds, mountPoint)
}

func (zfsBackend) Destroy(poolName, tierName, ref, mountPoint string) error {
	if strings.TrimSpace(ref) == "" {
		return nil // nothing to destroy if the assignment was never persisted
	}
	ds := datasetName(ref, poolName, tierName)
	_ = removeLegacyFSTabEntry(ds, mountPoint)

	// Unmount first. "not mounted" is fine — it means destroy is
	// safe to run unconditionally.
	out, umountErr := exec.Command("umount", mountPoint).CombinedOutput()
	if umountErr != nil && !strings.Contains(string(out), "not mounted") {
		// Try lazy as a fallback; a holder may be briefly racing with
		// tier destroy. Report the original error if lazy also fails.
		out2, lazyErr := exec.Command("umount", "-l", mountPoint).CombinedOutput()
		if lazyErr != nil && !strings.Contains(string(out2), "not mounted") {
			return fmt.Errorf("umount %s: %s: %w",
				mountPoint, strings.TrimSpace(string(out)), umountErr)
		}
	}

	exists, err := datasetExists(ds)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	// -r destroys children too; the slot dataset has no children in
	// normal operation but a crash mid-provision could leave some.
	return zfsExec("destroy", "-rf", ds)
}

// isAlreadyMounted does a cheap check that a given path is already
// a mount target. Reused by multiple backends so it lives here.
func isAlreadyMounted(path string) bool {
	out, err := exec.Command("findmnt", "-M", path).CombinedOutput()
	_ = out
	return err == nil
}

func ensureLegacyFSTabEntry(dataset, mountPoint string) error {
	entry := fmt.Sprintf("%s %s zfs defaults,nofail 0 0", dataset, mountPoint)
	data, err := os.ReadFile(zfsFSTabPath)
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 2 && (fields[0] == dataset || fields[1] == mountPoint) {
				if line == entry {
					return nil
				}
				lines[i] = entry
				output := strings.Join(lines, "\n")
				if !strings.HasSuffix(output, "\n") {
					output += "\n"
				}
				if err := os.WriteFile(zfsFSTabPath, []byte(output), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", zfsFSTabPath, err)
				}
				return nil
			}
		}
	}

	f, err := os.OpenFile(zfsFSTabPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", zfsFSTabPath, err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("append %s: %w", zfsFSTabPath, err)
	}
	return nil
}

func removeLegacyFSTabEntry(dataset, mountPoint string) error {
	data, err := os.ReadFile(zfsFSTabPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", zfsFSTabPath, err)
	}

	lines := strings.Split(string(data), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == dataset || fields[1] == mountPoint) {
			continue
		}
		filtered = append(filtered, line)
	}
	output := strings.Join(filtered, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	if err := os.WriteFile(zfsFSTabPath, []byte(output), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", zfsFSTabPath, err)
	}
	return nil
}
