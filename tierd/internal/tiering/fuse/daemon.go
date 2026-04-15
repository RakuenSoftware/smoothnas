package fuse

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxRestarts   = 5
	restartWindow = 5 * time.Minute
)

type daemonEntry struct {
	cmd        *exec.Cmd
	pid        int
	startTimes []time.Time
}

// DaemonSupervisor manages the lifecycle of the tierd-fuse-ns binary per
// namespace. It is backend-agnostic — the same supervisor is used by mdadm,
// zfs-managed, and any future adapter.
type DaemonSupervisor struct {
	mu      sync.Mutex
	daemons map[string]*daemonEntry
}

// NewDaemonSupervisor creates a new DaemonSupervisor.
func NewDaemonSupervisor() *DaemonSupervisor {
	return &DaemonSupervisor{
		daemons: make(map[string]*daemonEntry),
	}
}

// FindDaemonBinary returns the path to the tierd-fuse-ns daemon binary.
func FindDaemonBinary() (string, error) {
	if path, err := exec.LookPath("tierd-fuse-ns"); err == nil {
		return path, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(self), "tierd-fuse-ns")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("tierd-fuse-ns binary not found in PATH or next to executable")
}

// findPassthroughFixup looks for the LD_PRELOAD shim that patches Debian's
// libfuse3 INIT reply to include the flags2 passthrough bit.
func findPassthroughFixup() string {
	candidates := []string{
		"/usr/local/lib/fuse_passthrough_fixup.so",
	}
	self, err := os.Executable()
	if err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "fuse_passthrough_fixup.so"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// daemonCmd creates an exec.Cmd for the FUSE daemon with LD_PRELOAD set
// if the passthrough fixup shim is available.
func daemonCmd(binary, socketPath, mountPath string) *exec.Cmd {
	cmd := exec.Command(binary, socketPath, mountPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if fixup := findPassthroughFixup(); fixup != "" {
		cmd.Env = append(os.Environ(), "LD_PRELOAD="+fixup)
	}
	return cmd
}

// ClearStaleFuseMount issues a lazy umount on mountPath to remove any stale
// FUSE entry left by a previously-killed daemon. The kernel keeps the mount
// entry alive after the daemon exits, so the next daemon launch fails with
// "Transport endpoint is not connected" unless the entry is cleared first.
// The call is best-effort: errors are logged but never returned.
func ClearStaleFuseMount(mountPath string) {
	out, err := exec.Command("umount", "-l", mountPath).CombinedOutput()
	if err != nil {
		// "not mounted" / EINVAL is the normal case on a clean start — ignore it.
		// Log anything else so genuine umount failures are visible.
		msg := string(out)
		if !strings.Contains(msg, "not mounted") && !strings.Contains(msg, "no mount point") {
			log.Printf("fuse: ClearStaleFuseMount %s: %v: %s", mountPath, err, msg)
		}
	}
}

// Start launches the tierd-fuse-ns daemon for the given namespace.
func (d *DaemonSupervisor) Start(namespaceID, mountPath, socketPath string) error {
	binary, err := FindDaemonBinary()
	if err != nil {
		return fmt.Errorf("find daemon binary: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if entry, ok := d.daemons[namespaceID]; ok && entry.pid != 0 {
		return fmt.Errorf("daemon for namespace %q is already running (pid %d)", namespaceID, entry.pid)
	}

	ClearStaleFuseMount(mountPath)
	cmd := daemonCmd(binary, socketPath, mountPath)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tierd-fuse-ns for namespace %q: %w", namespaceID, err)
	}

	entry := &daemonEntry{
		cmd:        cmd,
		pid:        cmd.Process.Pid,
		startTimes: []time.Time{time.Now()},
	}
	d.daemons[namespaceID] = entry
	return nil
}

// Stop signals the daemon to exit and removes its tracking entry.
func (d *DaemonSupervisor) Stop(namespaceID string) error {
	d.mu.Lock()
	entry, ok := d.daemons[namespaceID]
	if !ok {
		d.mu.Unlock()
		return nil
	}
	delete(d.daemons, namespaceID)
	d.mu.Unlock()

	if entry.cmd == nil || entry.cmd.Process == nil {
		return nil
	}
	if err := entry.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = entry.cmd.Process.Kill()
	}
	_ = entry.cmd.Wait()
	return nil
}

// Supervise runs a goroutine that waits for the daemon to exit and calls
// onCrash if it terminates unexpectedly.
func (d *DaemonSupervisor) Supervise(namespaceID string, onCrash func()) {
	d.mu.Lock()
	entry, ok := d.daemons[namespaceID]
	d.mu.Unlock()

	if !ok || entry.cmd == nil {
		return
	}

	go func() {
		_ = entry.cmd.Wait()

		d.mu.Lock()
		current, stillPresent := d.daemons[namespaceID]
		crashed := stillPresent && current.pid == entry.pid
		d.mu.Unlock()

		if crashed {
			onCrash()
		}
	}()
}

// ActivePID returns the PID of the running daemon, or 0 if none.
func (d *DaemonSupervisor) ActivePID(namespaceID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if entry, ok := d.daemons[namespaceID]; ok {
		return entry.pid
	}
	return 0
}

// Restart attempts to restart the daemon with exponential backoff.
func (d *DaemonSupervisor) Restart(namespaceID, mountPath, socketPath string) error {
	d.mu.Lock()
	entry, ok := d.daemons[namespaceID]
	if !ok {
		entry = &daemonEntry{}
		d.daemons[namespaceID] = entry
	}

	cutoff := time.Now().Add(-restartWindow)
	pruned := entry.startTimes[:0]
	for _, t := range entry.startTimes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	entry.startTimes = pruned

	if len(entry.startTimes) >= maxRestarts {
		d.mu.Unlock()
		return fmt.Errorf("daemon for namespace %q has crashed %d times in %v; giving up",
			namespaceID, maxRestarts, restartWindow)
	}
	backoff := time.Duration(1<<uint(len(entry.startTimes))) * time.Second
	d.mu.Unlock()

	if backoff > 60*time.Second {
		backoff = 60 * time.Second
	}
	log.Printf("fuse: restarting daemon for namespace %q in %v", namespaceID, backoff)
	time.Sleep(backoff)

	binary, err := FindDaemonBinary()
	if err != nil {
		return fmt.Errorf("find daemon binary for restart: %w", err)
	}

	ClearStaleFuseMount(mountPath)
	cmd := daemonCmd(binary, socketPath, mountPath)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("restart tierd-fuse-ns for namespace %q: %w", namespaceID, err)
	}

	d.mu.Lock()
	entry, ok = d.daemons[namespaceID]
	if !ok {
		entry = &daemonEntry{}
		d.daemons[namespaceID] = entry
	}
	entry.cmd = cmd
	entry.pid = cmd.Process.Pid
	entry.startTimes = append(entry.startTimes, time.Now())
	d.mu.Unlock()

	return nil
}
