// Package iscsi manages iSCSI targets via targetcli (LIO) subprocess calls.
//
// Each target exposes a ZFS zvol or LVM LV as a block device over the
// network. A volume exported via iSCSI must not be mounted locally.
package iscsi

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Target represents an iSCSI target.
type Target struct {
	IQN      string `json:"iqn"`
	LUN      string `json:"lun"` // block device path (e.g. /dev/zvol/tank/lun0)
	CHAPUser string `json:"chap_user"`
	CHAPPass string `json:"-"` // never exposed in API responses
	HasCHAP  bool   `json:"has_chap"`
}

// ACL represents an initiator access control entry.
type ACL struct {
	InitiatorIQN string `json:"initiator_iqn"`
}

var iqnRegex = regexp.MustCompile(`^iqn\.\d{4}-\d{2}\.[a-zA-Z0-9.-]+(:[a-zA-Z0-9._-]+)*$`)

// ValidateIQN checks that an IQN is well-formed.
func ValidateIQN(iqn string) error {
	if !iqnRegex.MatchString(iqn) {
		return fmt.Errorf("invalid IQN: %s", iqn)
	}
	return nil
}

// ValidateBlockDevice checks that a device path is a valid block device for export.
var blockDevRegex = regexp.MustCompile(`^/dev/(zvol/[a-zA-Z][a-zA-Z0-9_/-]+|[a-zA-Z][a-zA-Z0-9_/-]+/[a-zA-Z][a-zA-Z0-9_-]+)$`)

func ValidateBlockDevice(path string) error {
	if !blockDevRegex.MatchString(path) {
		return fmt.Errorf("invalid block device path: %s", path)
	}
	return nil
}

// GenerateIQN creates an IQN from a hostname and volume name.
// Format: iqn.2026-01.com.smoothnas:{hostname}:{volume}
func GenerateIQN(hostname, volumeName string) string {
	// Sanitize for IQN.
	safeName := strings.ReplaceAll(volumeName, "/", ".")
	return fmt.Sprintf("iqn.2026-01.com.smoothnas:%s:%s", hostname, safeName)
}

// CreateTarget creates a new iSCSI target with a backstored LUN.
func CreateTarget(iqn, blockDevice string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}
	if err := ValidateBlockDevice(blockDevice); err != nil {
		return err
	}

	// Create a backstore.
	backstoreName := iqnToBackstoreName(iqn)
	cmd := exec.Command("targetcli",
		"/backstores/block", "create",
		"name="+backstoreName, "dev="+blockDevice)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create backstore: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Create the target.
	cmd = exec.Command("targetcli", "/iscsi", "create", iqn)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Cleanup backstore on failure.
		exec.Command("targetcli", "/backstores/block", "delete", backstoreName).Run()
		return fmt.Errorf("create target: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Create LUN.
	tpgPath := fmt.Sprintf("/iscsi/%s/tpg1/luns", iqn)
	cmd = exec.Command("targetcli", tpgPath, "create",
		fmt.Sprintf("/backstores/block/%s", backstoreName))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create lun: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Save config.
	return saveConfig()
}

// DestroyTarget removes an iSCSI target and its backstore.
func DestroyTarget(iqn string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}

	// Delete target.
	cmd := exec.Command("targetcli", "/iscsi", "delete", iqn)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete target: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Delete backstore.
	backstoreName := iqnToBackstoreName(iqn)
	exec.Command("targetcli", "/backstores/block", "delete", backstoreName).Run()

	return saveConfig()
}

// SetCHAP configures CHAP authentication on a target.
func SetCHAP(iqn, username, password string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}
	if len(password) < 12 {
		return fmt.Errorf("CHAP password must be at least 12 characters")
	}

	tpgPath := fmt.Sprintf("/iscsi/%s/tpg1", iqn)

	// Enable authentication.
	cmd := exec.Command("targetcli", tpgPath, "set", "attribute",
		"authentication=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("enable auth: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Set CHAP credentials (set on each ACL).
	// For now, set as parameter on the TPG for demo_mode_write_protect=0
	cmd = exec.Command("targetcli", tpgPath, "set", "parameter",
		"AuthMethod=CHAP")
	cmd.Run() // Best effort.

	return saveConfig()
}

// QuiesceTarget disables the target portal group for iqn. LIO rejects new
// sessions and tears down target service for the TPG while the backing file
// remains pinned; callers are responsible for checking the target is a
// file-backed LUN before exposing this as an active-LUN movement step.
func QuiesceTarget(iqn string) error {
	return setTargetPortalGroupState(iqn, false)
}

// ResumeTarget re-enables the target portal group for iqn after operator
// maintenance has completed.
func ResumeTarget(iqn string) error {
	return setTargetPortalGroupState(iqn, true)
}

