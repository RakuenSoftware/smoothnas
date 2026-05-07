package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// readFixture returns the raw bytes of a testdata YAML file.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func newTestInstaller(t *testing.T) (*Installer, string) {
	t.Helper()
	store := openTestStore(t)
	root := t.TempDir()
	inst := NewInstaller(store)
	inst.SetPluginsRoot(root)
	return inst, root
}

func TestInstaller_Install_OCIImage_RoundTrip(t *testing.T) {
	inst, root := newTestInstaller(t)
	rec, err := inst.Install(readFixture(t, "llama.yaml"))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Plugin.Name != "llama-cpp" || rec.Plugin.State != StateInstalled {
		t.Errorf("plugin row wrong: %+v", rec.Plugin)
	}
	// Tier-bound volume: no flat directory should have been created.
	tieredDir := filepath.Join(root, "llama-cpp", "models")
	if _, err := os.Stat(tieredDir); !os.IsNotExist(err) {
		t.Errorf("tier-bound volume should not create flat dir: %v", err)
	}
	// Manifest YAML stored verbatim.
	if rec.Plugin.ManifestYAML == "" {
		t.Error("manifest YAML not stored")
	}
	if got, want := rec.Plugin.ManifestYAML[:9], "apiVersio"; got != want {
		t.Errorf("manifest stored wrong: starts with %q", got)
	}
}

func TestInstaller_Install_LXCDistro_FlatVolumeMkdir(t *testing.T) {
	inst, root := newTestInstaller(t)
	if _, err := inst.Install(readFixture(t, "ubuntu-python.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	// flat-mode "data" volume → directory under root must exist.
	want := filepath.Join(root, "ubuntu-python", "data")
	st, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.IsDir() {
		t.Errorf("%s is not a directory", want)
	}
	if mode := st.Mode().Perm(); mode != 0o750 {
		t.Errorf("mode = %o want 0750", mode)
	}
}

func TestInstaller_Install_MultiInstance_PerInstanceFlatDirs(t *testing.T) {
	// gh-runner has a tier-bound volume, not flat — so it doesn't
	// exercise per-instance flat mkdir. Build a synthetic manifest
	// instead: take llama.yaml and rewrite the volume to flat +
	// perInstance, with instances.count: 3.
	inst, root := newTestInstaller(t)
	yaml := []byte(`apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: multi-flat
  version: 0.0.1
artifact:
  type: oci-image
  image: example.com/test:1
container:
  command: ["sleep","infinity"]
  restartPolicy: unless-stopped
instances:
  count: 3
  configurable: true
volumes:
  - name: scratch
    mode: flat
    bind: /scratch
    perInstance: true
`)
	rec, err := inst.Install(yaml)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(rec.Volumes) != 1 || len(rec.Volumes[0].Paths) != 3 {
		t.Fatalf("expected 1 volume × 3 paths, got %+v", rec.Volumes)
	}
	for inst := 1; inst <= 3; inst++ {
		want := filepath.Join(root, "multi-flat", "instance-"+itoa(inst), "scratch")
		if got := rec.Volumes[0].Paths[inst]; got != want {
			t.Errorf("path[%d] = %q want %q", inst, got, want)
		}
		if st, err := os.Stat(want); err != nil || !st.IsDir() {
			t.Errorf("instance %d dir not created: %v", inst, err)
		}
	}
}

func TestInstaller_Install_ValidationErrorBubblesUp(t *testing.T) {
	inst, _ := newTestInstaller(t)
	yaml := []byte(`apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: BAD-NAME
  version: not-semver
artifact:
  type: oci-image
  image: example.com/test:1
container:
  restartPolicy: unless-stopped
`)
	_, err := inst.Install(yaml)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
}

func TestInstaller_Uninstall_RemovesFlatDirsAndDBRows(t *testing.T) {
	inst, root := newTestInstaller(t)
	if _, err := inst.Install(readFixture(t, "ubuntu-python.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := filepath.Join(root, "ubuntu-python", "data")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("setup: data dir should exist: %v", err)
	}

	if err := inst.Uninstall("ubuntu-python"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("data dir should be gone: %v", err)
	}
	if _, err := inst.store.Get("ubuntu-python"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("plugin should be deleted: %v", err)
	}
}

func TestInstaller_Uninstall_MissingReturnsErrPluginNotFound(t *testing.T) {
	inst, _ := newTestInstaller(t)
	if err := inst.Uninstall("nope"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("err = %v, want ErrPluginNotFound", err)
	}
}

// fakeDemolisher records its calls and optionally fails on demand.
// Used to verify Installer.Uninstall calls the runtime teardown path
// when one is attached.
type fakeDemolisher struct {
	calls []string
	err   error
}

func (f *fakeDemolisher) Demolish(_ context.Context, name string) error {
	f.calls = append(f.calls, name)
	return f.err
}

func TestInstaller_Uninstall_CallsDemolisherBeforeDBDelete(t *testing.T) {
	inst, _ := newTestInstaller(t)
	if _, err := inst.Install(readFixture(t, "ubuntu-python.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	d := &fakeDemolisher{}
	inst.SetDemolisher(d)
	if err := inst.Uninstall("ubuntu-python"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(d.calls) != 1 || d.calls[0] != "ubuntu-python" {
		t.Errorf("Demolish calls = %v want [ubuntu-python]", d.calls)
	}
	if _, err := inst.store.Get("ubuntu-python"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("plugin should be deleted: %v", err)
	}
}

func TestInstaller_Uninstall_DemolisherErrorPreventsDBDelete(t *testing.T) {
	inst, _ := newTestInstaller(t)
	if _, err := inst.Install(readFixture(t, "ubuntu-python.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	d := &fakeDemolisher{err: errors.New("daemon unreachable")}
	inst.SetDemolisher(d)
	if err := inst.Uninstall("ubuntu-python"); err == nil {
		t.Fatal("expected error")
	}
	// DB row must still exist — operator can retry after daemon is back.
	if _, err := inst.store.Get("ubuntu-python"); err != nil {
		t.Errorf("plugin row should survive demolisher failure: %v", err)
	}
}

func TestInstaller_Uninstall_NoDemolisherIsPhaseOneBehaviour(t *testing.T) {
	// Re-asserts the original behaviour now that the code path is
	// gated on the optional Demolisher field.
	inst, _ := newTestInstaller(t)
	if _, err := inst.Install(readFixture(t, "ubuntu-python.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	// No SetDemolisher call.
	if err := inst.Uninstall("ubuntu-python"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := inst.store.Get("ubuntu-python"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("plugin should be deleted: %v", err)
	}
}

// tiny local itoa to avoid importing strconv just for the test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
