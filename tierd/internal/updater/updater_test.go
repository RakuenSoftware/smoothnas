package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKernelReleaseFromDepends(t *testing.T) {
	for depends, want := range map[string]string{
		"linux-image-7.2.10-74-smoothkernel (= 7.2.10-74)": "7.2.10-74-smoothkernel",
		"linux-image-7.2.10-74-smoothkernel":               "7.2.10-74-smoothkernel",
		"linux-image-6.12.0-1-amd64":                       "",
		"":                                                 "",
	} {
		if got := kernelReleaseFromDepends(depends); got != want {
			t.Errorf("kernelReleaseFromDepends(%q) = %q, want %q", depends, got, want)
		}
	}
}

func TestGetRebootStatus(t *testing.T) {
	dir := t.TempDir()
	debianRebootRequiredPath = filepath.Join(dir, "reboot-required")
	sysModuleDir = filepath.Join(dir, "sys-module")
	installedVersion = func(string) string { return "" }
	installed, running := "7.2.10-74-smoothkernel", "7.2.10-74-smoothkernel"
	installedKernel = func() string { return installed }
	runningKernel = func() string { return running }

	if st := GetRebootStatus(); st.Required {
		t.Fatalf("nothing pending: %+v", st)
	}

	// A newer SmoothKernel installed by apt: pending until the router reboots.
	installed = "7.2.10-75-smoothkernel"
	st := GetRebootStatus()
	if !st.Required || len(st.Reasons) != 1 ||
		st.Reasons[0] != "SmoothKernel 7.2.10-75-smoothkernel is installed; running 7.2.10-74-smoothkernel" {
		t.Fatalf("pending kernel: %+v", st)
	}

	// Debian's marker adds its packages.
	os.WriteFile(debianRebootRequiredPath, []byte("*** System restart required ***\n"), 0644)
	os.WriteFile(debianRebootRequiredPath+".pkgs", []byte("libc6\nlibc6\nlibssl3t64\n"), 0644)
	st = GetRebootStatus()
	if len(st.Reasons) != 2 || st.Reasons[1] != "Debian updates: libc6, libssl3t64" {
		t.Fatalf("with Debian marker: %+v", st)
	}

	// Rebooted: the running kernel is the installed one and the marker is gone.
	running = installed
	os.Remove(debianRebootRequiredPath)
	if st := GetRebootStatus(); st.Required {
		t.Fatalf("after reboot: %+v", st)
	}

	// No SmoothKernel package (e.g. a development box): no kernel reason.
	installed = ""
	if st := GetRebootStatus(); st.Required {
		t.Fatalf("without SmoothKernel: %+v", st)
	}
}

func TestRebootStatusForDKMSModules(t *testing.T) {
	dir := t.TempDir()
	debianRebootRequiredPath = filepath.Join(dir, "reboot-required")
	installedKernel = func() string { return "" }
	runningKernel = func() string { return "" }
	sysModuleDir = filepath.Join(dir, "sys-module")
	versions := map[string]string{
		"smoothfs-dkms":    "0.3.2-1",
		"openzfs-zfs-dkms": "2.3.4-1",
	}
	installedVersion = func(pkg string) string { return versions[pkg] }
	write := func(module, version string) {
		os.MkdirAll(filepath.Join(sysModuleDir, module), 0755)
		os.WriteFile(filepath.Join(sysModuleDir, module, "version"), []byte(version+"\n"), 0644)
	}

	// Module not loaded: nothing to compare.
	if st := GetRebootStatus(); st.Required {
		t.Fatalf("no modules loaded: %+v", st)
	}

	// smoothfs 0.3.2 installed while 0.3.1 is still loaded.
	write("smoothfs", "0.3.1")
	write("zfs", "2.3.4")
	st := GetRebootStatus()
	if !st.Required || len(st.Reasons) != 1 || st.Reasons[0] != "smoothfs 0.3.2 is installed; running 0.3.1" {
		t.Fatalf("pending smoothfs: %+v", st)
	}

	// A build suffix on the loaded version is not a pending update.
	write("smoothfs", "0.3.2")
	write("zfs", "2.3.4-80")
	if st := GetRebootStatus(); st.Required {
		t.Fatalf("loaded build suffix reported as pending: %+v", st)
	}

	// After the reboot the loaded module matches.
	write("smoothfs", "0.3.2")
	if st := GetRebootStatus(); st.Required {
		t.Fatalf("after reboot: %+v", st)
	}
}

func TestUpstreamVersion(t *testing.T) {
	for in, want := range map[string]string{
		"0.3.2-1": "0.3.2",
		"0.3.2+samba4.22.11+dfsg.0+deb13u1-1": "0.3.2",
		"2.3.4":  "2.3.4",
		"":       "",
	} {
		if got := upstreamVersion(in); got != want {
			t.Errorf("upstreamVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
