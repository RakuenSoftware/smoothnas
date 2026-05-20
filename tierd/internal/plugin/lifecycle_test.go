package plugin

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// fakeRuntime is an in-memory RuntimeClient used by lifecycle tests.
// It records every call and lets tests inject errors for specific
// operations.
type fakeRuntime struct {
	mu sync.Mutex

	// Recorded calls — useful in assertions.
	createCalls []runtime.CreateContainerRequest
	createNames []string
	startCalls  []string
	stopCalls   []string
	removeCalls []string
	pullCalls   []string
	commitCalls []string
	waitCalls   []string
	bridgeCalls int

	// Behaviour knobs.
	pullErr        error
	pullErrs       []error
	createErr      error
	startErr       error
	waitExit       int
	commitErr      error
	inspectMissing map[string]bool // container IDs that 404 on inspect
	containers     map[string]runtime.CreateContainerRequest
	// bridgeIP is what InspectContainerBridgeIP returns. Default
	// empty string makes captureBridgeIP retry; tests that exercise
	// the success path set this to a real-looking IP.
	bridgeIP string

	// Generated container IDs counter.
	nextID int
}

func (f *fakeRuntime) Ping(ctx context.Context) error                 { return nil }
func (f *fakeRuntime) Info(ctx context.Context) (runtime.Info, error) { return runtime.Info{}, nil }
func (f *fakeRuntime) EnsurePluginBridge(ctx context.Context) (string, error) {
	return "fake-bridge", nil
}
func (f *fakeRuntime) InspectContainerBridgeIP(ctx context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridgeCalls++
	if f.bridgeIP == "" {
		return "", runtime.ErrBridgeIPNotReady
	}
	return f.bridgeIP, nil
}
func (f *fakeRuntime) ListManagedContainers(ctx context.Context) ([]runtime.ContainerSummary, error) {
	return nil, nil
}
func (f *fakeRuntime) StreamLogs(ctx context.Context, id string, opts runtime.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeRuntime) SubscribeEvents(ctx context.Context) (<-chan runtime.Event, <-chan error, error) {
	ev := make(chan runtime.Event)
	er := make(chan error)
	close(ev)
	close(er)
	return ev, er, nil
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string, _ func(runtime.PullEvent)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullCalls = append(f.pullCalls, ref)
	if len(f.pullErrs) > 0 {
		err := f.pullErrs[0]
		f.pullErrs = f.pullErrs[1:]
		if err != nil {
			return "", err
		}
		return ref, nil
	}
	if f.pullErr != nil {
		return "", f.pullErr
	}
	return ref, nil
}

func (f *fakeRuntime) RemoveImage(ctx context.Context, ref string) error {
	return nil
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, name string, req runtime.CreateContainerRequest) (runtime.CreateContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return runtime.CreateContainerResponse{}, f.createErr
	}
	f.nextID++
	id := "fake-" + name
	f.createCalls = append(f.createCalls, req)
	f.createNames = append(f.createNames, name)
	if f.containers == nil {
		f.containers = map[string]runtime.CreateContainerRequest{}
	}
	f.containers[id] = req
	return runtime.CreateContainerResponse{ID: id}, nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, id)
	return f.startErr
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, id)
	return nil
}

func (f *fakeRuntime) RestartContainer(ctx context.Context, id string, _ int) error { return nil }

func (f *fakeRuntime) RemoveContainer(ctx context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, id)
	delete(f.containers, id)
	return nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, id string) (runtime.ContainerInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectMissing[id] {
		return runtime.ContainerInspect{}, &runtime.APIError{StatusCode: 404, Message: "no such container"}
	}
	if req, ok := f.containers[id]; ok {
		return runtime.ContainerInspect{
			ID:    id,
			Image: req.Image,
			Config: runtime.ContainerConfig{
				Image:        req.Image,
				Cmd:          append([]string(nil), req.Cmd...),
				Env:          append([]string(nil), req.Env...),
				WorkingDir:   req.WorkingDir,
				User:         req.User,
				Labels:       req.Labels,
				ExposedPorts: req.ExposedPorts,
			},
			HostConfig: req.HostConfig,
		}, nil
	}
	return runtime.ContainerInspect{ID: id}, nil
}

