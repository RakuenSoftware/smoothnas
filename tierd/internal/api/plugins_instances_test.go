package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// stubRuntime is a non-functional RuntimeClient that satisfies the
// interface so the api tests can attach a Lifecycle. It is wired
// only to exercise scale validation paths — every method that would
// touch a real daemon either no-ops or returns an error.
type stubRuntime struct{}

func (stubRuntime) Ping(context.Context) error                  { return nil }
func (stubRuntime) Info(context.Context) (runtime.Info, error)  { return runtime.Info{}, nil }
func (stubRuntime) PullImage(context.Context, string, func(runtime.PullEvent)) (string, error) {
	return "", errors.New("stub: pull not supported")
}
func (stubRuntime) RemoveImage(context.Context, string) error { return nil }
func (stubRuntime) ListImages(context.Context) ([]runtime.ImageSummary, error) {
	return nil, nil
}
func (stubRuntime) CreateContainer(context.Context, string, runtime.CreateContainerRequest) (runtime.CreateContainerResponse, error) {
	return runtime.CreateContainerResponse{}, errors.New("stub: create not supported")
}
func (stubRuntime) StartContainer(context.Context, string) error      { return nil }
func (stubRuntime) StopContainer(context.Context, string, int) error  { return nil }
func (stubRuntime) RestartContainer(context.Context, string, int) error { return nil }
func (stubRuntime) RemoveContainer(context.Context, string, bool) error { return nil }
func (stubRuntime) InspectContainer(context.Context, string) (runtime.ContainerInspect, error) {
	return runtime.ContainerInspect{}, &runtime.APIError{StatusCode: 404, Message: "stub: not found"}
}
func (stubRuntime) ListManagedContainers(context.Context) ([]runtime.ContainerSummary, error) {
	return nil, nil
}
func (stubRuntime) WaitContainer(context.Context, string) (int, error) { return 0, nil }
func (stubRuntime) CommitContainer(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (stubRuntime) StreamLogs(context.Context, string, runtime.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (stubRuntime) SubscribeEvents(context.Context) (<-chan runtime.Event, <-chan error, error) {
	ev := make(chan runtime.Event)
	er := make(chan error)
	close(ev)
	close(er)
	return ev, er, nil
}
func (stubRuntime) EnsurePluginBridge(context.Context) (string, error) { return "stub-bridge", nil }
func (stubRuntime) InspectContainerBridgeIP(context.Context, string) (string, error) {
	return "", runtime.ErrBridgeIPNotReady
}

// installAndAttachLifecycle is a helper for the instances tests:
// installs the supplied YAML manifest fixture and wires a Lifecycle
// backed by a stub runtime. Returns the handler and the parsed
// pluginsHandler for the route table.
func installAndAttachLifecycle(t *testing.T, fixture string) *PluginsHandler {
	t.Helper()
	h, inst := newPluginsHandlerForTest(t)
	yaml := readManifestFixture(t, fixture)
	if _, err := inst.InstallWithOptions([]byte(yaml), plugin.InstallOptions{
		Tiers: plugin.TierAssignments{Default: "media"},
	}); err != nil {
		t.Fatalf("install %s: %v", fixture, err)
	}
	lc := plugin.NewLifecycle(h.store, stubRuntime{})
	h.lifecycle = lc
	return h
}

func TestPluginsAPI_ListInstances_AfterInstall(t *testing.T) {
	h := installAndAttachLifecycle(t, "gh-runner.yaml")

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/gh-runner/instances", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got listInstancesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Plugin != "gh-runner" {
		t.Errorf("plugin = %q", got.Plugin)
	}
	if got.Count != 2 {
		t.Errorf("count = %d want 2 (gh-runner manifest)", got.Count)
	}
	if !got.Configurable {
		t.Error("configurable should be true for gh-runner manifest")
	}
	if len(got.Instances) != 2 {
		t.Errorf("instance rows = %d want 2", len(got.Instances))
	}
}

func TestPluginsAPI_ListInstances_NotFound(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/nope/instances", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginsAPI_ScaleInstances_RuntimeMissing503(t *testing.T) {
	h, inst := newPluginsHandlerForTest(t)
	if _, err := inst.InstallWithOptions(
		[]byte(readManifestFixture(t, "gh-runner.yaml")),
		plugin.InstallOptions{Tiers: plugin.TierAssignments{Default: "media"}},
	); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Lifecycle deliberately not wired — should 503.
	rr := doJSON(t, &routeHandler{h}, http.MethodPost,
		"/api/plugins/gh-runner/instances", map[string]any{"count": 4})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginsAPI_ScaleInstances_InvalidTarget(t *testing.T) {
	h := installAndAttachLifecycle(t, "gh-runner.yaml")
	rr := doJSON(t, &routeHandler{h}, http.MethodPost,
		"/api/plugins/gh-runner/instances", map[string]any{"count": 0})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugins.scale.invalid_target") {
		t.Errorf("expected scale.invalid_target code in body, got %s", rr.Body.String())
	}
}

func TestPluginsAPI_ScaleInstances_NotConfigurable(t *testing.T) {
	h := installAndAttachLifecycle(t, "llama.yaml")
	// llama-cpp manifest has no configurable: true. Try to scale to 1
	// (no-op) — that's allowed for non-configurable plugins? No: the
	// handler checks configurable first. But target == current (1) is
	// reported as no-op before the configurable check kicks in. So
	// fire a target that would otherwise be rejected.
	rr := doJSON(t, &routeHandler{h}, http.MethodPost,
		"/api/plugins/llama-cpp/instances", map[string]any{"count": 4})
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d body=%s want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugins.scale.not_configurable") {
		t.Errorf("expected scale.not_configurable code, got %s", rr.Body.String())
	}
}

func TestPluginsAPI_ScaleInstances_BoundaryRejected(t *testing.T) {
	h := installAndAttachLifecycle(t, "gh-runner.yaml")
	rr := doJSON(t, &routeHandler{h}, http.MethodPost,
		"/api/plugins/gh-runner/instances", map[string]any{"count": 1})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugins.scale.boundary") {
		t.Errorf("expected scale.boundary code, got %s", rr.Body.String())
	}
}

func TestPluginsAPI_ScaleInstances_NoOpWhenTargetMatches(t *testing.T) {
	h := installAndAttachLifecycle(t, "gh-runner.yaml")
	rr := doJSON(t, &routeHandler{h}, http.MethodPost,
		"/api/plugins/gh-runner/instances", map[string]any{"count": 2})
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var res plugin.ScaleResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.NoOp || res.From != 2 || res.To != 2 {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestPluginsAPI_ScaleInstances_MethodNotAllowed(t *testing.T) {
	h := installAndAttachLifecycle(t, "gh-runner.yaml")
	rr := doJSON(t, &routeHandler{h}, http.MethodDelete,
		"/api/plugins/gh-runner/instances", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}
