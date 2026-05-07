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
	createCalls   []runtime.CreateContainerRequest
	createNames   []string
	startCalls    []string
	stopCalls     []string
	removeCalls   []string
	pullCalls     []string
	commitCalls   []string
	waitCalls     []string

	// Behaviour knobs.
	pullErr     error
	createErr   error
	startErr    error
	waitExit    int
	commitErr   error
	inspectMissing map[string]bool // container IDs that 404 on inspect

	// Generated container IDs counter.
	nextID int
}

func (f *fakeRuntime) Ping(ctx context.Context) error                            { return nil }
func (f *fakeRuntime) Info(ctx context.Context) (runtime.Info, error)            { return runtime.Info{}, nil }
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
	return nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, id string) (runtime.ContainerInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectMissing[id] {
		return runtime.ContainerInspect{}, &runtime.APIError{StatusCode: 404, Message: "no such container"}
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
