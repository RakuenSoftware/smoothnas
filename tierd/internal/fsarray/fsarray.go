// Package fsarray manages first-class btrfs and bcachefs arrays built from
// one or more raw disks.
package fsarray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	KindBtrfs    = "btrfs"
	KindBcachefs = "bcachefs"

	StateActive     = "active"
	StateError      = "error"
	StateDestroying = "destroying"

	DefaultMountBase = "/mnt"
)

var (
	nameRe         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	arrayCommand   = exec.Command
	arrayMkdirAll  = os.MkdirAll
	arrayRemoveAll = os.RemoveAll
	fstabPath      = "/etc/fstab"
)

// Plan is the validated shape of a filesystem array create request.
type Plan struct {
	Name            string
	Kind            string
	Label           string
	MountPath       string
	DataProfile     string
	MetadataProfile string
	Replicas        int
	Devices         []Device
	SizeBytes       uint64
}

// Device is a raw block device selected for a filesystem array.
type Device struct {
	Path string
	Size uint64
}

// BuildPlan validates an array request and fills in defaults.
func BuildPlan(name, kind, mountBase, dataProfile, metadataProfile string, replicas int, devices []Device) (*Plan, error) {
	name = strings.TrimSpace(name)
	kind = strings.ToLower(strings.TrimSpace(kind))
	dataProfile = strings.ToLower(strings.TrimSpace(dataProfile))
	metadataProfile = strings.ToLower(strings.TrimSpace(metadataProfile))
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid filesystem array name %q", name)
	}
	switch kind {
	case KindBtrfs:
		if len(devices) == 0 {
			return nil, fmt.Errorf("btrfs array requires at least one disk")
		}
		if dataProfile == "" {
			dataProfile = "single"
		}
		if metadataProfile == "" {
			if len(devices) == 1 {
				metadataProfile = "dup"
			} else {
				metadataProfile = "raid1"
			}
		}
		if err := validateBtrfsProfile("data", dataProfile, len(devices)); err != nil {
			return nil, err
		}
		if err := validateBtrfsProfile("metadata", metadataProfile, len(devices)); err != nil {
			return nil, err
		}
		replicas = 0
	case KindBcachefs:
		if len(devices) == 0 {
			return nil, fmt.Errorf("bcachefs array requires at least one disk")
		}
		if replicas == 0 {
			replicas = 1
		}
		if replicas < 1 || replicas > 3 {
			return nil, fmt.Errorf("bcachefs replicas must be between 1 and 3")
		}
		if replicas > len(devices) {
			return nil, fmt.Errorf("bcachefs replicas=%d requires at least %d disks", replicas, replicas)
		}
		dataProfile = ""
		metadataProfile = ""
	default:
		return nil, fmt.Errorf("unsupported filesystem array kind %q", kind)
	}
	if mountBase == "" {
		mountBase = DefaultMountBase
	}
	if err := validateDistinctDevices(devices); err != nil {
		return nil, err
	}

	plan := &Plan{
		Name:            name,
		Kind:            kind,
		Label:           labelFor(kind, name),
		MountPath:       filepath.Join(mountBase, name),
		DataProfile:     dataProfile,
		MetadataProfile: metadataProfile,
		Replicas:        replicas,
		Devices:         append([]Device(nil), devices...),
	}
	for _, dev := range devices {
		plan.SizeBytes += dev.Size
	}
	return plan, nil
}

func labelFor(kind, name string) string {
	return "smoothnas-" + kind + "-" + name
}

func validateDistinctDevices(devices []Device) error {
	seen := map[string]struct{}{}
	for _, dev := range devices {
		path := strings.TrimSpace(dev.Path)
		if path == "" {
			return fmt.Errorf("disk path is required")
		}
		if _, ok := seen[path]; ok {
			return fmt.Errorf("disk %s is assigned more than once", path)
		}
		seen[path] = struct{}{}
	}
	return nil
}

func validateBtrfsProfile(scope, profile string, disks int) error {
	min := map[string]int{
		"single": 1,
		"dup":    1,
		"raid0":  2,
		"raid1":  2,
		"raid10": 4,
		"raid5":  3,
		"raid6":  4,
	}
	required, ok := min[profile]
	if !ok {
		return fmt.Errorf("unsupported btrfs %s profile %q", scope, profile)
	}
	if disks < required {
		return fmt.Errorf("btrfs %s profile %s requires at least %d disks", scope, profile, required)
	}
	return nil
}

