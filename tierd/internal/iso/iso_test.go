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

func TestFirstbootDoesNotBlockOnNginxRestart(t *testing.T) {
	path := filepath.Join(isoDir(t), "firstboot.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read firstboot.sh: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "systemctl --no-block restart nginx") {
		t.Error("firstboot.sh should restart nginx with --no-block to avoid deadlocking against tierd startup")
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

func TestInstallerInstallsTierRuntimePrereqs(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "xfsprogs") {
		t.Error("installer should install xfsprogs for mkfs.xfs tier provisioning")
	}
	if !strings.Contains(content, "tierd-host-init.service") ||
		!strings.Contains(content, "ExecStart=/usr/local/bin/tierd __host_init") ||
		!strings.Contains(content, "systemctl enable tierd-host-init.service") {
		t.Error("installer should install and enable tierd host initialization")
	}
	if !strings.Contains(content, "PrivateTmp=false") {
		t.Error("installer tierd.service should match the deployed service PrivateTmp setting")
	}
}

func TestInstallerDefersDKMSWorkToFirstBoot(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "Deferring OpenZFS and smoothfs DKMS builds to first boot") {
		t.Error("installer should explicitly defer DKMS-backed storage setup to first boot")
	}
	if strings.Contains(content, "dkms add -m smoothfs") ||
		strings.Contains(content, "dkms build -m smoothfs") ||
		strings.Contains(content, "apt-get install -y ${SMOOTHKERNEL_ZFS_PACKAGES}") {
		t.Error("installer should not run ZFS or smoothfs DKMS work")
	}
}

func TestInstallerBootstrapsSmoothfsFromBundledSource(t *testing.T) {
	path := filepath.Join(isoDir(t), "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "/smoothnas/package-manifest") {
		t.Error("installer should read the bundled package manifest")
	}
	if !strings.Contains(content, "file:/opt/smoothnas/repo") {
		t.Error("installer should expose the bundled local apt repo inside the target")
	}
	if !strings.Contains(content, "/opt/smoothnas/smoothfs-src") {
		t.Error("installer should stage bundled smoothfs source for first boot")
	}
	if !strings.Contains(content, "ensure_kernel_headers_ready") ||
		!strings.Contains(content, "${SMOOTHKERNEL_IMAGE_PACKAGE#linux-image-}") {
		t.Error("installer should validate SmoothKernel headers before first boot")
	}
	if strings.Contains(content, "apt-get build-dep -y samba") ||
		strings.Contains(content, "apt-get source samba=") ||
		strings.Contains(content, "samba-vfs/build.sh") ||
		strings.Contains(content, "deb-src $DEBIAN_MIRROR") ||
		strings.Contains(content, "samba cifs-utils") {
		t.Error("installer should not install Samba or pull Samba source/build dependencies")
	}
}

func TestFirstBootInstallsDKMSAndSamba(t *testing.T) {
	path := filepath.Join(isoDir(t), "firstboot.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "apt-get install -y $SMOOTHKERNEL_ZFS_PACKAGES") {
		t.Error("firstboot should install bundled OpenZFS packages")
	}
	if !strings.Contains(content, "dkms add -m smoothfs") ||
		!strings.Contains(content, "dkms build -m smoothfs") ||
		!strings.Contains(content, "dkms install -m smoothfs") {
		t.Error("firstboot should build and install smoothfs through DKMS")
	}
	if !strings.Contains(content, "apt-get install -y -qq samba cifs-utils") {
		t.Error("firstboot should install Samba/CIFS instead of the installer")
	}
	if !strings.Contains(content, "apt-get source --print-uris samba") ||
		!strings.Contains(content, "prepare_samba_source") {
		t.Error("firstboot should verify Debian source repos before building Samba VFS")
	}
	if !strings.Contains(content, "dpkg-buildpackage -us -uc -b") ||
		!strings.Contains(content, "install_smoothfs_samba_vfs") {
		t.Error("firstboot should build and install smoothfs-samba-vfs as a Debian package")
	}
	if !strings.Contains(content, "modprobe zfs") || !strings.Contains(content, "modprobe smoothfs") {
		t.Error("firstboot should load DKMS modules after installing them")
	}
}

func TestBuildISOInjectsSmoothKernelModules(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "setup_installer_kernel") {
		t.Error("build-iso.sh should replace the Debian installer kernel with SmoothKernel")
	}
	if !strings.Contains(content, "INSTALLER_KERNEL_VERSION") {
		t.Error("build-iso.sh should track the SmoothKernel module version for the initrd")
	}
	if !strings.Contains(content, "usr/lib/modules") {
		t.Error("build-iso.sh should place SmoothKernel modules under usr/lib/modules")
	}
	if !strings.Contains(content, "dpkg-scanpackages") || !strings.Contains(content, "package-manifest") {
		t.Error("build-iso.sh should generate a bundled local package repo manifest")
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
	dir := isoDir(t)
	path := filepath.Join(dir, "smoothnas-install")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "die") {
		t.Error("installer should have a die() function for fatal errors")
	}
	// die() now routes through the i18n dispatcher
	// (installer.error.shell key) instead of carrying the
	// English string inline. Confirm both halves are wired:
	if !strings.Contains(content, "installer.error.shell") {
		t.Error("die() should look up installer.error.shell via t() so the message localises")
	}
	if !strings.Contains(content, "exec /bin/sh") {
		t.Error("die() should drop to shell on fatal error for debugging")
	}
	// And the English fallback is in the locale bundle.
	enBundle, err := os.ReadFile(filepath.Join(dir, "locales", "en.properties"))
	if err != nil {
		t.Fatalf("read en.properties: %v", err)
	}
	if !strings.Contains(string(enBundle), "Dropping to shell") {
		t.Error("locales/en.properties: installer.error.shell should still mention 'Dropping to shell' in English")
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

	for _, tool := range []string{"xorriso", "cpio", "gzip", "dpkg-scanpackages"} {
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
	if !strings.Contains(content, "SMOOTHKERNEL_DIR") || !strings.Contains(content, "SMOOTHFS_REPO_URL") {
		t.Error("build-iso.sh should verify the bundled SmoothKernel and remote smoothfs artifacts")
	}
	if !strings.Contains(content, "linux-image-*smoothnas_*.deb") ||
		!strings.Contains(content, "linux-headers-*smoothnas_*.deb") ||
		!strings.Contains(content, "Refusing to use smoothnas-lts") {
		t.Error("build-iso.sh should select plain smoothnas kernel artifacts and reject smoothnas-lts")
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

func TestBuildISOUsesSmoothKernelForInstallerBoot(t *testing.T) {
	path := filepath.Join(isoDir(t), "build-iso.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "cp \"$vmlinuz\" \"${WORK_DIR}/install.amd/vmlinuz\"") {
		t.Error("build-iso.sh should copy the SmoothKernel vmlinuz into the installer boot path")
	}
	if !strings.Contains(content, "DEFAULT_SMOOTHKERNEL_DIR") ||
		!strings.Contains(content, "../smoothkernel/out-smoothnas") {
		t.Error("build-iso.sh should default to the sibling smoothkernel artifact directory")
	}
	if !strings.Contains(content, "SMOOTHFS_FETCH_DIR=\"${CACHE_DIR}/smoothfs-src\"") {
		t.Error("build-iso.sh should keep fetched smoothfs source outside the transient work dir")
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
