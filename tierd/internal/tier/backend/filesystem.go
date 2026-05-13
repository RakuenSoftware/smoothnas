package backend

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type filesystemBackend struct {
	kind string
}

func init() {
	Register(&filesystemBackend{kind: "btrfs"})
	Register(&filesystemBackend{kind: "bcachefs"})
}

func (b *filesystemBackend) Kind() string { return b.kind }

func (b *filesystemBackend) Provision(poolName, tierName, ref, mountPoint string, _ ProvisionOpts) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("%s backing_ref mount path required", b.kind)
	}
	if !filepath.IsAbs(ref) {
		return fmt.Errorf("%s backing_ref must be an absolute mount path", b.kind)
	}
	if !isAlreadyMounted(ref) {
		out, err := exec.Command("mount", ref).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount %s backing %s: %s: %w", b.kind, ref, strings.TrimSpace(string(out)), err)
		}
	}

	source := filesystemSlotPath(ref, poolName, tierName)
	if err := b.ensureSource(source); err != nil {
		return err
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPoint, err)
	}
	if isAlreadyMounted(mountPoint) {
		return ensureBindFSTabEntry(source, mountPoint)
	}
	out, err := exec.Command("mount", "--bind", source, mountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("bind mount %s at %s: %s: %w", source, mountPoint, strings.TrimSpace(string(out)), err)
	}
	return ensureBindFSTabEntry(source, mountPoint)
}

func (b *filesystemBackend) Destroy(poolName, tierName, ref, mountPoint string) error {
	ref = strings.TrimSpace(ref)
	source := filesystemSlotPath(ref, poolName, tierName)
	_ = removeBindFSTabEntry(source, mountPoint)
	if isAlreadyMounted(mountPoint) {
		out, err := exec.Command("umount", mountPoint).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "not mounted") {
			out2, lazyErr := exec.Command("umount", "-l", mountPoint).CombinedOutput()
			if lazyErr != nil && !strings.Contains(string(out2), "not mounted") {
				return fmt.Errorf("umount %s: %s: %w", mountPoint, strings.TrimSpace(string(out)), err)
			}
		}
	}
	if _, err := os.Stat(source); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", source, err)
	}
	if b.kind == "btrfs" {
		out, err := exec.Command("btrfs", "subvolume", "delete", source).CombinedOutput()
		if err == nil || strings.Contains(string(out), "not a btrfs subvolume") {
			return os.RemoveAll(source)
		}
		return fmt.Errorf("btrfs subvolume delete %s: %s: %w", source, strings.TrimSpace(string(out)), err)
	}
	return os.RemoveAll(source)
}

func (b *filesystemBackend) ensureSource(source string) error {
	if _, err := os.Stat(source); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(source), err)
	}
	if b.kind != "btrfs" {
		return os.MkdirAll(source, 0o755)
	}
	out, err := exec.Command("btrfs", "subvolume", "create", source).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs subvolume create %s: %s: %w", source, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func filesystemSlotPath(root, poolName, tierName string) string {
	return filepath.Join(root, "tierd", poolName, tierName)
}

func ensureBindFSTabEntry(source, mountPoint string) error {
	entry := fmt.Sprintf("%s %s none bind,nofail 0 0", source, mountPoint)
	return upsertLegacyFSTabEntry(source, mountPoint, entry)
}

func removeBindFSTabEntry(source, mountPoint string) error {
	return removeLegacyFSTabEntry(source, mountPoint)
}
