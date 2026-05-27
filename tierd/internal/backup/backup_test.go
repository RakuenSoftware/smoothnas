package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFormatRsyncErrorNoSpaceLeftOnDevice(t *testing.T) {
	err := formatRsyncError(errors.New("exit status 11"), `rsync: [receiver] write failed on "/mnt/media/storage/backup/Craft PC/DnD/Alyana/Animations/Beast Within/Beast Within v2.mov": No space left on device (28)
rsync error: error in file IO (code 11) at receiver.c(381) [receiver=3.4.1]
rsync: [sender] write error: Broken pipe (32)`)

	got := err.Error()
	if !strings.Contains(got, "destination filesystem is full") {
		t.Fatalf("expected ENOSPC explanation, got %q", got)
	}
	if !strings.Contains(got, `/mnt/media/storage/backup/Craft PC/DnD/Alyana/Animations/Beast Within/Beast Within v2.mov`) {
		t.Fatalf("expected destination path in error, got %q", got)
	}
}

func TestParseRsyncWriteFailedPathMissing(t *testing.T) {
	if got := parseRsyncWriteFailedPath("plain error"); got != "" {
		t.Fatalf("expected empty path, got %q", got)
	}
}

func TestRsyncArchiveArgsKeepInplaceForSmoothfsDestination(t *testing.T) {
	if !containsArg(rsyncArchiveArgs("/mnt/media/storage"), "--inplace") {
		t.Fatal("smoothfs destination should keep --inplace")
	}
}

func TestRsyncArchiveArgsKeepInplaceForNormalDestination(t *testing.T) {
	if !containsArg(rsyncArchiveArgs("/srv/backups"), "--inplace") {
		t.Fatal("normal destination should keep --inplace")
	}
}

func TestRsyncArchiveArgsSkipExpensiveOwnershipMetadata(t *testing.T) {
	args := rsyncArchiveArgs("/mnt/media/storage")
	for _, want := range []string{"-rltW", "--links", "--no-perms", "--no-owner", "--no-group", "--omit-dir-times"} {
		if !containsArg(args, want) {
			t.Fatalf("rsync args missing %s: %v", want, args)
		}
	}
	if containsArg(args, "-aW") {
		t.Fatalf("rsync args should not use archive owner/group/perms preservation: %v", args)
	}
}

func TestBackupNFSMountOptsFailBoundedly(t *testing.T) {
	opts := strings.Split(nfsMountOpts, ",")
	if !containsArg(opts, "softerr") {
		t.Fatalf("backup NFS mounts must fail after retransmits instead of hanging forever: %q", nfsMountOpts)
	}
	if containsArg(opts, "hard") {
		t.Fatalf("backup NFS mounts must not force hard retry semantics: %q", nfsMountOpts)
	}
	for _, want := range []string{"timeo=50", "retrans=3"} {
		if !containsArg(opts, want) {
			t.Fatalf("backup NFS mounts missing bounded retry option %s: %q", want, nfsMountOpts)
		}
	}
}

func TestForceUmountOnCancelRunsOnCancel(t *testing.T) {
	orig := forceUmountFn
	defer func() { forceUmountFn = orig }()

	called := make(chan string, 1)
	forceUmountFn = func(path string) error {
		called <- path
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := forceUmountOnCancel(ctx, "/tmp/smoothnas-backup-test")
	cancel()

	select {
	case got := <-called:
		if got != "/tmp/smoothnas-backup-test" {
			t.Fatalf("force umount path = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("force umount was not called after cancellation")
	}
	if stop() {
		t.Fatal("stop returned true after cancellation callback ran")
	}
}

func TestForceUmountOnCancelStopSuppressesCallback(t *testing.T) {
	orig := forceUmountFn
	defer func() { forceUmountFn = orig }()

	called := make(chan string, 1)
	forceUmountFn = func(path string) error {
		called <- path
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := forceUmountOnCancel(ctx, "/tmp/smoothnas-backup-test")
	if !stop() {
		t.Fatal("stop returned false before cancellation")
	}
	cancel()

	select {
	case got := <-called:
		t.Fatalf("force umount unexpectedly called for %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMountNFSFailsFastWhenPortUnreachable(t *testing.T) {
	orig := nfsDialFn
	defer func() { nfsDialFn = orig }()

	nfsDialFn = func(_ context.Context, addr string) error {
		return fmt.Errorf("dial tcp %s: connect: connection refused", addr)
	}

	start := time.Now()
	err := mount(Config{TargetType: "nfs", Host: "192.0.2.1", Share: "/share"}, t.TempDir())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for unreachable NFS port")
	}
	if !strings.Contains(err.Error(), "port 2049 not reachable") {
		t.Fatalf("expected port 2049 error, got: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("mount took %v, expected near-instant failure", elapsed)
	}
}

func TestMountNFSProceedsWhenPortReachable(t *testing.T) {
	orig := nfsDialFn
	defer func() { nfsDialFn = orig }()

	// Dial succeeds; mount itself will fail because there's no real NFS server,
	// but the important thing is we get past the port-check step.
	nfsDialFn = func(_ context.Context, addr string) error { return nil }

	err := mount(Config{TargetType: "nfs", Host: "192.0.2.1", Share: "/share"}, t.TempDir())
	if err == nil {
		t.Fatal("expected mount to fail (no real NFS server)")
	}
	if strings.Contains(err.Error(), "port 2049 not reachable") {
		t.Fatalf("should not get port-check error when dial succeeds, got: %v", err)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