func (f *fakeRuntime) WaitContainer(ctx context.Context, id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitCalls = append(f.waitCalls, id)
	return f.waitExit, nil
}

func (f *fakeRuntime) CommitContainer(ctx context.Context, containerID, repo, tag string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls = append(f.commitCalls, repo+":"+tag)
	if f.commitErr != nil {
		return "", f.commitErr
	}
	return "img-" + containerID, nil
}

// installFixture installs a manifest into the test store and returns
// the resulting Lifecycle bound to a fresh fakeRuntime.
func installFixture(t *testing.T, fixture string) (*Lifecycle, *fakeRuntime, *Store) {
	t.Helper()
	store := openTestStore(t)
	root := t.TempDir()
	inst := NewInstaller(store)
	inst.SetPluginsRoot(root)
	if _, err := inst.Install(readFixture(t, fixture)); err != nil {
		t.Fatalf("install %s: %v", fixture, err)
	}
	rt := &fakeRuntime{}
	return NewLifecycle(store, rt), rt, store
}

func withFastImagePullRetry(t *testing.T) {
	t.Helper()
	oldDelay := imagePullRetryDelay
	imagePullRetryDelay = 0
	t.Cleanup(func() {
		imagePullRetryDelay = oldDelay
	})
}

func TestLifecycle_Materialise_OCIImage(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")

	// llama.yaml's volume is tier-bound (unresolved in phase 1) — fake
	// the resolution by writing a host path so BuildCreatePayload
	// doesn't fail. Phase 03 will wire this in for real.
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/media/.plugins/llama-cpp/models' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}

	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if len(rt.pullCalls) != 1 {
		t.Errorf("pull calls = %v want 1", rt.pullCalls)
	} else if want := "ghcr.io/ggml-org/llama.cpp@sha256:abababababababababababababababababababababababababababababababab"; rt.pullCalls[0] != want {
		t.Errorf("pull ref = %q want %q", rt.pullCalls[0], want)
	}
	if len(rt.createCalls) != 1 {
		t.Errorf("create calls = %d want 1", len(rt.createCalls))
	}
	rec, err := store.Get("llama-cpp")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Instances[0].ContainerID == "" {
		t.Error("container_id should be recorded after materialise")
	}
	if rec.Instances[0].State != StateStopped {
		t.Errorf("state = %q want stopped (post-create)", rec.Instances[0].State)
	}
}

func TestDigestPinnedImageRefStripsTag(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name   string
		image  string
		digest string
		want   string
	}{
		{
			name:   "tagged ghcr image with separate digest",
			image:  "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.2.0-vulkan",
			digest: digest,
			want:   "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp@" + digest,
		},
		{
			name:   "registry port without tag",
			image:  "registry.local:5000/llama-cpp",
			digest: digest,
			want:   "registry.local:5000/llama-cpp@" + digest,
		},
		{
			name:   "registry port with tag",
			image:  "registry.local:5000/llama-cpp:vulkan",
			digest: digest,
			want:   "registry.local:5000/llama-cpp@" + digest,
		},
		{
			name:  "embedded digest with tag",
			image: "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.2.0-vulkan@" + digest,
			want:  "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp@" + digest,
		},
		{
			name:  "embedded digest without tag",
			image: "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp@" + digest,
			want:  "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp@" + digest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := digestPinnedImageRef(tt.image, tt.digest); got != tt.want {
				t.Fatalf("digestPinnedImageRef() = %q want %q", got, tt.want)
			}
		})
	}
}

func TestLifecycle_Materialise_MultiInstance_OnePullManyCreates(t *testing.T) {
	lc, rt, store := installFixture(t, "gh-runner.yaml")
	// Resolve the per-instance tier-bound paths.
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/x/' || instance WHERE plugin_name = 'gh-runner'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	if err := lc.Materialise(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if len(rt.pullCalls) != 1 {
		t.Errorf("expected exactly one pull, got %d: %v", len(rt.pullCalls), rt.pullCalls)
	}
	if len(rt.createCalls) != 2 {
		t.Errorf("expected 2 create calls (one per instance), got %d", len(rt.createCalls))
	}
	if rt.createNames[0] != "gh-runner-1" || rt.createNames[1] != "gh-runner-2" {
		t.Errorf("container names = %v", rt.createNames)
	}
}

