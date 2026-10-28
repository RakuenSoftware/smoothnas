// Package updater reports what the NAS's updates still need.
//
// Updates are plain apt: unattended-upgrades installs Debian security updates
// and everything published to the Smooth* archive (SmoothKernel, SmoothNAS, smoothfs)
// as soon as it appears. Some updates only take effect on a reboot, and the
// operator decides when that happens; this package tells them one is needed.
package updater

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// RebootStatus describes a pending reboot for the UI.
type RebootStatus struct {
	Required bool     `json:"required"`
	Reasons  []string `json:"reasons"`
}

var (
	// debianRebootRequiredPath is created by Debian packages that need a reboot
	// (it lives on tmpfs, so a reboot clears it); the .pkgs file names them.
	debianRebootRequiredPath = "/run/reboot-required"

	// installedKernel and runningKernel return the kernel release SmoothKernel's
	// metapackage currently points at and the one the router booted; vars so
	// tests can fake them.
	installedKernel = smoothKernelRelease
	runningKernel   = unameRelease
)

// GetRebootStatus reports whether a reboot is needed for installed updates to
// take effect, and why.
func GetRebootStatus() RebootStatus {
	var st RebootStatus
	if installed, running := installedKernel(), runningKernel(); installed != "" && running != "" && installed != running {
		st.Reasons = append(st.Reasons, "SmoothKernel "+installed+" is installed; running "+running)
	}
	if _, err := os.Stat(debianRebootRequiredPath); err == nil {
		reason := "Debian updates"
		if pkgs := readLines(debianRebootRequiredPath + ".pkgs"); len(pkgs) > 0 {
			reason += ": " + strings.Join(pkgs, ", ")
		}
		st.Reasons = append(st.Reasons, reason)
	}
	st.Required = len(st.Reasons) > 0
	return st
}

var kernelDependency = regexp.MustCompile(`\blinux-image-(\S+-smoothkernel)\b`)

// smoothKernelRelease returns the kernel release linux-image-smoothkernel
// depends on ("7.2.10-74-smoothkernel"), or "" if it is not installed.
func smoothKernelRelease() string {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Depends}", "linux-image-smoothkernel").Output()
	if err != nil {
		return ""
	}
	return kernelReleaseFromDepends(string(out))
}

func kernelReleaseFromDepends(depends string) string {
	if m := kernelDependency.FindStringSubmatch(depends); m != nil {
		return m[1]
	}
	return ""
}

func unameRelease() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readLines returns the distinct non-empty lines of a file.
func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}
