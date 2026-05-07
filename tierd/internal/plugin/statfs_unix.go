//go:build linux

package plugin

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StatAvail returns the free bytes at path via statfs(2). Used by
// the preflight free-space gate. Linux-only; build tag prevents
// accidental use on platforms that don't have statfs(2).
func StatAvail(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Bavail (blocks available to unprivileged users) is the
	// honest number — Bfree includes blocks reserved for root.
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
