//go:build linux && e2e

package iscsi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestE2EFileBackedTargetAutoPinsOnSmoothfs stands up a 2-tier XFS
// smoothfs, creates a fileio LUN through tierd's public API on the
// smoothfs mount, and asserts that the backing file ends up pinned
// with PIN_LUN (trusted.smoothfs.lun == 1). DestroyFileBackedTarget
// must then clear the pin back to PIN_NONE.
//
// Gated by SMOOTHFS_KO + iSCSI tooling (targetcli-fb, open-iscsi).
// Skips cleanly in CI environments that don't have LIO.
func TestE2EFileBackedTargetAutoPinsOnSmoothfs(t *testing.T) {
	ko := os.Getenv("SMOOTHFS_KO")
	if ko == "" {
		t.Skip("set SMOOTHFS_KO to the built smoothfs.ko path")
	}
	if _, err := exec.LookPath("targetcli"); err != nil {
		t.Skip("targetcli not installed — skip LUN auto-pin e2e")
	}
	if _, err := exec.LookPath("mkfs.xfs"); err != nil {
		t.Skip("mkfs.xfs not installed — skip smoothfs setup")
	}
	if os.Geteuid() != 0 {
		t.Skip("must run as root (mount, targetcli, trusted xattrs)")
	}

	const iqn = "iqn.2026-04.com.smoothnas:phase65test"
	const portUUID = "00000000-0000-0000-0000-000000000650"

	root := t.TempDir()
	fast := filepath.Join(root, "fast")
	slow := filepath.Join(root, "slow")
	server := filepath.Join(root, "server")
	for _, d := range []string{fast, slow, server} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	fastImg := filepath.Join(root, "fast.img")
	slowImg := filepath.Join(root, "slow.img")
	for _, img := range []string{fastImg, slowImg} {
		if err := exec.Command("truncate", "-s", "1G", img).Run(); err != nil {
			t.Fatalf("truncate %s: %v", img, err)
		}
		if err := exec.Command("mkfs.xfs", "-q", "-f", img).Run(); err != nil {
			t.Fatalf("mkfs.xfs %s: %v", img, err)
		}
	}

	mustRun := func(t *testing.T, name string, args ...string) {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %s: %v", name, args, string(out), err)
		}
	}

	mustRun(t, "mount", "-o", "loop", fastImg, fast)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", fast).Run() })
	mustRun(t, "mount", "-o", "loop", slowImg, slow)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", slow).Run() })

	// Load smoothfs module if it isn't already — insmod will fail
	// with -EEXIST if it's already loaded, which the follow-up
	// mount handles gracefully. We don't care which of the two
	// outcomes the test hit; only whether the mount succeeds.
	_ = exec.Command("insmod", ko).Run()
	mustRun(t, "mount", "-t", "smoothfs",
		"-o", "pool=tierpin,uuid="+portUUID+",tiers="+fast+":"+slow,
		"none", server)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", server).Run() })

	// Sanity: the mountpoint must report as smoothfs.
	onSmoothfs, err := IsOnSmoothfs(server)
	if err != nil {
		t.Fatalf("IsOnSmoothfs: %v", err)
	}
	if !onSmoothfs {
		t.Fatal("smoothfs mount doesn't match SmoothfsMagic (constant wrong?)")
	}

	backing := filepath.Join(server, "lun.img")
	if err := exec.Command("truncate", "-s", "64M", backing).Run(); err != nil {
		t.Fatalf("truncate backing: %v", err)
	}

	// Pre-state: no pin.
	if v, err := readLunXattr(backing); err != nil {
		t.Fatalf("read lun xattr pre-create: %v", err)
	} else if v != 0 {
		t.Fatalf("pre-create lun xattr = %d, want 0", v)
	}

	// Make sure no residue from a prior run.
	_ = DestroyFileBackedTarget(iqn, backing)
	t.Cleanup(func() { _ = DestroyFileBackedTarget(iqn, backing) })

	if err := CreateFileBackedTarget(iqn, backing); err != nil {
		t.Fatalf("CreateFileBackedTarget: %v", err)
	}

	if v, err := readLunXattr(backing); err != nil {
		t.Fatalf("read lun xattr post-create: %v", err)
	} else if v != 1 {
		t.Fatalf("post-create lun xattr = %d, want 1 (PIN_LUN)", v)
	}

	if err := DestroyFileBackedTarget(iqn, backing); err != nil {
		t.Fatalf("DestroyFileBackedTarget: %v", err)
	}

	if v, err := readLunXattr(backing); err != nil {
		t.Fatalf("read lun xattr post-destroy: %v", err)
	} else if v != 0 {
		t.Fatalf("post-destroy lun xattr = %d, want 0 (PIN_NONE)", v)
	}
}

func readLunXattr(path string) (int, error) {
	buf := make([]byte, 1)
	n, err := unix.Getxattr(path, LunPinXattr, buf)
	if err != nil {
		return -1, err
	}
	if n != 1 {
		return -1, fmt.Errorf("lun xattr returned %d bytes, want 1", n)
	}
	return int(buf[0]), nil
}

