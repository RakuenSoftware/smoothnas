package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, want := range []string{"APT::Update::Error-Mode=any", "dpkg --configure -a", "--fix-broken install", "full-upgrade", "--force-confold", startMarker + " install", endMarker, runLogPath} {
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
