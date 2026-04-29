package iscsi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// SmoothfsMagic is the statfs f_type for a smoothfs mount, from
// src/smoothfs/smoothfs.h:SMOOTHFS_MAGIC ('SMOF' as a 4-byte ASCII
// little-endian u32). Exported so operator tooling or higher-level
// share-management code can make the same is-on-smoothfs decision
// as the iscsi package without duplicating the constant.
const SmoothfsMagic = 0x534D4F46

// LunPinXattr is the smoothfs xattr that drives PIN_LUN. A 1-byte
// value of \x01 installs the pin (pin_state: PIN_NONE -> PIN_LUN);
// \x00 or removexattr clears it back to PIN_NONE. Any other pin on
// the inode (HARDLINK, LEASE, HOT, COLD) causes the set to fail with
// EBUSY — smoothfs does not allow overwriting a non-LUN pin. See
// Phase 6.2 (src/smoothfs/xattr.c) for the kernel-side contract.
const LunPinXattr = "trusted.smoothfs.lun"

// LUNPinStatus is the operator-facing state of the smoothfs PIN_LUN
// protection for a fileio backing file.
type LUNPinStatus struct {
	Path       string `json:"path"`
	OnSmoothfs bool   `json:"on_smoothfs"`
	Pinned     bool   `json:"pinned"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
}

// ValidateBackingFilePath checks that a fileio backstore path is
// reasonable: absolute, no newlines/null bytes (targetcli sees it as
// a shell-safe key=value), exists, and is a regular file.
//
// We do NOT require the file to be on smoothfs — LIO fileio works
// over any filesystem and operators may legitimately run a fileio
// LUN on stock XFS alongside smoothfs-backed ones. The is-smoothfs
// probe only gates the pin.
func ValidateBackingFilePath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("backing file path must be absolute: %q", path)
	}
	if strings.ContainsAny(path, "\n\x00") {
		return fmt.Errorf("backing file path contains control characters")
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat backing file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("backing file is not a regular file: %q", path)
	}
	return nil
}

// IsOnSmoothfs returns true iff the path resolves on a smoothfs
// mount, determined by statfs(2) f_type against SmoothfsMagic. The
// path does not have to exist — if stat fails we return false and a
// nil error; callers get a clear "not smoothfs" signal.
func IsOnSmoothfs(path string) (bool, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("statfs %q: %w", path, err)
	}
	// unix.Statfs_t.Type is int64 on some arches, int32 on others;
	// just cast to compare against the kernel constant.
	return uint32(fs.Type) == SmoothfsMagic, nil
}

// PinLUN installs the smoothfs PIN_LUN pin on path. It is a no-op on
// any non-smoothfs filesystem — auto-detected via IsOnSmoothfs — so
// callers can invoke it unconditionally whenever they create a
// fileio backstore. Returns nil on successful pin install, nil on
// non-smoothfs (nothing to do), or the underlying setxattr error on
// a smoothfs lower that refused the call (the most likely cause is
// EBUSY when another pin already owns the inode; that's a
// pre-existing contract violation and PinLUN should not paper over
// it).
func PinLUN(path string) error {
	onSmoothfs, err := IsOnSmoothfs(path)
	if err != nil {
		return fmt.Errorf("smoothfs probe: %w", err)
	}
	if !onSmoothfs {
		return nil
	}
	return unix.Setxattr(path, LunPinXattr, []byte{1}, 0)
}

// UnpinLUN clears the smoothfs PIN_LUN pin on path. Same
// non-smoothfs no-op semantics as PinLUN. ENODATA (xattr wasn't
// there) is silently accepted — the kernel's idempotent "set value
// 0" path means removexattr of an absent xattr is never a real
// failure for callers, only for diagnostics.
func UnpinLUN(path string) error {
	onSmoothfs, err := IsOnSmoothfs(path)
	if err != nil {
		return fmt.Errorf("smoothfs probe: %w", err)
	}
	if !onSmoothfs {
		return nil
	}
	if err := unix.Removexattr(path, LunPinXattr); err != nil {
		if errors.Is(err, unix.ENODATA) {
			return nil
		}
		return err
	}
	return nil
}

// InspectLUNPin reports whether path is on smoothfs and, if so, whether
// trusted.smoothfs.lun currently marks it as PIN_LUN. It is intentionally
// best-effort for status surfaces: stale DB paths and xattr read failures
// are returned as explicit states instead of failing the whole target list.
func InspectLUNPin(path string) LUNPinStatus {
	status := LUNPinStatus{Path: path, State: "unknown"}
	if path == "" {
		status.Reason = "backing file path is empty"
		return status
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\n\x00") {
		status.Reason = "backing file path is invalid"
		return status
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			status.State = "missing"
			status.Reason = "backing file does not exist"
			return status
		}
		status.Reason = "stat backing file: " + err.Error()
		return status
	}
	if !st.Mode().IsRegular() {
		status.State = "not_regular"
		status.Reason = "backing path is not a regular file"
		return status
	}

	onSmoothfs, err := IsOnSmoothfs(path)
	if err != nil {
		status.Reason = "smoothfs probe: " + err.Error()
		return status
	}
	status.OnSmoothfs = onSmoothfs
	if !onSmoothfs {
		status.State = "not_applicable"
		status.Reason = "backing file is not on smoothfs"
		return status
	}

	buf := []byte{0}
	n, err := unix.Getxattr(path, LunPinXattr, buf)
	if err != nil {
		if errors.Is(err, unix.ENODATA) {
			status.State = "unpinned"
			status.Reason = "PIN_LUN xattr is absent"
			return status
		}
		status.Reason = "read PIN_LUN xattr: " + err.Error()
		return status
	}
	if n != 1 {
		status.Reason = fmt.Sprintf("PIN_LUN xattr returned %d bytes", n)
		return status
	}
	switch buf[0] {
	case 1:
		status.Pinned = true
		status.State = "pinned"
	case 0:
		status.State = "unpinned"
		status.Reason = "PIN_LUN xattr is cleared"
	default:
		status.Reason = fmt.Sprintf("PIN_LUN xattr has invalid value %d", buf[0])
	}
	return status
}

// CreateFileBackedTarget stands up an iSCSI target with a fileio
// backstore pointing at path. On a smoothfs lower the backing file
// is auto-pinned with PIN_LUN so tierd refuses to move it while the
// LUN is live; on any other lower the pin step is silently skipped.
//
// The file must already exist and be sized to the desired LUN size
// — this function does not create or truncate it. Callers (the
// Phase 7 share-management path) are expected to have provisioned
// the file first.
//
// On any error after the backstore is created, the backstore and
// target are torn down and the pin (if installed) is cleared, so
// callers never observe a half-built state.
func CreateFileBackedTarget(iqn, path string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}
	if err := ValidateBackingFilePath(path); err != nil {
		return err
	}

	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat backing file: %w", err)
	}
	size := st.Size()
	if size <= 0 {
		return fmt.Errorf("backing file is empty: %q (LIO fileio needs a sized file)", path)
	}

	bsName := iqnToBackstoreName(iqn)

	// 1. Pin before telling LIO about the file. If the pin fails we
	//    bail before any LIO state exists, so no cleanup needed.
	if err := PinLUN(path); err != nil {
		return fmt.Errorf("pin LUN: %w", err)
	}

	cleanup := func() {
		_ = UnpinLUN(path)
	}

	// 2. Create the fileio backstore. targetcli expects size in
	//    bytes; pass it explicitly so LIO doesn't try to re-probe
	//    the file size (which requires O_DIRECT capability, already
	//    handled by Phase 6.0, but an explicit size is cheaper).
	cmd := exec.Command("targetcli",
		"/backstores/fileio", "create",
		"name="+bsName,
		"file_or_dev="+path,
		fmt.Sprintf("size=%d", size))
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return fmt.Errorf("create fileio backstore: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 3. Create the target.
	cmd = exec.Command("targetcli", "/iscsi", "create", iqn)
	if out, err := cmd.CombinedOutput(); err != nil {
		exec.Command("targetcli", "/backstores/fileio", "delete", bsName).Run()
		cleanup()
		return fmt.Errorf("create target: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 4. Create the LUN referencing the fileio backstore.
	lunPath := fmt.Sprintf("/iscsi/%s/tpg1/luns", iqn)
	cmd = exec.Command("targetcli", lunPath, "create",
		fmt.Sprintf("/backstores/fileio/%s", bsName))
	if out, err := cmd.CombinedOutput(); err != nil {
		exec.Command("targetcli", "/iscsi", "delete", iqn).Run()
		exec.Command("targetcli", "/backstores/fileio", "delete", bsName).Run()
		cleanup()
		return fmt.Errorf("create lun: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return saveConfig()
}

// DestroyFileBackedTarget tears down a target created by
// CreateFileBackedTarget and releases the PIN_LUN pin on the backing
// file. Safe to call on a target that's partially gone — each step
// is best-effort. Returns the first error encountered after all
// steps have run so callers see the earliest failure but aren't
// left with residual state.
func DestroyFileBackedTarget(iqn, path string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}

	var firstErr error
	setErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	bsName := iqnToBackstoreName(iqn)

	cmd := exec.Command("targetcli", "/iscsi", "delete", iqn)
	if out, err := cmd.CombinedOutput(); err != nil {
		setErr(fmt.Errorf("delete target: %s: %w",
			strings.TrimSpace(string(out)), err))
	}

	cmd = exec.Command("targetcli", "/backstores/fileio", "delete", bsName)
	if out, err := cmd.CombinedOutput(); err != nil {
		setErr(fmt.Errorf("delete backstore: %s: %w",
			strings.TrimSpace(string(out)), err))
	}

	// Clear the pin last so a re-create attempt on the same backing
	// file after a teardown failure doesn't trip over the old pin.
	// UnpinLUN is a no-op on non-smoothfs and on ENODATA.
	if path != "" {
		if err := UnpinLUN(path); err != nil {
			setErr(fmt.Errorf("unpin LUN: %w", err))
		}
	}

	if err := saveConfig(); err != nil {
		setErr(err)
	}
	return firstErr
}
