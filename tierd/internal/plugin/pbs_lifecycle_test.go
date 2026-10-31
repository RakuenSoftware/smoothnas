package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
)

// Exercise the shipped PBS project through install/materialise/start/uninstall
// and reinstall. The runner is fake; the storage and database are real. In
// particular, compose pins are distinct from native plugin-owned volume rows.
func TestPBSUninstallRetainsAndReattachesData(t *testing.T) {
	project, err := os.ReadFile("../api/localplugins/pbs/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t)
	tier := t.TempDir()
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	tp.put("backup", tier, "healthy")
	installer := NewInstaller(store)
	installer.SetPluginsRoot(t.TempDir())
	runner := &recRunner{}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetTierProvider(tp)
	lc.SetComposeBackend(compose.NewBackend(compose.New("", runner), t.TempDir()))
	installer.SetDemolisher(lc)
	opts := InstallOptions{Tiers: TierAssignments{Default: "backup"}, Config: map[string]string{"PBS_ADMIN_PASSWORD": "initial-test-password"}}
	ctx := context.Background()
	install := func() map[string]ComposeVolumePin {
		t.Helper()
		if _, err := installer.InstallWithOptions(project, opts); err != nil {
			t.Fatal(err)
		}
		if err := lc.Materialise(ctx, "pbs"); err != nil {
			t.Fatal(err)
		}
		if err := lc.Start(ctx, "pbs"); err != nil {
			t.Fatal(err)
		}
		pins, err := store.GetComposeVolumePins("pbs")
		if err != nil {
			t.Fatal(err)
		}
		if len(pins) != 4 {
			t.Fatalf("PBS volume pins = %v", pins)
		}
		return pins
	}
	first := install()
	for name, pin := range first {
		if err := os.WriteFile(filepath.Join(pin.HostPath, "retained"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := installer.Uninstall("pbs"); err != nil {
		t.Fatal(err)
	}
	for _, pin := range first {
		if _, err := os.Stat(filepath.Join(pin.HostPath, "retained")); err != nil {
			t.Fatalf("uninstall removed backup data: %v", err)
		}
	}
	second := install()
	for name, pin := range second {
		if pin.HostPath != first[name].HostPath {
			t.Fatal("reinstall changed retained path")
		}
		b, err := os.ReadFile(filepath.Join(pin.HostPath, "retained"))
		if err != nil || string(b) != name {
			t.Fatalf("reinstall lost %s: %v", name, err)
		}
	}
}
