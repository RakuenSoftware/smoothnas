package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseUpgradable(t *testing.T) {
	out := `Listing... Done
smoothfs-dkms/smoothnas 0.3.2-1 all [upgradable from: 0.3.1-1]
smoothfs-samba-vfs/smoothnas 0.3.2+samba4.22.11+dfsg.0+deb13u1-1 amd64 [upgradable from: 0.3.1+samba4.22.11+dfsg.0+deb13u1-1]
`
	got := parseUpgradable(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 packages, got %+v", got)
	}
	if got[0] != (Package{Name: "smoothfs-dkms", Installed: "0.3.1-1", Available: "0.3.2-1"}) {
		t.Errorf("unexpected first package %+v", got[0])
	}
	if got[1].Name != "smoothfs-samba-vfs" || !strings.HasPrefix(got[1].Available, "0.3.2+") {
		t.Errorf("unexpected second package %+v", got[1])
	}
	if pkgs := parseUpgradable("Listing... Done\n"); len(pkgs) != 0 {
		t.Errorf("expected no packages, got %+v", pkgs)
	}
}

func TestParseRunLogFailedFetch(t *testing.T) {
	log := `== smoothnas-update start check 2026-10-29T18:10:00Z
Ign:5 https://updates.rakuensoftware.com/public common InRelease
Err:5 https://updates.rakuensoftware.com/public common InRelease
  Temporary failure resolving 'updates.rakuensoftware.com'
W: Failed to fetch https://updates.rakuensoftware.com/public/dists/common/InRelease  Temporary failure resolving 'updates.rakuensoftware.com'
E: Some index files failed to download. They have been ignored, or old ones used instead.
== smoothnas-update exit 100 2026-10-29T18:10:31Z
`
	run := parseRunLog(strings.NewReader(log))
	if run == nil {
		t.Fatal("expected a run")
	}
	if run.Action != "check" || run.StartedAt != "2026-10-29T18:10:00Z" || run.FinishedAt != "2026-10-29T18:10:31Z" {
		t.Errorf("unexpected run header %+v", run)
	}
	if run.ExitCode == nil || *run.ExitCode != 100 {
		t.Errorf("expected exit 100, got %v", run.ExitCode)
	}
	if len(run.Errors) != 3 {
		t.Fatalf("expected 3 errors, got %q", run.Errors)
	}
	if !strings.Contains(run.Errors[0], "Temporary failure resolving") {
		t.Errorf("Err: line should carry its reason, got %q", run.Errors[0])
	}
}

func TestParseRunLogInProgress(t *testing.T) {
	run := parseRunLog(strings.NewReader("== smoothnas-update start install 2026-10-29T18:10:00Z\nReading package lists...\n"))
	if run == nil || run.Action != "install" || run.ExitCode != nil || run.FinishedAt != "" {
		t.Fatalf("expected an unfinished install run, got %+v", run)
	}
}

