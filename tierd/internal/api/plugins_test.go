package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// readManifestFixture pulls a YAML manifest from the plugin
// package's testdata so tests share the same source of truth as the
// plugin package's own tests.
func readManifestFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "plugin", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// fakeTierProvider for the API tests. The installer needs one to
// resolve tier-bound volumes; we register a single 'media' tier
// pointing at a tempdir so installs can mkdir the volume tree.
type fakeAPITierProvider struct {
	mountPoint string
}

func (f *fakeAPITierProvider) GetTierInstance(name string) (*db.TierInstance, error) {
	if name == "media" {
		return &db.TierInstance{Name: name, MountPoint: f.mountPoint, State: db.TierPoolStateHealthy}, nil
	}
	return nil, db.ErrNotFound
}

func (f *fakeAPITierProvider) ListTierSlots(_ string) ([]db.TierSlot, error) { return nil, nil }

// newPluginsHandlerForTest builds a fully-wired PluginsHandler
// against an in-memory DB. Lifecycle is left nil so lifecycle verbs
// surface 503 — phase 6a tests focus on install/list/uninstall/
// preflight/config since lifecycle integration was already covered
// in the plugin package's own tests.
func newPluginsHandlerForTest(t *testing.T) (*PluginsHandler, *plugin.Installer) {
	t.Helper()
	dbStore, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := dbStore.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { dbStore.Close() })

	pluginStore := plugin.NewStore(dbStore)
	tierMount := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(tierMount, 0o755); err != nil {
		t.Fatalf("mkdir tier mount: %v", err)
	}
	tp := &fakeAPITierProvider{mountPoint: tierMount}

	inst := plugin.NewInstaller(pluginStore)
	inst.SetPluginsRoot(t.TempDir())
	inst.SetTierProvider(tp, nil)

	cat, err := plugin.NewCatalog("")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	return NewPluginsHandler(pluginStore, inst, nil, cat, tp), inst
}

// doJSON is a tiny test helper that assembles a request, dispatches
// through the handler, and returns the recorded response.
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// httpServeMux wraps PluginsHandler.Route so doJSON can use http.Handler.
type routeHandler struct {
	h *PluginsHandler
}

func (r *routeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.h.Route(w, req)
}

func TestPluginsAPI_ListEmpty(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string][]pluginListItem
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got["plugins"]) != 0 {
		t.Errorf("expected empty list, got %v", got)
	}
}

