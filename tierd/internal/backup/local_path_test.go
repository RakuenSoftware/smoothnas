package backup

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct{ dev uint64 }

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return true }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Dev: f.dev} }

func TestValidateMountedLocalPathRejectsRootFallbackUnderMnt(t *testing.T) {
	orig := statPath
	statPath = func(path string) (fs.FileInfo, error) {
		switch path {
		case "/", "/mnt/media":
			return fakeInfo{dev: 1}, nil
		default:
			return nil, fmt.Errorf("unexpected path %s", path)
		}
	}
	defer func() { statPath = orig }()

	err := validateMountedLocalPath("/mnt/media/storage/backup")
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("expected root fallback error, got %v", err)
	}
}

func TestValidateMountedLocalPathRejectsMissingAnchorUnderMnt(t *testing.T) {
	orig := statPath
	statPath = func(path string) (fs.FileInfo, error) {
		switch path {
		case "/mnt/media":
			return nil, os.ErrNotExist
		default:
			return nil, fmt.Errorf("unexpected path %s", path)
		}
	}
	defer func() { statPath = orig }()

	err := validateMountedLocalPath("/mnt/media/storage/backup")
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("expected missing mount anchor error, got %v", err)
	}
}

func TestValidateMountedLocalPathAllowsMountedStorage(t *testing.T) {
	orig := statPath
	statPath = func(path string) (fs.FileInfo, error) {
		switch path {
		case "/":
			return fakeInfo{dev: 1}, nil
		case "/mnt/media":
			return fakeInfo{dev: 2}, nil
		default:
			return nil, fmt.Errorf("unexpected path %s", path)
		}
	}
	defer func() { statPath = orig }()

	if err := validateMountedLocalPath("/mnt/media/storage/backup"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMountedLocalPathAllowsNestedBackingMount(t *testing.T) {
	orig := statPath
	statPath = func(path string) (fs.FileInfo, error) {
		switch path {
		case "/":
			return fakeInfo{dev: 1}, nil
		case "/mnt/.tierd-backing/media/HDD":
			return fakeInfo{dev: 3}, nil
		default:
			return nil, fmt.Errorf("path %s does not exist", path)
		}
	}
	defer func() { statPath = orig }()

	if err := validateMountedLocalPath("/mnt/.tierd-backing/media/HDD/storage/backup"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
