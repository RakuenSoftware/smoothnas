//go:build linux

package meta

import (
	"io/fs"
	"syscall"
	"testing"
)

// sysStatIno extracts the backing FS inode from a FileInfo. Kept in a
// build-tagged file so test suites on non-Linux hosts still compile
// (the meta package itself is Linux-only in practice via tierd).
func sysStatIno(t *testing.T, fi fs.FileInfo) uint64 {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("FileInfo.Sys() = %T, want *syscall.Stat_t", fi.Sys())
	}
	return st.Ino
}