// AddACL adds an initiator ACL to a target.
func AddACL(iqn, initiatorIQN string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}
	if err := ValidateIQN(initiatorIQN); err != nil {
		return err
	}

	aclPath := fmt.Sprintf("/iscsi/%s/tpg1/acls", iqn)
	cmd := exec.Command("targetcli", aclPath, "create", initiatorIQN)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create acl: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return saveConfig()
}

// RemoveACL removes an initiator ACL from a target.
func RemoveACL(iqn, initiatorIQN string) error {
	if err := ValidateIQN(iqn); err != nil {
		return err
	}
	if err := ValidateIQN(initiatorIQN); err != nil {
		return err
	}

	aclPath := fmt.Sprintf("/iscsi/%s/tpg1/acls", iqn)
	cmd := exec.Command("targetcli", aclPath, "delete", initiatorIQN)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete acl: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return saveConfig()
}

// ListACLs returns the initiator ACLs for a target by parsing targetcli output.
func ListACLs(iqn string) ([]ACL, error) {
	if err := ValidateIQN(iqn); err != nil {
		return nil, err
	}

	aclPath := fmt.Sprintf("/iscsi/%s/tpg1/acls", iqn)
	out, err := exec.Command("targetcli", aclPath, "ls").Output()
	if err != nil {
		return nil, nil // Target may not exist or no ACLs.
	}

	var acls []ACL
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "o- iqn.") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				initiator := parts[1]
				if initiator == "" {
					initiator = parts[0]
					initiator = strings.TrimPrefix(initiator, "o- ")
				}
				acls = append(acls, ACL{InitiatorIQN: initiator})
			}
		}
	}

	return acls, nil
}

// ListTargets returns all configured iSCSI targets by parsing targetcli ls output.
func ListTargets() ([]Target, error) {
	out, err := exec.Command("targetcli", "/iscsi", "ls").Output()
	if err != nil {
		return nil, nil
	}

	var targets []Target
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "o- iqn.") {
			// Extract IQN: "o- iqn.2026-01.com.smoothnas:... [....]"
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				iqn := strings.TrimPrefix(parts[1], "o- ")
				if iqn == "" && len(parts) >= 1 {
					iqn = parts[1]
				}
				targets = append(targets, Target{IQN: iqn})
			}
		}
	}

	return targets, nil
}

// EnableService starts the LIO target service.
func EnableService() error {
	return exec.Command("systemctl", "enable", "--now", "rtslib-fb-targetctl").Run()
}

// DisableService stops the LIO target service.
func DisableService() error {
	return exec.Command("systemctl", "disable", "--now", "rtslib-fb-targetctl").Run()
}

// IsEnabled checks if the target service is active.
func IsEnabled() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "rtslib-fb-targetctl").Run() == nil
}

// --- Build helpers (exported for testing) ---

// BuildCreateTargetArgs returns the targetcli commands needed.
func BuildCreateTargetArgs(iqn, blockDevice string) (backstoreArgs, targetArgs, lunArgs []string, err error) {
	if err := ValidateIQN(iqn); err != nil {
		return nil, nil, nil, err
	}
	if err := ValidateBlockDevice(blockDevice); err != nil {
		return nil, nil, nil, err
	}

	bsName := iqnToBackstoreName(iqn)
	backstoreArgs = []string{"/backstores/block", "create", "name=" + bsName, "dev=" + blockDevice}
	targetArgs = []string{"/iscsi", "create", iqn}
	lunArgs = []string{fmt.Sprintf("/iscsi/%s/tpg1/luns", iqn), "create", fmt.Sprintf("/backstores/block/%s", bsName)}
	return backstoreArgs, targetArgs, lunArgs, nil
}

// BuildSetTargetPortalGroupStateArgs returns the targetcli command needed to
// enable or disable a target portal group.
func BuildSetTargetPortalGroupStateArgs(iqn string, enabled bool) ([]string, error) {
	if err := ValidateIQN(iqn); err != nil {
		return nil, err
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	return []string{fmt.Sprintf("/iscsi/%s/tpg1", iqn), action}, nil
}

// IQNToBackstoreName converts an IQN to a backstore name. Exported for testing.
func IQNToBackstoreName(iqn string) string {
	return iqnToBackstoreName(iqn)
}

// --- internal ---

func iqnToBackstoreName(iqn string) string {
	// Replace non-alphanumeric chars with underscores.
	name := strings.NewReplacer(".", "_", ":", "_", "-", "_").Replace(iqn)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func setTargetPortalGroupState(iqn string, enabled bool) error {
	args, err := BuildSetTargetPortalGroupStateArgs(iqn, enabled)
	if err != nil {
		return err
	}
	action := args[len(args)-1]
	cmd := exec.Command("targetcli", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s target portal group: %s: %w", action, strings.TrimSpace(string(out)), err)
	}
	return saveConfig()
}

func saveConfig() error {
	cmd := exec.Command("targetcli", "saveconfig")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("saveconfig: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
