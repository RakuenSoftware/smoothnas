// Package iso tests validate the ISO build scripts, preseed config,
// and related files for correctness.
package iso_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isoDir returns the path to the iso/ directory.
func isoDir(t *testing.T) string {
	t.Helper()
	// Walk up from the test file to find the project root.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// We're in tierd/internal/iso, go up 3 levels to project root.
	root := filepath.Join(dir, "..", "..", "..")
	isoPath := filepath.Join(root, "iso")
	if _, err := os.Stat(isoPath); err != nil {
		t.Fatalf("iso dir not found at %s: %v", isoPath, err)
	}
	return isoPath
}

// --- Shell syntax validation ---

func TestBuildISOShellSyntax(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	cmd := exec.Command("bash", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-iso.sh has syntax errors:\n%s", string(out))
	}
}

func TestInstallerShellSyntax(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	cmd := exec.Command("bash", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("smoothnas-install has syntax errors:\n%s", string(out))
	}
}

func TestLateCommandShellSyntax(t *testing.T) {
	path := filepath.Join(isoDir(t), "late-command.sh")
	if _, err := os.Stat(path); err != nil {
		t.Skip("late-command.sh not present (custom installer handles post-install)")
	}
	cmd := exec.Command("bash", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("late-command.sh has syntax errors:\n%s", string(out))
	}
}

func TestFirstbootShellSyntax(t *testing.T) {
	path := filepath.Join(isoDir(t), "firstboot.sh")
	if _, err := os.Stat(path); err != nil {
		t.Skip("firstboot.sh not present (custom installer handles first boot)")
	}
	cmd := exec.Command("bash", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("firstboot.sh has syntax errors:\n%s", string(out))
	}
}

// --- Script executable permissions ---

func TestScriptsAreExecutable(t *testing.T) {
	scripts := []string{"build-iso.sh"}
	dir := isoDir(t)

	for _, script := range scripts {
		path := filepath.Join(dir, script)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		if info.Mode()&0111 == 0 {
			t.Errorf("%s is not executable (mode %o)", script, info.Mode())
		}
	}
}

// --- Preseed validation ---

func TestPreseedExists(t *testing.T) {
	path := filepath.Join(isoDir(t), "preseed.cfg")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("preseed.cfg not found: %v", err)
	}
}

func TestPreseedHandsOffToInstaller(t *testing.T) {
	path := filepath.Join(isoDir(t), "preseed.cfg")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preseed: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "preseed/early_command") {
		t.Error("preseed should use early_command to hand off to custom installer")
	}
	if !strings.Contains(content, "smoothnas-install") {
		t.Error("preseed early_command should exec smoothnas-install")
	}
}

// --- Custom installer validation ---

func TestInstallerHasRequiredStages(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	stages := []struct {
		substring   string
		description string
	}{
		{"setup_env", "environment setup"},
		{"setup_network", "network configuration"},
		{"select_disks", "disk selection"},
		{"prompt_password", "admin password"},
		{"do_partitioning", "disk partitioning"},
		{"install_base", "base system install"},
		{"install_packages", "package installation"},
		{"configure_system", "system configuration"},
	}

	for _, s := range stages {
		if !strings.Contains(content, s.substring) {
			t.Errorf("smoothnas-install missing stage: %s (%s)", s.substring, s.description)
		}
	}
}

func TestInstallerLoadsNetworkModules(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "virtio_net") {
		t.Error("installer should load virtio_net module")
	}
	if !strings.Contains(content, "failover") {
		t.Error("installer should load failover module (virtio_net dependency)")
	}
	if !strings.Contains(content, "insmod") {
		t.Error("installer should use insmod for network modules (busybox modprobe lacks dep resolution)")
	}
}

func TestInstallerLoadsDeviceMapper(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "dm-mod") {
		t.Error("installer should load dm-mod module (required for LVM device nodes)")
	}
}

func TestBuildISOInjectsDMModules(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "dm-modules") {
		t.Error("build-iso.sh should extract dm-modules udeb for LVM support")
	}
}

func TestInstallerNoBusyboxIncompatiblePatterns(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "grep -oP") {
		t.Error("installer must not use grep -oP (requires Perl regex, unavailable in busybox)")
	}
	if strings.Contains(content, "grep -P") {
		t.Error("installer must not use grep -P (requires Perl regex, unavailable in busybox)")
	}
}

func TestInstallerHasErrorHandling(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "die") {
		t.Error("installer should have a die() function for fatal errors")
	}
	if !strings.Contains(content, "Dropping to shell") {
		t.Error("installer should drop to shell on fatal error for debugging")
	}
}

// --- Build script validation ---

func TestBuildISOHasVersionArg(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "${1:?") {
		t.Error("build-iso.sh should require a version argument")
	}
}

func TestBuildISOChecksPrereqs(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	for _, tool := range []string{"xorriso", "cpio", "gzip"} {
		if !strings.Contains(content, tool) {
			t.Errorf("build-iso.sh should check for %s", tool)
		}
	}
}

func TestBuildISOChecksArtifacts(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "smoothnas-install") {
		t.Error("build-iso.sh should check for the custom installer script")
	}
	if !strings.Contains(content, "preseed.cfg") {
		t.Error("build-iso.sh should check for preseed config")
	}
}

func TestBuildISOUsesXorriso(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "xorriso") {
		t.Error("build-iso.sh should use xorriso to repack the ISO")
	}
	if !strings.Contains(content, "isohybrid") {
		t.Error("build-iso.sh should create a hybrid ISO (USB bootable)")
	}
}

func TestBuildISOInjectsNetworkModules(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "nic-modules") {
		t.Error("build-iso.sh should extract network modules from nic-modules udeb")
	}
	if !strings.Contains(content, "virtio_net") {
		t.Error("build-iso.sh should include virtio_net module")
	}
	if !strings.Contains(content, "usr/lib/modules") {
		t.Error("build-iso.sh should place modules under usr/lib/modules (initrd layout)")
	}
}

// --- File set completeness ---

func TestAllISOFilesExist(t *testing.T) {
	dir := isoDir(t)
	required := []string{
		"build-iso.sh",
		"preseed.cfg",
		"smoothnas-install",
	}

	for _, f := range required {
		path := filepath.Join(dir, f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing required ISO file: %s", f)
		}
	}
}
