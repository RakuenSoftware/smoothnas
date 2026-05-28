package backup

import (
	"context"
	"errors"
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

func TestRsyncArchiveArgsKeepInplace(t *testing.T) {
	if !containsArg(rsyncArchiveArgs(60), "--inplace") {
		t.Fatal("rsync args should keep --inplace")
	}
}

func TestRsyncArchiveArgsSkipExpensiveOwnershipMetadata(t *testing.T) {
	args := rsyncArchiveArgs(60)
	for _, want := range []string{"-rltW", "--links", "--no-perms", "--no-owner", "--no-group", "--omit-dir-times"} {
		if !containsArg(args, want) {
			t.Fatalf("rsync args missing %s: %v", want, args)
		}
	}
	if containsArg(args, "-aW") {
		t.Fatalf("rsync args should not use archive owner/group/perms preservation: %v", args)
	}
}

func TestRsyncMountArgsUsesLongerTimeoutForHDDSpinup(t *testing.T) {
	// NFS-mounted backups need a longer timeout than SSH backups so that
	// a sleeping source HDD has time to spin up before rsync declares a stall.
	args := rsyncMountArgs("/mnt/dst")
	for _, a := range args {
		if a == "--timeout=60" {
			t.Fatalf("rsyncMountArgs should not use --timeout=60; source HDDs may need > 60 s to spin up: %v", args)
		}
	}
	hasTimeout := false
	for _, a := range args {
		if strings.HasPrefix(a, "--timeout=") {
			hasTimeout = true
		}
	}
	if !hasTimeout {
		t.Fatalf("rsyncMountArgs missing --timeout flag: %v", args)
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