func TestPluginsAPI_ParseReturnsManifestJSONShape(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/parse", map[string]any{
		"manifest": readManifestFixture(t, "llama.yaml"),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Manifest map[string]any `json:"manifest"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp.Manifest["Metadata"]; ok {
		t.Fatalf("manifest used Go field name Metadata; body=%s", rr.Body.String())
	}
	metadata, ok := resp.Manifest["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.metadata missing or wrong type: %#v", resp.Manifest["metadata"])
	}
	if got := metadata["name"]; got != "llama-cpp" {
		t.Fatalf("manifest.metadata.name = %#v", got)
	}
	artifact, ok := resp.Manifest["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.artifact missing or wrong type: %#v", resp.Manifest["artifact"])
	}
	if got := artifact["image"]; got != "ghcr.io/ggml-org/llama.cpp:server-cuda-b3500" {
		t.Fatalf("manifest.artifact.image = %#v", got)
	}
	volumes, ok := resp.Manifest["volumes"].([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("manifest.volumes = %#v", resp.Manifest["volumes"])
	}
	firstVolume, ok := volumes[0].(map[string]any)
	if !ok {
		t.Fatalf("manifest.volumes[0] wrong type: %#v", volumes[0])
	}
	if got := firstVolume["minSize"]; got != "50G" {
		t.Fatalf("manifest.volumes[0].minSize = %#v", got)
	}
}

func TestPluginsAPI_PreflightFailureSurfacesPlacements(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/preflight", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{}, // no tier!
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight should still respond 200 with ok:false; got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false for missing tier assignment")
	}
	if len(resp.Placements) == 0 || len(resp.Placements[0].Errors) == 0 {
		t.Errorf("expected per-volume errors; got %+v", resp.Placements)
	}
}

func TestPluginsAPI_PreflightHappyPath(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/preflight", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp preflightResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.OK {
		t.Errorf("expected ok=true; got placements=%+v", resp.Placements)
	}
}

func TestPluginsAPI_InstallHappyPath(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var detail pluginDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Plugin.Name != "llama-cpp" {
		t.Errorf("name = %q", detail.Plugin.Name)
	}
	if detail.Plugin.State != plugin.StateInstalled {
		t.Errorf("state = %q want installed", detail.Plugin.State)
	}
	if detail.Manifest == "" {
		t.Errorf("manifest YAML should be returned")
	}
}

func TestPluginsAPI_InstallPreflightFailure400(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{}, // no tier
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "preflight_failed") {
		t.Errorf("error body should include preflight_failed code: %s", rr.Body.String())
	}
}

func TestPluginsAPI_InstallDuplicate409(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	body := map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	}
	first := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first install: %d %s", first.Code, first.Body.String())
	}
	second := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", body)
	if second.Code != http.StatusConflict {
		t.Errorf("second install status = %d want 409; body=%s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "already_exists") {
		t.Errorf("expected already_exists error code: %s", second.Body.String())
	}
}

func TestPluginsAPI_DetailRoundTrip(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/llama-cpp", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rr.Code, rr.Body.String())
	}
	var detail pluginDetail
	_ = json.Unmarshal(rr.Body.Bytes(), &detail)
	if detail.Plugin.Name != "llama-cpp" {
		t.Errorf("name = %q", detail.Plugin.Name)
	}
	if len(detail.Volumes) != 1 || detail.Volumes[0].TierPool != "media" {
		t.Errorf("volume tier_pool wrong: %+v", detail.Volumes)
	}
}

func TestPluginsAPI_DetailNotFound(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/ghost", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not_found") {
		t.Errorf("expected not_found code; got %s", rr.Body.String())
	}
}

func TestPluginsAPI_LifecycleWithoutRuntime503(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/start", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d want 503; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "runtime_unavailable") {
		t.Errorf("expected runtime_unavailable code; got %s", rr.Body.String())
	}
}

func TestPluginsAPI_UpdateConfig(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	rr := doJSON(t, &routeHandler{h}, http.MethodPut, "/api/plugins/llama-cpp/config", map[string]any{
		"config": map[string]string{"MODEL_PATH": "/models/custom.gguf"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("config: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "restartNeeded") {
		t.Errorf("response should mention restartNeeded: %s", rr.Body.String())
	}
	// Detail should now show the new value.
	det := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/llama-cpp", nil)
	var detail pluginDetail
	_ = json.Unmarshal(det.Body.Bytes(), &detail)
	if len(detail.Config) != 1 || detail.Config[0].Value != "/models/custom.gguf" {
		t.Errorf("config not updated: %+v", detail.Config)
	}
}

func TestPluginsAPI_UpdateConfig_MissingPlugin404(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPut, "/api/plugins/ghost/config", map[string]any{
		"config": map[string]string{"X": "Y"},
	})
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d want 404", rr.Code)
	}
}

func TestPluginsAPI_Uninstall(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	rr := doJSON(t, &routeHandler{h}, http.MethodDelete, "/api/plugins/llama-cpp", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	det := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/llama-cpp", nil)
	if det.Code != http.StatusNotFound {
		t.Errorf("plugin should be gone after uninstall; got %d", det.Code)
	}
}

func TestPluginsAPI_MethodNotAllowed(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPatch, "/api/plugins", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d want 405", rr.Code)
	}
}

func TestPluginsAPI_EventsNotImplemented501(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/x/events", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d want 501", rr.Code)
	}
}

func TestPluginsAPI_PreflightBadManifest400(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/preflight", map[string]any{
		"manifest": "not yaml: {{{",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d want 400; body=%s", rr.Code, rr.Body.String())
	}
}
