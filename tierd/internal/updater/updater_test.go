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
