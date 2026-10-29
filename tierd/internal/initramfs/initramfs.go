// Package initramfs rebuilds the initramfs safely.
//
// Debian's update-initramfs builds into initrd.img-<version>.new and then
// renames it into place, with no locking. Two runs at once write the same
// .new file and leave a corrupt image that the kernel cannot unpack, so the
// next boot panics ("Initramfs unpacking failed"). tierd triggered that by
// creating two mdadm arrays concurrently, each job refreshing the initramfs.
package initramfs

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
)

// lockPath serialises rebuilds with anything else that takes the same lock.
const lockPath = "/run/lock/smoothnas-initramfs.lock"

var (
	mu sync.Mutex

	// run executes a command and returns its combined output; a var so
	// tests can fake it.
	run = func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
)

// Update rebuilds the initramfs of every installed kernel that has one, one
// rebuild at a time, then checks each new image unpacks. A rebuild whose
// image does not unpack is retried once before Update reports the failure.
func Update() error {
	mu.Lock()
	defer mu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		out, err := run("flock", lockPath, "update-initramfs", "-u", "-k", "all")
		if err != nil {
			lastErr = fmt.Errorf("update-initramfs: %v: %s", err, out)
			log.Printf("initramfs: attempt %d: %v", attempt, lastErr)
			continue
		}
		if err := verify(); err != nil {
			lastErr = err
			log.Printf("initramfs: attempt %d: %v", attempt, lastErr)
			continue
		}
		return nil
	}
	return lastErr
}

// verify unpacks every initramfs image with lsinitramfs, which decompresses
// the whole archive and fails on corruption.
func verify() error {
	out, err := run("sh", "-c", "ls /boot/initrd.img-* 2>/dev/null")
	if err != nil || out == "" {
		return nil // no images to check (e.g. a development box)
	}
	for _, image := range strings.Fields(out) {
		if listing, err := run("lsinitramfs", image); err != nil {
			return fmt.Errorf("initramfs %s does not unpack: %v: %s", image, err, lastLine(listing))
		}
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
