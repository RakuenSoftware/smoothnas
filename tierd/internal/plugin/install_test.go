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

func TestInstaller_InstallWithOptions_TierBoundResolves(t *testing.T) {
	inst, _ := newTestInstaller(t)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir tier mount: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME", "SSD", "HDD")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	rec, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "media"},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(rec.Volumes) != 1 {
		t.Fatalf("volumes = %d", len(rec.Volumes))
	}
	v := rec.Volumes[0]
	if v.TierPool != "media" {
		t.Errorf("tier_pool = %q want media", v.TierPool)
	}
	wantPath := filepath.Join(tierMount, ".plugins", "llama-cpp", "models")
	if v.Paths[1] != wantPath {
		t.Errorf("path[1] = %q want %q", v.Paths[1], wantPath)
	}
	if st, err := os.Stat(wantPath); err != nil || !st.IsDir() {
		t.Errorf("expected tier-bound dir to be created: %v", err)
	}
}

func TestInstaller_InstallWithOptions_AppliesInitialConfig(t *testing.T) {
	inst, _ := newTestInstaller(t)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir tier mount: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME", "SSD", "HDD")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	rec, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers:  TierAssignments{Default: "media"},
		Config: map[string]string{"MODEL_PATH": "/models/custom.gguf", "UNKNOWN": "ignored"},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	got := ""
	for _, c := range rec.Config {
		if c.Key == "MODEL_PATH" {
			got = c.Value
		}
		if c.Key == "UNKNOWN" {
			t.Fatalf("unknown config key was persisted: %+v", rec.Config)
		}
	}
	if got != "/models/custom.gguf" {
		t.Fatalf("MODEL_PATH = %q, want install-time override", got)
	}
}

func TestInstaller_InstallWithOptions_PreflightFailureBlocksInstall(t *testing.T) {
	inst, _ := newTestInstaller(t)
	tp := newFakeTP() // no tiers registered
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	_, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "ghost"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PreflightError, got %T: %v", err, err)
	}
	// Plugin row must NOT exist — preflight runs before any DB
	// writes.
	if _, err := inst.store.Get("llama-cpp"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("plugin row should not exist after preflight failure: %v", err)
	}
}

func TestInstaller_InstallWithOptions_FlatVolumeUnchanged(t *testing.T) {
	// ubuntu-python.yaml has only flat volumes; tier-bound preflight
	// shouldn't touch the install path.
	inst, _ := newTestInstaller(t)
	tp := newFakeTP() // empty — flat volumes shouldn't ask
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	rec, err := inst.InstallWithOptions(readFixture(t, "ubuntu-python.yaml"), InstallOptions{})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Volumes[0].Mode != VolumeModeFlat {
		t.Errorf("expected flat volume; got %q", rec.Volumes[0].Mode)
	}
}

