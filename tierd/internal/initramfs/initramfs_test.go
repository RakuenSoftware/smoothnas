package initramfs

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateSerialisesRebuilds(t *testing.T) {
	var active, maxActive int32
	run = func(name string, args ...string) (string, error) {
		if name == "flock" {
			n := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxActive)
				if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return "", nil
		}
		return "", nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); Update() }()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("rebuilds overlapped: %d ran at once", maxActive)
	}
}

func TestUpdateRetriesCorruptImageOnce(t *testing.T) {
	var builds int
	corrupt := true
	run = func(name string, args ...string) (string, error) {
		switch name {
		case "flock":
			builds++
			return "", nil
		case "sh":
			return "/boot/initrd.img-7.2.10-76-smoothkernel", nil
		case "lsinitramfs":
			if corrupt {
				corrupt = false
				return "zstd: Data corruption detected", errors.New("exit status 1")
			}
			return "kernel/fs/smoothfs.ko", nil
		}
		return "", nil
	}
	if err := Update(); err != nil {
		t.Fatalf("expected the retry to succeed: %v", err)
	}
	if builds != 2 {
		t.Fatalf("expected 2 builds, got %d", builds)
	}
}

func TestUpdateReportsPersistentFailure(t *testing.T) {
	run = func(name string, args ...string) (string, error) {
		switch name {
		case "sh":
			return "/boot/initrd.img-x", nil
		case "lsinitramfs":
			return "Data corruption detected", errors.New("exit status 1")
		}
		return "", nil
	}
	err := Update()
	if err == nil || !strings.Contains(err.Error(), "does not unpack") {
		t.Fatalf("expected an unpack failure, got %v", err)
	}
}
