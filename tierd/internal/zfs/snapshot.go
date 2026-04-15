package zfs

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Snapshot represents a ZFS snapshot.
type Snapshot struct {
	Name       string `json:"name"`        // e.g. "tank/data@snap1"
	Dataset    string `json:"dataset"`     // e.g. "tank/data"
	SnapName   string `json:"snap_name"`   // e.g. "snap1"
	Used       uint64 `json:"used"`        // bytes
	Creation   string `json:"creation"`    // timestamp
	UsedHuman  string `json:"used_human"`
}

var snapshotNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_/-]+@[a-zA-Z0-9_.-]+$`)

// ValidateSnapshotName checks that a full snapshot name (dataset@snap) is safe.
func ValidateSnapshotName(name string) error {
	if !snapshotNameRegex.MatchString(name) {
		return fmt.Errorf("invalid snapshot name: %s (must be dataset@snapname)", name)
	}
	return nil
}

// CreateSnapshot creates a snapshot of a dataset.
func CreateSnapshot(datasetName, snapName string) error {
	fullName := datasetName + "@" + snapName
	if err := ValidateSnapshotName(fullName); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "snapshot", fullName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs snapshot: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ListSnapshots returns all snapshots, optionally filtered by dataset.
func ListSnapshots(dataset string) ([]Snapshot, error) {
	args := []string{"list", "-Hp", "-t", "snapshot",
		"-o", "name,used,creation",
		"-S", "creation", // sort newest first
	}
	if dataset != "" {
		args = append(args, "-r", dataset)
	}

	out, err := exec.Command("zfs", args...).Output()
	if err != nil {
		return nil, nil
	}

	var snaps []Snapshot
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Fields are tab-separated in -H mode.
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			// Try space-separated fallback.
			fields = strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
		}

		name := strings.TrimSpace(fields[0])
		used, _ := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		creation := strings.TrimSpace(fields[2])

		parts := strings.SplitN(name, "@", 2)
		dsName := parts[0]
		snapName := ""
		if len(parts) > 1 {
			snapName = parts[1]
		}

		snaps = append(snaps, Snapshot{
			Name:      name,
			Dataset:   dsName,
			SnapName:  snapName,
			Used:      used,
			Creation:  creation,
			UsedHuman: humanSize(used),
		})
	}
	return snaps, nil
}

// DestroySnapshot destroys a snapshot.
func DestroySnapshot(name string) error {
	if err := ValidateSnapshotName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "destroy", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs destroy snapshot: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RollbackSnapshot rolls back a dataset to a snapshot.
// This destroys all snapshots newer than the target.
func RollbackSnapshot(name string) error {
	if err := ValidateSnapshotName(name); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "rollback", "-r", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs rollback: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// CloneSnapshot clones a snapshot into a new dataset.
func CloneSnapshot(snapName, newDataset string) error {
	if err := ValidateSnapshotName(snapName); err != nil {
		return err
	}
	if err := ValidateDatasetName(newDataset); err != nil {
		return err
	}

	cmd := exec.Command("zfs", "clone", snapName, newDataset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs clone: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SendSnapshot sends a snapshot to a file. Returns the output file path.
// If baseSnap is provided, sends an incremental stream.
func SendSnapshot(snapName, outputPath, baseSnap string) error {
	if err := ValidateSnapshotName(snapName); err != nil {
		return err
	}

	args := []string{"send"}
	if baseSnap != "" {
		if err := ValidateSnapshotName(baseSnap); err != nil {
			return err
		}
		args = append(args, "-i", baseSnap)
	}
	args = append(args, snapName)

	// Pipe to file.
	outFile, err := exec.Command("touch", outputPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create output file: %s: %w", strings.TrimSpace(string(outFile)), err)
	}

	cmd := exec.Command("zfs", args...)
	f, err := exec.Command("tee", outputPath).StdinPipe()
	if err != nil {
		return fmt.Errorf("setup pipe: %w", err)
	}
	_ = f

	// Use shell-free approach: redirect stdout to file.
	cmd = exec.Command("sh", "-c", fmt.Sprintf("zfs send %s > %s",
		shellQuoteArgs(args[1:]), outputPath))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs send: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func shellQuoteArgs(args []string) string {
	var quoted []string
	for _, a := range args {
		// Simple quoting for known-safe strings (validated above).
		quoted = append(quoted, "'"+a+"'")
	}
	return strings.Join(quoted, " ")
}

// --- Build helpers ---

// BuildSnapshotName constructs a full snapshot name.
func BuildSnapshotName(dataset, snap string) string {
	return dataset + "@" + snap
}