func TestInstaller_InstallWithOptions_PerVolumeOverride(t *testing.T) {
	inst, _ := newTestInstaller(t)
	fastMount := filepath.Join(t.TempDir(), "fast")
	defaultMount := filepath.Join(t.TempDir(), "default-pool")
	for _, m := range []string{fastMount, defaultMount} {
		if err := os.MkdirAll(m, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	tp := newFakeTP()
	tp.put("fast", fastMount, "healthy", "SSD")
	tp.put("default-pool", defaultMount, "healthy", "SSD")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	rec, err := inst.InstallWithOptions(readFixture(t, "gh-runner.yaml"), InstallOptions{
		Tiers: TierAssignments{
			Default:   "default-pool",
			PerVolume: map[string]string{"workspace": "fast"},
		},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Volumes[0].TierPool != "fast" {
		t.Errorf("per-volume override didn't win; tier_pool = %q", rec.Volumes[0].TierPool)
	}
	if len(rec.Volumes[0].Paths) != 2 {
		t.Fatalf("paths = %d want 2", len(rec.Volumes[0].Paths))
	}
	want1 := filepath.Join(fastMount, ".plugins", "gh-runner", "instance-1", "workspace")
	want2 := filepath.Join(fastMount, ".plugins", "gh-runner", "instance-2", "workspace")
	if rec.Volumes[0].Paths[1] != want1 || rec.Volumes[0].Paths[2] != want2 {
		t.Errorf("per-instance paths = %+v", rec.Volumes[0].Paths)
	}
}

func TestInstaller_Uninstall_RemovesTierBoundDirsAndParent(t *testing.T) {
	inst, _ := newTestInstaller(t)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	if _, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "media"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	volumeDir := filepath.Join(tierMount, ".plugins", "llama-cpp", "models")
	if _, err := os.Stat(volumeDir); err != nil {
		t.Fatalf("setup: volume dir should exist: %v", err)
	}

	if err := inst.Uninstall("llama-cpp"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(volumeDir); !os.IsNotExist(err) {
		t.Errorf("volume dir should be gone: %v", err)
	}
	pluginParent := filepath.Join(tierMount, ".plugins", "llama-cpp")
	if _, err := os.Stat(pluginParent); !os.IsNotExist(err) {
		t.Errorf("plugin parent dir should be gone: %v", err)
	}
}

func TestInstaller_Uninstall_RemovesTierBoundParentWhenPathRowMissing(t *testing.T) {
	inst, _ := newTestInstaller(t)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	if _, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "media"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	pluginParent := filepath.Join(tierMount, ".plugins", "llama-cpp")
	volumeDir := filepath.Join(pluginParent, "models")
	if err := os.WriteFile(filepath.Join(volumeDir, "model.gguf"), []byte("GGUF"), 0o640); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if _, err := inst.store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '' WHERE plugin_name = ? AND volume_name = ?`,
		"llama-cpp", "models",
	); err != nil {
		t.Fatalf("blank stored host path: %v", err)
	}

	if err := inst.Uninstall("llama-cpp"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(pluginParent); !os.IsNotExist(err) {
		t.Errorf("plugin parent dir should be gone: %v", err)
	}
	if _, err := inst.store.Get("llama-cpp"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("plugin should be deleted: %v", err)
	}
}

func TestInstaller_Uninstall_RemoveErrorPreservesPluginRecord(t *testing.T) {
	inst, _ := newTestInstaller(t)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	if _, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "media"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	volumeDir := filepath.Join(tierMount, ".plugins", "llama-cpp", "models")
	inst.removeAll = func(path string) error {
		if path == volumeDir {
			return errors.New("device busy")
		}
		return os.RemoveAll(path)
	}

	if err := inst.Uninstall("llama-cpp"); err == nil {
		t.Fatal("expected volume removal error")
	}
	if _, err := inst.store.Get("llama-cpp"); err != nil {
		t.Fatalf("plugin row should survive volume removal failure for retry: %v", err)
	}
	if _, err := os.Stat(volumeDir); err != nil {
		t.Fatalf("volume dir should remain after failed removal: %v", err)
	}

	inst.removeAll = os.RemoveAll
	if err := inst.Uninstall("llama-cpp"); err != nil {
		t.Fatalf("retry uninstall: %v", err)
	}
	if _, err := inst.store.Get("llama-cpp"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("plugin row should be deleted after retry: %v", err)
	}
}

func TestInstaller_BearerInjectedIssuesToken(t *testing.T) {
	// llama.yaml's manifest declares ui.embed.auth=bearer-injected,
	// so install should generate a token in plugin_secrets.
	inst, _ := newTestInstaller(t)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	if _, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "media"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	tok, err := inst.store.GetBearerToken("llama-cpp")
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("token = %q (length %d, want 64 hex chars)", tok, len(tok))
	}
	rec, err := inst.store.Get("llama-cpp")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	gotEnv := ""
	for _, c := range rec.Config {
		if c.Key == bearerExpectedConfigKey {
			gotEnv = c.Value
			break
		}
	}
	if gotEnv != tok {
		t.Errorf("%s = %q, want issued bearer token", bearerExpectedConfigKey, gotEnv)
	}
}

func TestInstaller_NoEmbedAuthSkipsToken(t *testing.T) {
	// ubuntu-python has no ui block; no token should be issued.
	inst, _ := newTestInstaller(t)
	if _, err := inst.Install(readFixture(t, "ubuntu-python.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	tok, err := inst.store.GetBearerToken("ubuntu-python")
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if tok != "" {
		t.Errorf("expected empty token for plugin without embed auth; got %q", tok)
	}
}

func TestStore_IssueBearerTokenIsIdempotentAndRotates(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsForSingleInstance(m, "/tmp"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t1, err := s.IssueBearerToken("llama-cpp")
	if err != nil {
		t.Fatalf("issue 1: %v", err)
	}
	t2, err := s.IssueBearerToken("llama-cpp")
	if err != nil {
		t.Fatalf("issue 2: %v", err)
	}
	if t1 == t2 {
		t.Errorf("rotate should produce a different token; got identical")
	}
	got, _ := s.GetBearerToken("llama-cpp")
	if got != t2 {
		t.Errorf("Get returned %q, want last-issued %q", got, t2)
	}
	rec, err := s.Get("llama-cpp")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	gotEnv := ""
	for _, c := range rec.Config {
		if c.Key == bearerExpectedConfigKey {
			gotEnv = c.Value
			break
		}
	}
	if gotEnv != t2 {
		t.Errorf("%s = %q, want rotated token", bearerExpectedConfigKey, gotEnv)
	}
	if err := s.ReplaceConfig("llama-cpp", map[string]string{
		"MODEL_PATH":            "/models/qwen3.6-27b-128k-q5.gguf",
		bearerExpectedConfigKey: "",
	}); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	rec, err = s.Get("llama-cpp")
	if err != nil {
		t.Fatalf("get plugin after replace: %v", err)
	}
	gotEnv = ""
	for _, c := range rec.Config {
		if c.Key == bearerExpectedConfigKey {
			gotEnv = c.Value
			break
		}
	}
	if gotEnv != t2 {
		t.Errorf("ReplaceConfig should preserve generated bearer token, got %q want %q", gotEnv, t2)
	}
}

func TestStore_BearerTokenCascadesOnPluginDelete(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsForSingleInstance(m, "/tmp"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.IssueBearerToken("llama-cpp"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := s.Delete("llama-cpp"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	tok, err := s.GetBearerToken("llama-cpp")
	if err != nil {
		t.Fatalf("get post-delete: %v", err)
	}
	if tok != "" {
		t.Errorf("token should cascade-delete; got %q", tok)
	}
}

func TestStore_TierConsumers(t *testing.T) {
	s := openTestStore(t)

	// Install one plugin onto "media" via the resolved-tier path.
	inst := NewInstaller(s)
	inst.SetPluginsRoot(t.TempDir())
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tp := newFakeTP()
	tp.put("media", tierMount, "healthy", "NVME")
	inst.SetTierProvider(tp, fakeStatfs{}.avail)

	if _, err := inst.InstallWithOptions(readFixture(t, "llama.yaml"), InstallOptions{
		Tiers: TierAssignments{Default: "media"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	consumers, err := s.TierConsumers("media")
	if err != nil {
		t.Fatalf("consumers: %v", err)
	}
	if len(consumers) != 1 || consumers[0] != "llama-cpp" {
		t.Errorf("consumers = %v want [llama-cpp]", consumers)
	}
	// Different tier name → no consumers.
	other, _ := s.TierConsumers("nope")
	if len(other) != 0 {
		t.Errorf("nope consumers should be empty, got %v", other)
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
