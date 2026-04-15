package network

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultNetworkDir = "/etc/systemd/network"
	defaultBackupDir  = "/var/lib/tierd/network-backup"
	safeTimeout       = 90 * time.Second
)

// PendingChange represents a network change awaiting confirmation.
type PendingChange struct {
	Description string    `json:"description"`
	AppliedAt   time.Time `json:"applied_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Remaining   int       `json:"remaining_seconds"`
}

// SafeApply manages the test-and-confirm workflow for network changes.
type SafeApply struct {
	mu         sync.Mutex
	pending    bool
	timer      *time.Timer
	desc       string
	applied    time.Time
	networkDir string
	backupDir  string
	reloadFn   func() error // called after config write or restore
}

// NewSafeApply creates a SafeApply that operates on the real system directories.
func NewSafeApply() *SafeApply {
	return &SafeApply{
		networkDir: defaultNetworkDir,
		backupDir:  defaultBackupDir,
		reloadFn:   reloadNetworkd,
	}
}

// NewSafeApplyWithDirs creates a SafeApply with custom directories and reload function.
// Used for testing.
func NewSafeApplyWithDirs(networkDir, backupDir string, reloadFn func() error) *SafeApply {
	return &SafeApply{
		networkDir: networkDir,
		backupDir:  backupDir,
		reloadFn:   reloadFn,
	}
}

// Apply backs up current config, writes new config, reloads networkd,
// and starts the countdown timer. If not confirmed within 90 seconds,
// the old config is restored.
func (s *SafeApply) Apply(description string, writeConfig func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending {
		return fmt.Errorf("a network change is already pending confirmation")
	}

	// Backup current config.
	if err := s.backup(); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	// Write new config.
	if err := writeConfig(); err != nil {
		s.restore()
		return fmt.Errorf("write config failed: %w", err)
	}

	// Reload.
	if err := s.reloadFn(); err != nil {
		s.restore()
		s.reloadFn()
		return fmt.Errorf("reload failed: %w", err)
	}

	// Start countdown.
	s.pending = true
	s.desc = description
	s.applied = time.Now()

	s.timer = time.AfterFunc(safeTimeout, func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		if !s.pending {
			return
		}

		// Timeout: revert.
		s.restore()
		s.reloadFn()
		s.pending = false
		s.timer = nil
	})

	return nil
}

// Confirm makes the pending change permanent.
func (s *SafeApply) Confirm() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.pending {
		return fmt.Errorf("no pending change to confirm")
	}

	if s.timer != nil {
		s.timer.Stop()
	}

	// Remove backup (change is now permanent).
	os.RemoveAll(s.backupDir)

	s.pending = false
	s.timer = nil
	return nil
}

// Revert immediately restores the previous config.
func (s *SafeApply) Revert() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.pending {
		return fmt.Errorf("no pending change to revert")
	}

	if s.timer != nil {
		s.timer.Stop()
	}

	if err := s.restore(); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}

	if err := s.reloadFn(); err != nil {
		return fmt.Errorf("reload after revert failed: %w", err)
	}

	s.pending = false
	s.timer = nil
	return nil
}

// Status returns the current pending change, or nil if none.
func (s *SafeApply) Status() *PendingChange {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.pending {
		return nil
	}

	expires := s.applied.Add(safeTimeout)
	remaining := int(time.Until(expires).Seconds())
	if remaining < 0 {
		remaining = 0
	}

	return &PendingChange{
		Description: s.desc,
		AppliedAt:   s.applied,
		ExpiresAt:   expires,
		Remaining:   remaining,
	}
}

// IsPending returns whether a change is awaiting confirmation.
func (s *SafeApply) IsPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// --- internal ---

func (s *SafeApply) backup() error {
	os.RemoveAll(s.backupDir)
	if err := os.MkdirAll(s.backupDir, 0750); err != nil {
		return err
	}

	entries, err := os.ReadDir(s.networkDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No config to backup.
		}
		return err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(s.networkDir, e.Name())
		dst := filepath.Join(s.backupDir, e.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return err
		}
	}

	return nil
}

func (s *SafeApply) restore() error {
	// Remove current config files.
	entries, _ := os.ReadDir(s.networkDir)
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".network") ||
			strings.HasSuffix(e.Name(), ".netdev") ||
			strings.HasSuffix(e.Name(), ".link")) {
			os.Remove(filepath.Join(s.networkDir, e.Name()))
		}
	}

	// Restore from backup.
	backupEntries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return err
	}

	for _, e := range backupEntries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(s.backupDir, e.Name())
		dst := filepath.Join(s.networkDir, e.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return err
		}
	}

	return nil
}

func reloadNetworkd() error {
	cmd := exec.Command("networkctl", "reload")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("networkctl reload: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