func TestStart(t *testing.T) {
	dir := t.TempDir()
	runLogPath = filepath.Join(dir, "log", "update.log")
	active := false
	var script string
	unitActive = func() bool { return active }
	startUnit = func(s string) error { script = s; return nil }

	if err := Start("bogus"); err == nil {
		t.Error("expected unknown action to fail")
	}
	if err := os.MkdirAll(filepath.Dir(runLogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLogPath, []byte("old run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Start(ActionInstall); err != nil {
		t.Fatalf("start install: %v", err)
	}
	if data, _ := os.ReadFile(runLogPath); len(data) != 0 {
		t.Errorf("expected the log truncated for a new run, got %q", data)
	}
	for _, want := range []string{"APT::Update::Error-Mode=any", refreshMarker, "dpkg --configure -a", "--fix-broken install", "full-upgrade", "--force-confold", startMarker + " install", endMarker, runLogPath} {
		if !strings.Contains(script, want) {
			t.Errorf("install script missing %q: %s", want, script)
		}
	}

	if err := Start(ActionCheck); err != nil {
		t.Fatalf("start check: %v", err)
	}
	if strings.Contains(script, "full-upgrade") || strings.Contains(script, "--fix-broken") {
		t.Errorf("check must not change packages: %s", script)
	}

	active = true
	if err := Start(ActionCheck); err != ErrRunning {
		t.Errorf("expected ErrRunning while a run is active, got %v", err)
	}
}

func TestReadLog(t *testing.T) {
	runLogPath = filepath.Join(t.TempDir(), "update.log")
	if text, next, err := ReadLog(0); err != nil || text != "" || next != 0 {
		t.Fatalf("missing log: %q %d %v", text, next, err)
	}
	if err := os.WriteFile(runLogPath, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, next, err := ReadLog(2)
	if err != nil || text != "cdef" || next != 6 {
		t.Fatalf("ReadLog(2) = %q %d %v", text, next, err)
	}
	// A new run truncated the log: an offset past the end starts over.
	text, next, _ = ReadLog(100)
	if text != "abcdef" || next != 6 {
		t.Fatalf("ReadLog past end = %q %d", text, next)
	}
}

// The WTR Max's Updates page on 2026-11-02: an install started from the page
// on Nov 1 failed because apt.bcachefs.org refused connections, and the
// page still said "Installing updates failed" a day later, after unattended
// upgrades had installed kernel 7.2.15-84 and OpenZFS without an error.
const wtrMaxFailedInstall = `== smoothnas-update start install 2026-11-01T15:36:21Z
Err:4 https://apt.bcachefs.org/trixie bcachefs-tools-release InRelease
  Could not connect to apt.bcachefs.org:443 (5.161.236.196). - connect (111: Connection refused)
E: Failed to fetch https://apt.bcachefs.org/trixie/dists/bcachefs-tools-release/InRelease  Cannot initiate the connection to apt.bcachefs.org:443 (2a01:4ff:f0:49f3::1). - connect (101: Network is unreachable)
E: Some index files failed to download. They have been ignored, or old ones used instead.
== smoothnas-update exit 100 2026-11-01T15:36:28Z
`

func writeStamp(t *testing.T, at time.Time) {
	t.Helper()
	upgradeStampPath = filepath.Join(t.TempDir(), "upgrade-stamp")
	if err := os.WriteFile(upgradeStampPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(upgradeStampPath, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestMarkSupersededAfterUnattendedSuccess(t *testing.T) {
	writeStamp(t, time.Date(2026, 11, 2, 13, 38, 27, 0, time.UTC))
	run := parseRunLog(strings.NewReader(wtrMaxFailedInstall))
	markSuperseded(run)
	if run.SupersededAt != "2026-11-02T13:38:27Z" {
		t.Fatalf("failed install older than a successful unattended upgrade: SupersededAt = %q", run.SupersededAt)
	}
	if run.ExitCode == nil || *run.ExitCode != 100 || len(run.Errors) == 0 {
		t.Fatalf("superseding must keep the failure's record: %+v", run)
	}
}

func TestMarkSupersededLeavesCurrentFailures(t *testing.T) {
	failedAt := time.Date(2026, 11, 1, 15, 36, 28, 0, time.UTC)
	cases := []struct {
		name  string
		log   string
		stamp *time.Time
	}{
		{"unattended last succeeded before the failure", wtrMaxFailedInstall, ptr(failedAt.Add(-time.Hour))},
		{"unattended succeeded in the same second", wtrMaxFailedInstall, ptr(failedAt)},
		{"no unattended success recorded", wtrMaxFailedInstall, nil},
		{"failed check", strings.Replace(wtrMaxFailedInstall, "start install", "start check", 1), ptr(failedAt.Add(time.Hour))},
		{"successful install", strings.Replace(wtrMaxFailedInstall, "exit 100", "exit 0", 1), ptr(failedAt.Add(time.Hour))},
		{"unfinished install", "== smoothnas-update start install 2026-11-01T15:36:21Z\n", ptr(failedAt.Add(time.Hour))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.stamp != nil {
				writeStamp(t, *c.stamp)
			} else {
				upgradeStampPath = filepath.Join(t.TempDir(), "absent")
			}
			run := parseRunLog(strings.NewReader(c.log))
			markSuperseded(run)
			if run.SupersededAt != "" {
				t.Fatalf("SupersededAt = %q, want empty", run.SupersededAt)
			}
		})
	}
	markSuperseded(nil) // no run yet: must not panic
}

func ptr(t time.Time) *time.Time { return &t }

func TestGetUpdateStatusReportsSuperseded(t *testing.T) {
	runLogPath = filepath.Join(t.TempDir(), "update.log")
	if err := os.WriteFile(runLogPath, []byte(wtrMaxFailedInstall), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStamp(t, time.Date(2026, 11, 2, 13, 38, 27, 0, time.UTC))
	unitActive = func() bool { return false }
	aptListUpgradable = func() ([]byte, error) { return []byte("Listing... Done\n"), nil }
	st := GetUpdateStatus()
	if st.LastRun == nil || st.LastRun.SupersededAt == "" {
		t.Fatalf("status must report the failure as superseded: %+v", st.LastRun)
	}
}

// Run the install script Start builds, with stub apt-get and dpkg, and read
// its log back the way the Updates page does.
func runInstallScript(t *testing.T, updateExit, upgradeExit int) (*LastRun, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := fmt.Sprintf(`#!/bin/sh
echo "$(basename "$0") $*" >> %[1]q
case "$*" in
  *" update") [ %[2]d -eq 0 ] || echo "E: Some index files failed to download."; exit %[2]d ;;
  *full-upgrade*) exit %[3]d ;;
esac
exit 0
`, calls, updateExit, upgradeExit)
	for _, name := range []string{"apt-get", "dpkg"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runLogPath = filepath.Join(dir, "update.log")
	unitActive = func() bool { return false }
	var script string
	startUnit = func(s string) error { script = s; return nil }
	if err := Start(ActionInstall); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	f, err := os.Open(runLogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := os.ReadFile(calls)
	return parseRunLog(f), string(got)
}

// A source that can't be refreshed must not stop the install.
func TestInstallRunsWhenRefreshFails(t *testing.T) {
	run, calls := runInstallScript(t, 100, 0)
	if !strings.Contains(calls, "full-upgrade") {
		t.Fatalf("install did not run after the refresh failed; calls:\n%s", calls)
	}
	if run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("install succeeded, exit = %v", run.ExitCode)
	}
	if !run.RefreshFailed {
		t.Fatal("refresh failure not recorded")
	}
	if len(run.Errors) == 0 {
		t.Fatal("refresh errors not reported")
	}
}

func TestInstallFailureStillReported(t *testing.T) {
	run, _ := runInstallScript(t, 0, 100)
	if run.ExitCode == nil || *run.ExitCode != 100 {
		t.Fatalf("failed install must report its exit code, got %v", run.ExitCode)
	}
	if run.RefreshFailed {
		t.Fatal("refresh succeeded but was reported as failed")
	}
}

func TestInstallCleanRun(t *testing.T) {
	run, calls := runInstallScript(t, 0, 0)
	if run.ExitCode == nil || *run.ExitCode != 0 || run.RefreshFailed || len(run.Errors) != 0 {
		t.Fatalf("clean run reported as %+v", run)
	}
	for _, want := range []string{"apt-get -o APT::Update::Error-Mode=any", "dpkg --configure -a", "--fix-broken install", "full-upgrade"} {
		if !strings.Contains(calls, want) {
			t.Errorf("install did not run %q; calls:\n%s", want, calls)
		}
	}
}
