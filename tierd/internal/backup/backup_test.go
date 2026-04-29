package backup

import (
	"errors"
	"strings"
	"testing"
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