// Create destructively formats and mounts the filesystem array described by plan.
func Create(plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("filesystem array plan is required")
	}
	for _, dev := range plan.Devices {
		if err := run("wipefs", "-a", dev.Path); err != nil {
			return err
		}
	}
	switch plan.Kind {
	case KindBtrfs:
		args := BuildMkfsBtrfsArgs(plan)
		if err := run(args[0], args[1:]...); err != nil {
			return err
		}
		_ = arrayCommand("btrfs", "device", "scan").Run()
		return Mount(plan.Kind, "LABEL="+plan.Label, plan.MountPath)
	case KindBcachefs:
		args := BuildBcachefsFormatArgs(plan)
		if err := run(args[0], args[1:]...); err != nil {
			return err
		}
		return Mount(plan.Kind, BcachefsMountSource(plan), plan.MountPath)
	default:
		return fmt.Errorf("unsupported filesystem array kind %q", plan.Kind)
	}
}

// Destroy unmounts an array mount and wipes member signatures.
func Destroy(mountPath string, devices []string) error {
	_ = RemoveFSTabEntry(mountPath)
	if isMounted(mountPath) {
		if err := run("umount", mountPath); err != nil {
			if lazyErr := run("umount", "-l", mountPath); lazyErr != nil {
				return err
			}
		}
	}
	for _, dev := range devices {
		_ = run("wipefs", "-a", dev)
	}
	return arrayRemoveAll(mountPath)
}

func BuildMkfsBtrfsArgs(plan *Plan) []string {
	args := []string{
		"mkfs.btrfs", "-f",
		"-L", plan.Label,
		"-d", plan.DataProfile,
		"-m", plan.MetadataProfile,
	}
	for _, dev := range plan.Devices {
		args = append(args, dev.Path)
	}
	return args
}

func BuildBcachefsFormatArgs(plan *Plan) []string {
	args := []string{
		"bcachefs", "format",
		"--replicas=" + strconv.Itoa(plan.Replicas),
	}
	for _, dev := range plan.Devices {
		args = append(args, dev.Path)
	}
	return args
}

func BcachefsMountSource(plan *Plan) string {
	parts := make([]string, 0, len(plan.Devices))
	for _, dev := range plan.Devices {
		parts = append(parts, dev.Path)
	}
	return strings.Join(parts, ":")
}

func Mount(kind, source, mountPath string) error {
	if err := arrayMkdirAll(mountPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPath, err)
	}
	if isMounted(mountPath) {
		return EnsureFSTabEntry(source, mountPath, kind)
	}
	if err := run("mount", "-t", kind, source, mountPath); err != nil {
		return err
	}
	return EnsureFSTabEntry(source, mountPath, kind)
}

func EnsureFSTabEntry(source, mountPath, kind string) error {
	entry := fmt.Sprintf("%s %s %s defaults,nofail 0 0", source, mountPath, kind)
	return upsertFSTabEntry(source, mountPath, entry)
}

func RemoveFSTabEntry(mountPath string) error {
	data, err := os.ReadFile(fstabPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", fstabPath, err)
	}
	lines := strings.Split(string(data), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mountPath {
			continue
		}
		filtered = append(filtered, line)
	}
	output := strings.Join(filtered, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	if err := os.WriteFile(fstabPath, []byte(output), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", fstabPath, err)
	}
	return nil
}

func upsertFSTabEntry(source, mountPath, entry string) error {
	data, err := os.ReadFile(fstabPath)
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 2 && (fields[0] == source || fields[1] == mountPath) {
				if line == entry {
					return nil
				}
				lines[i] = entry
				output := strings.Join(lines, "\n")
				if !strings.HasSuffix(output, "\n") {
					output += "\n"
				}
				if err := os.WriteFile(fstabPath, []byte(output), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", fstabPath, err)
				}
				return nil
			}
		}
	}

	f, err := os.OpenFile(fstabPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", fstabPath, err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("append /etc/fstab: %w", err)
	}
	return nil
}

func isMounted(path string) bool {
	return arrayCommand("findmnt", "-M", path).Run() == nil
}

func run(name string, args ...string) error {
	out, err := arrayCommand(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Usage returns best-effort statfs usage for a mounted array.
func Usage(path string) (total, free uint64, mounted bool) {
	if !isMounted(path) {
		return 0, 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), true
}
