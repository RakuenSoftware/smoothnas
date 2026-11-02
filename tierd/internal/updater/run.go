package updater

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Operator-started update runs. unattended-upgrades keeps the NAS current on
// its own; these let the operator check for and install updates now, and see
// what happened, including failures (no DNS, archive unreachable) that the
// unattended runs only record in their own log.
//
// A run executes in its own transient systemd unit, not as a child of tierd:
// upgrading the smoothnas package restarts tierd, and the run must survive
// that. Its output goes to runLogPath, which the UI reads back while it runs.

const runUnit = "smoothnas-update"

var (
	runLogPath = "/var/log/smoothnas/update.log"

	// apt.systemd.daily touches this only when unattended-upgrade exits 0, so
	// its time is when updates last installed without an error.
	upgradeStampPath = "/var/lib/apt/periodic/upgrade-stamp"

	// systemctl and systemd-run are vars so tests can fake them.
	unitActive = func() bool {
		return exec.Command("systemctl", "is-active", "--quiet", runUnit).Run() == nil
	}
	startUnit = func(script string) error {
		out, err := exec.Command("systemd-run",
			"--unit="+runUnit, "--collect",
			"--description=SmoothNAS update",
			"--setenv=DEBIAN_FRONTEND=noninteractive",
			"/bin/sh", "-c", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	aptListUpgradable = func() ([]byte, error) {
		return exec.Command("apt", "list", "--upgradable").Output()
	}
)

// aptUpdate fails on any index it cannot fetch. By default apt-get update
// warns and exits 0, which hid an unreachable archive.
const aptUpdate = "apt-get -o APT::Update::Error-Mode=any -o DPkg::Lock::Timeout=600 update"

const aptUpgrade = "apt-get -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef " +
	"-o Dpkg::Options::=--force-confold -y full-upgrade"

// aptRepair finishes an interrupted dpkg run and resolves broken
// dependencies before installing: after a power cut or a killed upgrade,
// full-upgrade otherwise refuses with "Unmet dependencies" until someone
// runs these by hand. dpkg --configure -a is allowed to fail (the lock may
// be held by an unattended run); apt-get -f then waits for the lock.
const aptRepair = "{ dpkg --configure -a || true; } && " +
	"apt-get -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef " +
	"-o Dpkg::Options::=--force-confold -y --fix-broken install"

// installCmd refreshes the package lists, then installs whatever they offer.
// A source that can't be refreshed (a third-party archive that is down) must
// not stop SmoothNAS's own updates from installing: the refresh result is
// recorded on its own line and reported, and the install still runs, from
// the lists as of the last successful refresh for that source.
const installCmd = aptUpdate + "; refresh=$?; echo \"" + refreshMarker + " $refresh\"; " +
	aptRepair + " && " + aptUpgrade

// Run actions.
const (
	ActionCheck   = "check"
	ActionInstall = "install"
)

// ErrRunning is returned when a run is already in progress.
var ErrRunning = fmt.Errorf("an update run is already in progress")

// Start begins a check (refresh the package lists) or an install (refresh,
// then install every available update) in the background.
func Start(action string) error {
	// The script is built only from constants: action selects one, and
	// its value never reaches the shell.
	var script string
	switch action {
	case ActionCheck:
		script = runScript(ActionCheck, aptUpdate, runLogPath)
	case ActionInstall:
		script = runScript(ActionInstall, installCmd, runLogPath)
	default:
		return fmt.Errorf("unknown update action %q", action)
	}
	if unitActive() {
		return ErrRunning
	}
	if err := os.MkdirAll(dirOf(runLogPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(runLogPath, nil, 0o644); err != nil {
		return err
	}
	return startUnit(script)
}

func runScript(action, cmd, logPath string) string {
	return fmt.Sprintf(`{ echo "%s %s $(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"; %s; rc=$?; echo "%s $rc $(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"; } >>%s 2>&1`,
		startMarker, action, cmd, endMarker, logPath)
}

const (
	startMarker   = "== smoothnas-update start"
	endMarker     = "== smoothnas-update exit"
	refreshMarker = "== smoothnas-update refresh-exit"
)

// Package is an update available for an installed package.
type Package struct {
	Name      string `json:"name"`
	Installed string `json:"installed"`
	Available string `json:"available"`
}

// LastRun summarises the most recent operator-started run.
type LastRun struct {
	Action     string   `json:"action"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Errors     []string `json:"errors"`
	// RefreshFailed: an install whose package-list refresh failed for some
	// source; the install itself still ran, and ExitCode is its result.
	RefreshFailed bool `json:"refresh_failed,omitempty"`
	// SupersededAt: this failed install is older than a later unattended
	// run that installed updates without an error, at this time. The
	// failure is history, not the NAS's current state.
	SupersededAt string `json:"superseded_at,omitempty"`
}

// UpdateStatus is what the Updates page shows.
type UpdateStatus struct {
	Running   bool         `json:"running"`
	Available []Package    `json:"available"`
	LastRun   *LastRun     `json:"last_run"`
	Reboot    RebootStatus `json:"reboot"`
}

// GetUpdateStatus reports available updates (from the package lists as of
// the last check), the last run's outcome, and whether a reboot is pending.
func GetUpdateStatus() UpdateStatus {
	st := UpdateStatus{Running: unitActive(), Reboot: GetRebootStatus(), Available: []Package{}}
	if out, err := aptListUpgradable(); err == nil {
		st.Available = parseUpgradable(string(out))
	}
	if f, err := os.Open(runLogPath); err == nil {
		st.LastRun = parseRunLog(f)
		f.Close()
	}
	markSuperseded(st.LastRun)
	return st
}

// markSuperseded sets SupersededAt on a failed install that unattended
// upgrades have since succeeded past. A failed check is left alone: the
// unattended refresh runs in apt's default mode, which exits 0 even when an
// archive is unreachable, so it can't show that a check would now pass.
func markSuperseded(run *LastRun) {
	if run == nil || run.Action != ActionInstall || run.ExitCode == nil || *run.ExitCode == 0 {
		return
	}
	failed, err := time.Parse(time.RFC3339, run.FinishedAt)
	if err != nil {
		return
	}
	info, err := os.Stat(upgradeStampPath)
	if err != nil || !info.ModTime().After(failed) {
		return
	}
	run.SupersededAt = info.ModTime().UTC().Format(time.RFC3339)
}

// ReadLog returns the run log from byte offset on, and the offset to read
// from next. A run truncates the log, so an offset past the end restarts at 0.
func ReadLog(offset int64) (string, int64, error) {
	f, err := os.Open(runLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return "", 0, err
	}
	return string(data), offset + int64(len(data)), nil
}

// upgradableLine matches `apt list --upgradable` entries:
// "smoothfs-dkms/smoothnas 0.3.2-1 all [upgradable from: 0.3.1-1]".
var upgradableLine = regexp.MustCompile(`^([^/\s]+)/\S+\s+(\S+)\s+\S+\s+\[upgradable from: ([^\]]+)\]`)

func parseUpgradable(out string) []Package {
	pkgs := []Package{}
	for _, line := range strings.Split(out, "\n") {
		if m := upgradableLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			pkgs = append(pkgs, Package{Name: m[1], Available: m[2], Installed: m[3]})
		}
	}
	return pkgs
}

// parseRunLog reads a run log written by runScript. Errors are apt's "E:"
// and "Err:" lines and the failed-fetch warnings, which name what went wrong.
func parseRunLog(r io.Reader) *LastRun {
	var run *LastRun
	lastWasErr := false // apt prints an Err: line's reason on the next, indented line
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if lastWasErr && strings.HasPrefix(line, "  ") && len(run.Errors) > 0 {
			run.Errors[len(run.Errors)-1] += ": " + strings.TrimSpace(line)
			lastWasErr = false
			continue
		}
		lastWasErr = false
		switch {
		case strings.HasPrefix(line, startMarker+" "):
			f := strings.Fields(strings.TrimPrefix(line, startMarker+" "))
			run = &LastRun{Errors: []string{}}
			if len(f) > 0 {
				run.Action = f[0]
			}
			if len(f) > 1 {
				run.StartedAt = f[1]
			}
		case run != nil && strings.HasPrefix(line, refreshMarker+" "):
			if code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, refreshMarker+" "))); err == nil {
				run.RefreshFailed = code != 0
			}
		case run != nil && strings.HasPrefix(line, endMarker+" "):
			f := strings.Fields(strings.TrimPrefix(line, endMarker+" "))
			if len(f) > 0 {
				if code, err := strconv.Atoi(f[0]); err == nil {
					run.ExitCode = &code
				}
			}
			if len(f) > 1 {
				run.FinishedAt = f[1]
			}
		case run != nil && (strings.HasPrefix(line, "E: ") || strings.HasPrefix(line, "W: Failed to fetch") ||
			strings.HasPrefix(line, "Err:")):
			if len(run.Errors) < 20 {
				run.Errors = append(run.Errors, strings.TrimSpace(line))
				lastWasErr = strings.HasPrefix(line, "Err:")
			}
		}
	}
	return run
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}