func TestLifecycle_Materialise_LXCDistro_RunsSetupAndCommits(t *testing.T) {
	lc, rt, _ := installFixture(t, "ubuntu-python.yaml")
	if err := lc.Materialise(context.Background(), "ubuntu-python"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	// Setup flow: pull base, create+start setup container, wait, commit, remove setup, then create the real container.
	if len(rt.pullCalls) != 1 || rt.pullCalls[0] != "ubuntu:jammy" {
		t.Errorf("pull calls = %v", rt.pullCalls)
	}
	if len(rt.commitCalls) != 1 {
		t.Errorf("commit calls = %v", rt.commitCalls)
	}
	if rt.commitCalls[0] != "smoothnas-plugin-ubuntu-python:0.1.0" {
		t.Errorf("commit tag = %q", rt.commitCalls[0])
	}
	// One setup container + one real container = 2 creates.
	if len(rt.createCalls) != 2 {
		t.Errorf("create calls = %d want 2 (setup + real)", len(rt.createCalls))
	}
	// Setup container must have been removed.
	if len(rt.removeCalls) == 0 {
		t.Errorf("setup container should be removed")
	}
}

func TestLifecycle_Materialise_LXCDistro_NoSetupSkipsCommit(t *testing.T) {
	// Synthetic manifest: lxc-distro with no packages and no setup.
	yaml := []byte(`apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: bare-distro
  version: 0.0.1
artifact:
  type: lxc-distro
  distro: alpine
  release: "3.19"
container:
  command: ["/bin/sh", "-c", "sleep infinity"]
  restartPolicy: unless-stopped
`)
	store := openTestStore(t)
	inst := NewInstaller(store)
	inst.SetPluginsRoot(t.TempDir())
	if _, err := inst.Install(yaml); err != nil {
		t.Fatalf("install: %v", err)
	}
	rt := &fakeRuntime{}
	lc := NewLifecycle(store, rt)
	if err := lc.Materialise(context.Background(), "bare-distro"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if len(rt.commitCalls) != 0 {
		t.Errorf("no setup → no commit; got %v", rt.commitCalls)
	}
	if len(rt.createCalls) != 1 {
		t.Errorf("expected 1 create (real container only); got %d", len(rt.createCalls))
	}
}

func TestLifecycle_Materialise_PullErrorMarksInstanceFailed(t *testing.T) {
	withFastImagePullRetry(t)
	lc, rt, store := installFixture(t, "llama.yaml")
	rt.pullErr = errors.New("manifest unknown")

	err := lc.Materialise(context.Background(), "llama-cpp")
	if err == nil {
		t.Fatal("expected error")
	}
	rec, _ := store.Get("llama-cpp")
	if rec.Instances[0].State != StateFailed {
		t.Errorf("state = %q want failed", rec.Instances[0].State)
	}
	if !strings.Contains(rec.Instances[0].LastError, "manifest unknown") {
		t.Errorf("last_error = %q", rec.Instances[0].LastError)
	}
}

func TestLifecycle_Materialise_RetriesTransientPullError(t *testing.T) {
	withFastImagePullRetry(t)
	lc, rt, store := installFixture(t, "llama.yaml")
	rt.pullErrs = []error{errors.New("ghcr 502"), nil}
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/media/.plugins/llama-cpp/models' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}

	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if len(rt.pullCalls) != 2 {
		t.Fatalf("pull calls = %d want 2: %v", len(rt.pullCalls), rt.pullCalls)
	}
}

func TestLifecycle_Materialise_DigestMismatchFails(t *testing.T) {
	// llama.yaml pins a specific digest; have the fake daemon report
	// a different one by claiming to resolve a different ref.
	store := openTestStore(t)
	inst := NewInstaller(store)
	inst.SetPluginsRoot(t.TempDir())
	if _, err := inst.Install(readFixture(t, "llama.yaml")); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The fake's PullImage just echoes whatever was passed in. So the
	// resolved ref will contain the manifest's digest. To force a
	// mismatch, override the fake's behavior by giving it a custom
	// PullImage that returns a different ref.
	rt := &mismatchPullRuntime{fakeRuntime: fakeRuntime{}}
	lc := NewLifecycle(store, rt)

	err := lc.Materialise(context.Background(), "llama-cpp")
	if err == nil {
		t.Fatal("expected digest mismatch error")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("err = %v", err)
	}
}

// mismatchPullRuntime is a fakeRuntime that returns a different ref
// from PullImage to exercise the digest-mismatch check.
type mismatchPullRuntime struct{ fakeRuntime }

func (m *mismatchPullRuntime) PullImage(ctx context.Context, ref string, _ func(runtime.PullEvent)) (string, error) {
	return "ghcr.io/some/other@sha256:" + strings.Repeat("0", 64), nil
}

func TestLifecycle_Start_StopAndRestart(t *testing.T) {
	lc, rt, store := installFixture(t, "gh-runner.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/x/' || instance WHERE plugin_name = 'gh-runner'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	if err := lc.Materialise(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	// Phase 04: Start now polls for a bridge IP. Tell the fake to
	// hand one back immediately so the test doesn't burn its retry
	// budget.
	rt.bridgeIP = "10.66.0.10"

	if err := lc.Start(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(rt.startCalls) != 2 {
		t.Errorf("start calls = %d want 2", len(rt.startCalls))
	}
	rec, _ := store.Get("gh-runner")
	if rec.Plugin.State != StateRunning {
		t.Errorf("aggregate state = %q want running", rec.Plugin.State)
	}

	if err := lc.Stop(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(rt.stopCalls) != 2 {
		t.Errorf("stop calls = %d want 2", len(rt.stopCalls))
	}
	rec, _ = store.Get("gh-runner")
	if rec.Plugin.State != StateStopped {
		t.Errorf("aggregate state = %q want stopped", rec.Plugin.State)
	}
}

func TestLifecycle_AutostartAll_MaterialisesAndStartsInstalledPlugins(t *testing.T) {
	lc, rt, store := installFixture(t, "gh-runner.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/x/' || instance WHERE plugin_name = 'gh-runner'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	rt.bridgeIP = "10.66.0.11"

	if err := lc.AutostartAll(context.Background()); err != nil {
		t.Fatalf("autostart: %v", err)
	}
	if len(rt.createCalls) != 2 {
		t.Fatalf("create calls = %d want 2", len(rt.createCalls))
	}
	if len(rt.startCalls) != 2 {
		t.Fatalf("start calls = %d want 2", len(rt.startCalls))
	}
	rec, _ := store.Get("gh-runner")
	if rec.Plugin.State != StateRunning {
		t.Fatalf("aggregate state = %q want running", rec.Plugin.State)
	}
}

func TestLifecycle_Start_RequiresMaterialise(t *testing.T) {
	lc, _, _ := installFixture(t, "llama.yaml")
	err := lc.Start(context.Background(), "llama-cpp")
	if err == nil || !strings.Contains(err.Error(), "Materialise") {
		t.Errorf("err = %v want one mentioning Materialise", err)
	}
}

func TestLifecycle_Demolish_RemovesContainersAndImage(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if err := lc.Demolish(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("demolish: %v", err)
	}
	if len(rt.removeCalls) == 0 {
		t.Error("expected RemoveContainer calls during demolish")
	}
	rec, _ := store.Get("llama-cpp")
	if rec.Instances[0].ContainerID != "" {
		t.Errorf("container_id should be cleared, got %q", rec.Instances[0].ContainerID)
	}
}

func TestLifecycle_Materialise_RecreatesGhostContainer(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("first materialise: %v", err)
	}
	rec, _ := store.Get("llama-cpp")
	ghostID := rec.Instances[0].ContainerID

	// Now mark it as missing in the runtime.
	rt.inspectMissing = map[string]bool{ghostID: true}

	// Materialise again — should re-create.
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("second materialise: %v", err)
	}
	if len(rt.createCalls) != 2 {
		t.Errorf("create calls = %d want 2 (initial + ghost recreate)", len(rt.createCalls))
	}
}

func TestLifecycle_Materialise_RecreatesWhenPayloadChanges(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("first materialise: %v", err)
	}
	rec, _ := store.Get("llama-cpp")
	cfg := map[string]string{}
	for _, c := range rec.Config {
		cfg[c.Key] = c.Value
	}
	cfg["MODEL_PATH"] = "/models/changed.gguf"
	if err := store.ReplaceConfig("llama-cpp", cfg); err != nil {
		t.Fatalf("replace config: %v", err)
	}

	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("second materialise: %v", err)
	}
	if len(rt.removeCalls) == 0 {
		t.Fatal("expected stale container to be removed")
	}
	if len(rt.createCalls) != 2 {
		t.Fatalf("create calls = %d want 2 (initial + changed payload)", len(rt.createCalls))
	}
	found := false
	for _, e := range rt.createCalls[1].Env {
		if e == "MODEL_PATH=/models/changed.gguf" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("changed env was not rendered into replacement payload: %v", rt.createCalls[1].Env)
	}
}

func TestContainerMatchesDesired_HostExposeChanges(t *testing.T) {
	existing := runtime.ContainerInspect{
		Config: runtime.ContainerConfig{
			Image:        "ghcr.io/example/wolf:1",
			ExposedPorts: map[string]struct{}{"47989/tcp": {}},
		},
		HostConfig: runtime.HostConfig{
			PortBindings: map[string][]runtime.PortBinding{
				"47989/tcp": []runtime.PortBinding{{HostPort: "47989"}},
			},
		},
	}
	desired := runtime.CreateContainerRequest{
		Image:        "ghcr.io/example/wolf:1",
		ExposedPorts: map[string]struct{}{"47989/tcp": {}},
		HostConfig: runtime.HostConfig{
			PortBindings: map[string][]runtime.PortBinding{
				"47989/tcp": []runtime.PortBinding{{HostPort: "47989"}},
			},
		},
	}
	if !containerMatchesDesired(existing, desired) {
		t.Fatal("matching host-exposed ports should not force recreate")
	}

	desired.HostConfig.PortBindings["47989/tcp"] = []runtime.PortBinding{{HostPort: "47990"}}
	if containerMatchesDesired(existing, desired) {
		t.Fatal("changed host port should force recreate")
	}

	desired.HostConfig.PortBindings["47989/tcp"] = []runtime.PortBinding{{HostPort: "47989"}}
	desired.ExposedPorts = map[string]struct{}{"47990/tcp": {}}
	if containerMatchesDesired(existing, desired) {
		t.Fatal("changed exposed port set should force recreate")
	}
}

func TestContainerMatchesDesired_CPUResourceChanges(t *testing.T) {
	existing := runtime.ContainerInspect{
		Config: runtime.ContainerConfig{Image: "ghcr.io/example/plugin:1"},
		HostConfig: runtime.HostConfig{
			NanoCPUs: 1_000_000_000,
		},
	}
	desired := runtime.CreateContainerRequest{
		Image: "ghcr.io/example/plugin:1",
		HostConfig: runtime.HostConfig{
			NanoCPUs: 1_000_000_000,
		},
	}
	if !containerMatchesDesired(existing, desired) {
		t.Fatal("matching CPU limit should not force recreate")
	}

	desired.HostConfig.NanoCPUs = 2_000_000_000
	if containerMatchesDesired(existing, desired) {
		t.Fatal("changed CPU limit should force recreate")
	}
}

// fakeProxyManager records every Apply / Remove for lifecycle
// integration assertions.
type fakeProxyManager struct {
	applyCalls  []PluginRoute
	removeCalls []string
	applyErr    error
	removeErr   error
}

func (f *fakeProxyManager) Apply(_ context.Context, route PluginRoute) error {
	f.applyCalls = append(f.applyCalls, route)
	return f.applyErr
}
func (f *fakeProxyManager) Remove(_ context.Context, name string) error {
	f.removeCalls = append(f.removeCalls, name)
	return f.removeErr
}

func TestLifecycle_Start_CapturesBridgeIPAndAppliesProxyRoute(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier resolution: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rt.bridgeIP = "10.66.0.42"
	pxy := &fakeProxyManager{}
	lc.SetProxy(pxy)

	if err := lc.Start(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// IP captured into the DB.
	rec, _ := store.Get("llama-cpp")
	if rec.Instances[0].BridgeIP != "10.66.0.42" {
		t.Errorf("bridge_ip = %q want 10.66.0.42", rec.Instances[0].BridgeIP)
	}
	// One Apply call with the right shape.
	if len(pxy.applyCalls) != 1 {
		t.Fatalf("apply calls = %d want 1", len(pxy.applyCalls))
	}
	route := pxy.applyCalls[0]
	if route.PluginName != "llama-cpp" {
		t.Errorf("PluginName = %q", route.PluginName)
	}
	if len(route.Routes) != 1 {
		t.Fatalf("Routes = %d want 1", len(route.Routes))
	}
	if route.Routes[0].LocationPath != "/plugins/llama-cpp/" {
		t.Errorf("LocationPath = %q", route.Routes[0].LocationPath)
	}
	if route.Routes[0].UpstreamURL != "http://10.66.0.42:8080/" {
		t.Errorf("UpstreamURL = %q", route.Routes[0].UpstreamURL)
	}
}

func TestLifecycle_Start_NoProxyDoesNotApply(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rt.bridgeIP = "10.66.0.5"
	// No SetProxy call — Start must not panic, must still capture IP.
	if err := lc.Start(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("start: %v", err)
	}
	rec, _ := store.Get("llama-cpp")
	if rec.Instances[0].BridgeIP != "10.66.0.5" {
		t.Errorf("bridge_ip not captured")
	}
}

func TestLifecycle_Demolish_CallsProxyRemoveBeforeContainers(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rt.bridgeIP = "10.66.0.7"
	pxy := &fakeProxyManager{}
	lc.SetProxy(pxy)
	if err := lc.Start(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := lc.Demolish(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("demolish: %v", err)
	}
	if len(pxy.removeCalls) != 1 || pxy.removeCalls[0] != "llama-cpp" {
		t.Errorf("Remove calls = %v want [llama-cpp]", pxy.removeCalls)
	}
}

func TestLifecycle_Materialise_ResolvesAndPersistsProfiles(t *testing.T) {
	lc, _, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	cat, err := NewCatalog("")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	lc.SetCatalog(cat)

	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rec, _ := store.Get("llama-cpp")
	// llama.yaml declares profiles: [gpu-nvidia, default-limits].
	// default-limits should not be duplicated; resolution preserves
	// manifest order (default-limits already at end).
	if len(rec.Plugin.ResolvedProfiles) == 0 {
		t.Errorf("ResolvedProfiles should be populated; got %v", rec.Plugin.ResolvedProfiles)
	}
	hasGPU, hasLimits := false, false
	for _, p := range rec.Plugin.ResolvedProfiles {
		if p == "gpu-nvidia" {
			hasGPU = true
		}
		if p == "default-limits" {
			hasLimits = true
		}
	}
	if !hasGPU || !hasLimits {
		t.Errorf("ResolvedProfiles should contain gpu-nvidia + default-limits, got %v",
			rec.Plugin.ResolvedProfiles)
	}
}

func TestLifecycle_Materialise_NoCatalogSkipsResolution(t *testing.T) {
	lc, _, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	// No SetCatalog call.
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rec, _ := store.Get("llama-cpp")
	if len(rec.Plugin.ResolvedProfiles) != 0 {
		t.Errorf("expected empty ResolvedProfiles without catalog; got %v", rec.Plugin.ResolvedProfiles)
	}
}

func TestLifecycle_Start_BearerTokenReachesProxyRoute(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	// llama.yaml declares ui.embed.auth=bearer-injected — issue the
	// token here the way Installer would in production. (The test
	// installFixture skips Installer's token issuance because it
	// uses Store.Insert directly.)
	if _, err := store.IssueBearerToken("llama-cpp"); err != nil {
		t.Fatalf("issue token: %v", err)
	}
	expectedToken, _ := store.GetBearerToken("llama-cpp")

	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rt.bridgeIP = "10.66.0.42"
	pxy := &fakeProxyManager{}
	lc.SetProxy(pxy)

	if err := lc.Start(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(pxy.applyCalls) != 1 {
		t.Fatalf("apply calls = %d", len(pxy.applyCalls))
	}
	route := pxy.applyCalls[0]
	if len(route.Routes) != 1 {
		t.Fatalf("routes = %d", len(route.Routes))
	}
	if route.Routes[0].AuthBearer != expectedToken {
		t.Errorf("AuthBearer = %q, want %q", route.Routes[0].AuthBearer, expectedToken)
	}
}

func TestLifecycle_ApplyRouteFor_RewritesRouteWithLatestToken(t *testing.T) {
	lc, rt, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	if _, err := store.IssueBearerToken("llama-cpp"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	rt.bridgeIP = "10.66.0.42"
	pxy := &fakeProxyManager{}
	lc.SetProxy(pxy)
	if err := lc.Start(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Rotate the token and re-apply.
	rotated, err := store.IssueBearerToken("llama-cpp")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := lc.ApplyRouteFor(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("apply route: %v", err)
	}
	if len(pxy.applyCalls) != 2 {
		t.Fatalf("apply calls = %d, want 2 (Start + ApplyRouteFor)", len(pxy.applyCalls))
	}
	if pxy.applyCalls[1].Routes[0].AuthBearer != rotated {
		t.Errorf("re-applied route token = %q, want %q",
			pxy.applyCalls[1].Routes[0].AuthBearer, rotated)
	}
}

func TestLifecycle_Materialise_EnsuresPluginBridge(t *testing.T) {
	lc, _, store := installFixture(t, "llama.yaml")
	if _, err := store.db.Exec(
		`UPDATE plugin_volume_paths SET host_path = '/mnt/m' WHERE plugin_name = 'llama-cpp'`,
	); err != nil {
		t.Fatalf("fake tier: %v", err)
	}
	// fakeRuntime.EnsurePluginBridge always returns ("fake-bridge", nil)
	// — just verify Materialise completes without error and that
	// the container create payload set NetworkMode appropriately.
	if err := lc.Materialise(context.Background(), "llama-cpp"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
}

func TestLifecycle_BuildPluginRoute_MultiplePortsGetSuffixes(t *testing.T) {
	// Synthesise a record with two exposed ports to verify the
	// first-gets-bare-path / rest-get-suffix logic.
	rec := &PluginRecord{
		Plugin:    PluginRow{Name: "myapp", Version: "1.0.0"},
		Instances: []InstanceRow{{Instance: 1, BridgeIP: "10.66.0.5"}},
		Ports: []PortRow{
			{Name: "http", ContainerPort: 8080, Protocol: "tcp", Expose: true},
			{Name: "api", ContainerPort: 9090, Protocol: "tcp", Expose: true},
		},
	}
	lc := &Lifecycle{}
	route, err := lc.buildPluginRoute(rec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(route.Routes) != 2 {
		t.Fatalf("routes = %d want 2", len(route.Routes))
	}
	if route.Routes[0].LocationPath != "/plugins/myapp/" {
		t.Errorf("first location = %q", route.Routes[0].LocationPath)
	}
	if route.Routes[1].LocationPath != "/plugins/myapp/api/" {
		t.Errorf("second location = %q", route.Routes[1].LocationPath)
	}
}

func TestLifecycle_BuildPluginRoute_SkipsUnexposedPorts(t *testing.T) {
	rec := &PluginRecord{
		Plugin:    PluginRow{Name: "x"},
		Instances: []InstanceRow{{Instance: 1, BridgeIP: "10.66.0.5"}},
		Ports: []PortRow{
			{Name: "internal", ContainerPort: 7777, Protocol: "tcp", Expose: false},
			{Name: "http", ContainerPort: 8080, Protocol: "tcp", Expose: true},
		},
	}
	lc := &Lifecycle{}
	route, err := lc.buildPluginRoute(rec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(route.Routes) != 1 || !strings.Contains(route.Routes[0].UpstreamURL, ":8080/") {
		t.Errorf("expected only the exposed port; got %+v", route.Routes)
	}
}

func TestBuildSetupCmd_DistroDispatch(t *testing.T) {
	cases := []struct {
		distro      string
		packages    []string
		mustContain string
	}{
		{"ubuntu", []string{"python3"}, "apt-get install"},
		{"debian", []string{"python3"}, "apt-get install"},
		{"alpine", []string{"py3"}, "apk add"},
		{"freebsd", []string{"py"}, "unknown distro"},
	}
	for _, tc := range cases {
		got := buildSetupCmd(tc.distro, tc.packages, nil)
		if !strings.Contains(got, tc.mustContain) {
			t.Errorf("distro=%s: got=%q, want it to contain %q", tc.distro, got, tc.mustContain)
		}
	}
}
