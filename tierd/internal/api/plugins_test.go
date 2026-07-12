package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
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

func TestPluginsAPI_ListIncludesContainerRefs(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	install := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "gh-runner.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if install.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", install.Code, install.Body.String())
	}

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string][]pluginListItem
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got["plugins"]) != 1 {
		t.Fatalf("plugins = %+v", got["plugins"])
	}
	row := got["plugins"][0]
	if !row.ContainerUpdateAvailable {
		t.Fatalf("containerUpdateAvailable = false, want true")
	}
	if len(row.ContainerRefs) != 1 {
		t.Fatalf("container refs = %+v", row.ContainerRefs)
	}
	ref := row.ContainerRefs[0]
	if ref.Service != "gh-runner" || ref.Name != "primary" {
		t.Fatalf("container ref identity = %+v", ref)
	}
	if ref.ImageRef != "ghcr.io/rakuensoftware/smoothnas-plugin-gh-runner:0.1.0" {
		t.Fatalf("container ref image = %q", ref.ImageRef)
	}
}

func TestPluginsAPI_ContainerUpdateAvailabilityUsesOriginalRef(t *testing.T) {
	refs := []plugin.ContainerRefRow{{
		ImageRef:    "ghcr.io/example/app:edge",
		Digest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResolvedRef: "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	if !hasMutableContainerRef(refs) {
		t.Fatalf("resolved mutable tag should remain updateable")
	}

	pinned := []plugin.ContainerRefRow{{
		ImageRef: "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Digest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	if hasMutableContainerRef(pinned) {
		t.Fatalf("digest-pinned ref should not be updateable")
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
	services, ok := resp.Manifest["services"].([]any)
	if !ok || len(services) != 1 {
		t.Fatalf("manifest.services = %#v", resp.Manifest["services"])
	}
	service, ok := services[0].(map[string]any)
	if !ok {
		t.Fatalf("manifest.services[0] wrong type: %#v", services[0])
	}
	artifact, ok := service["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.services[0].artifact missing or wrong type: %#v", service["artifact"])
	}
	if got := artifact["image"]; got != "ghcr.io/ggml-org/llama.cpp:server-cuda-b3500" {
		t.Fatalf("manifest.services[0].artifact.image = %#v", got)
	}
	volumes, ok := service["volumes"].([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("manifest.services[0].volumes = %#v", service["volumes"])
	}
	firstVolume, ok := volumes[0].(map[string]any)
	if !ok {
		t.Fatalf("manifest.services[0].volumes[0] wrong type: %#v", volumes[0])
	}
	if got := firstVolume["minSize"]; got != "50G" {
		t.Fatalf("manifest.services[0].volumes[0].minSize = %#v", got)
	}
}

func TestPluginsAPI_ListGPUsRoute(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/gpus", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		GPUs []map[string]any `json:"gpus"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GPUs == nil {
		t.Fatalf("gpus key missing from body=%s", rr.Body.String())
	}
}

func TestPluginsAPI_CatalogLatestFetchesReleaseManifest(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	fixture := readManifestFixture(t, "gh-runner.yaml")

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/Community/smoothnas-plugin-demo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "SmoothNAS" {
			t.Fatalf("User-Agent = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.3.2",
			"html_url": "https://github.com/Community/smoothnas-plugin-demo/releases/tag/v0.3.2",
			"assets": []map[string]any{
				{"name": "notes.txt", "browser_download_url": srv.URL + "/assets/notes.txt"},
				{"name": "smoothnas-plugin-intel.yaml", "browser_download_url": srv.URL + "/assets/smoothnas-plugin-intel.yaml"},
				{"name": "smoothnas-plugin.yaml", "browser_download_url": srv.URL + "/assets/smoothnas-plugin.yaml"},
				{"name": "smoothnas-plugin-amd.yaml", "browser_download_url": srv.URL + "/assets/smoothnas-plugin-amd.yaml"},
			},
		})
	})
	mux.HandleFunc("/assets/smoothnas-plugin.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fixture))
	})
	mux.HandleFunc("/assets/smoothnas-plugin-amd.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fixture))
	})
	mux.HandleFunc("/assets/smoothnas-plugin-intel.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fixture))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	h.catalogAPIBaseURL = srv.URL
	h.catalogHTTPClient = srv.Client()

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/latest?repo=Community/smoothnas-plugin-demo", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got pluginCatalogLatestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TagName != "v0.3.2" {
		t.Fatalf("tag = %q", got.TagName)
	}
	if len(got.Manifests) != 3 {
		t.Fatalf("manifests = %d", len(got.Manifests))
	}
	if got.Manifests[0].AssetName != "smoothnas-plugin.yaml" {
		t.Fatalf("asset = %q", got.Manifests[0].AssetName)
	}
	if got.Manifests[1].AssetName != "smoothnas-plugin-amd.yaml" || got.Manifests[2].AssetName != "smoothnas-plugin-intel.yaml" {
		t.Fatalf("manifest assets not sorted with base first: %#v", got.Manifests)
	}
	if got.Manifests[0].Manifest.Metadata.Name != "gh-runner" {
		t.Fatalf("manifest name = %q", got.Manifests[0].Manifest.Metadata.Name)
	}
	if !strings.Contains(got.Manifests[0].ManifestYAML, "apiVersion: smoothnas.io/v1") {
		t.Fatalf("manifest yaml missing fixture content")
	}
}

func TestPluginsAPI_CatalogLatestRejectsInvalidRepo(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/latest?repo=https://github.com/Community/smoothnas-plugin-demo", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
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

func TestPluginsAPI_DetailUsesEmptyArraysForNoPortPlugin(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	install := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "gh-runner.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if install.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", install.Code, install.Body.String())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/plugins/gh-runner", nil)
	(&routeHandler{h}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rr.Code, rr.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	for _, key := range []string{"instances", "volumes", "ports", "config"} {
		if _, ok := raw[key].([]any); !ok {
			t.Fatalf("%s = %#v, want JSON array", key, raw[key])
		}
	}
}

func TestPluginsAPI_DetailExposesServices(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	install := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "gh-runner.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if install.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", install.Code, install.Body.String())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/plugins/gh-runner", nil)
	(&routeHandler{h}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rr.Code, rr.Body.String())
	}

	var detail pluginDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Services) != 1 {
		t.Fatalf("services = %+v, want one", detail.Services)
	}
	svc := detail.Services[0]
	if svc.Name != "gh-runner" {
		t.Errorf("service name = %q want gh-runner", svc.Name)
	}
	if svc.ArtifactType != "oci-image" {
		t.Errorf("service artifactType = %q", svc.ArtifactType)
	}
	// gh-runner is count: 2, so the service rolls up two instances.
	if len(svc.Instances) != 2 {
		t.Errorf("service instances = %d want 2", len(svc.Instances))
	}
	if svc.State != "installed" {
		t.Errorf("service state = %q want installed", svc.State)
	}
}

func TestPluginsAPI_InstallAppliesInitialConfig(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
		"config":          map[string]string{"MODEL_PATH": "/models/custom.gguf"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var detail pluginDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := ""
	for _, cfg := range detail.Config {
		if cfg.Key == "MODEL_PATH" {
			got = cfg.Value
			break
		}
	}
	if got != "/models/custom.gguf" {
		t.Fatalf("MODEL_PATH = %q, want install-time config override; config=%+v", got, detail.Config)
	}
}

func TestPluginsAPI_UpdateInstalledPlugin(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	install := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if install.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", install.Code, install.Body.String())
	}

	manifest := strings.Replace(readManifestFixture(t, "llama.yaml"), "version: 0.1.0", "version: 9.9.9", 1)
	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/update", map[string]any{
		"manifest": manifest,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", rr.Code, rr.Body.String())
	}
	var detail pluginDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Plugin.Version != "9.9.9" {
		t.Fatalf("version = %q", detail.Plugin.Version)
	}
	if detail.Plugin.State != plugin.StateInstalled {
		t.Fatalf("state = %q", detail.Plugin.State)
	}
	if !strings.Contains(detail.Manifest, "version: 9.9.9") {
		t.Fatalf("manifest was not replaced: %s", detail.Manifest)
	}
}

func TestPluginsAPI_InstallAutostartsWhenRuntimeIsWired(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rt := &fakeModelRuntime{}
	h.lifecycle = plugin.NewLifecycle(h.store, rt)

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
	// Install returns immediately; materialise + start run in the background.
	rt.waitCounts(t, 1, 1)
	if c, s := rt.counts(); c != 1 || s != 1 {
		t.Fatalf("runtime created=%d started=%d, want 1/1", c, s)
	}
	waitPluginState(t, h.store, detail.Plugin.Name, plugin.StateRunning)
}

func TestPluginsAPI_RefreshContainersEndpoint(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rt := &fakeModelRuntime{}
	h.lifecycle = plugin.NewLifecycle(h.store, rt)

	install := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if install.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", install.Code, install.Body.String())
	}

	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/refresh-containers", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"state"`) {
		t.Fatalf("refresh response missing state: %s", rr.Body.String())
	}
}

func TestPluginsAPI_SetPinnedImageAppliesInPlace(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rt := &fakeModelRuntime{}
	h.lifecycle = plugin.NewLifecycle(h.store, rt)

	install := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	if install.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", install.Code, install.Body.String())
	}
	// Install materialises in the background; wait for it to settle before
	// snapshotting the create/start counts.
	rt.waitCounts(t, 1, 1)
	createsAfterInstall, startsAfterInstall := rt.counts()

	const pinned = "ghcr.io/example/custom-llama:vulkan"
	rr := doJSON(t, &routeHandler{h}, http.MethodPut, "/api/plugins/llama-cpp/image", map[string]any{
		"image": pinned,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("set image status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"applied":true`) {
		t.Fatalf("response should report applied=true: %s", rr.Body.String())
	}

	// The pin must be applied in place: the running plugin is re-materialised
	// (new container created onto the pinned image) and restarted. A plain
	// restart would NOT re-resolve the image, so this guards the regression.
	createsNow, startsNow := rt.counts()
	if createsNow <= createsAfterInstall {
		t.Fatalf("expected a new container create after pin; created=%d (was %d)", createsNow, createsAfterInstall)
	}
	if startsNow <= startsAfterInstall {
		t.Fatalf("expected a restart after pin; started=%d (was %d)", startsNow, startsAfterInstall)
	}
	last := rt.lastCreated()
	if last.Image != pinned {
		t.Fatalf("container created with image %q, want pinned %q", last.Image, pinned)
	}

	// And the pin persists in the detail view.
	det := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/llama-cpp", nil)
	if !strings.Contains(det.Body.String(), pinned) {
		t.Fatalf("detail should report pinned image %q: %s", pinned, det.Body.String())
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
	gotModelPath := ""
	for _, cfg := range detail.Config {
		if cfg.Key == "MODEL_PATH" {
			gotModelPath = cfg.Value
			break
		}
	}
	if gotModelPath != "/models/custom.gguf" {
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

func TestPluginsAPI_ModelInstallDownloadsUpdatesConfigAndStarts(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	rt := &fakeModelRuntime{}
	h.lifecycle = plugin.NewLifecycle(h.store, rt)

	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})
	// Wait for the async install materialise (create #1) before the model
	// install re-materialises (create #2), so the two don't race.
	rt.waitCounts(t, 1, 1)

	modelBytes := []byte("GGUF test model")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(modelBytes)
	}))
	defer srv.Close()

	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/models/install", map[string]any{
		"url":         srv.URL + "/tiny.gguf",
		"temperature": 0.35,
	})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp installModelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	job := waitForJobDone(t, resp.JobID)
	if job.Status != "completed" {
		t.Fatalf("job status = %s error=%s progress=%s", job.Status, job.Error, job.Progress)
	}

	rec, err := h.store.Get("llama-cpp")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	cfg := map[string]string{}
	for _, row := range rec.Config {
		cfg[row.Key] = row.Value
	}
	if cfg["MODEL_PATH"] != "/models/tiny.gguf" {
		t.Fatalf("MODEL_PATH = %q", cfg["MODEL_PATH"])
	}
	if cfg[pluginModelTemperatureConfigKey] != "0.35" {
		t.Fatalf("%s = %q", pluginModelTemperatureConfigKey, cfg[pluginModelTemperatureConfigKey])
	}
	modelDir := rec.Volumes[0].Paths[1]
	got, err := os.ReadFile(filepath.Join(modelDir, "tiny.gguf"))
	if err != nil {
		t.Fatalf("read installed model: %v", err)
	}
	if string(got) != string(modelBytes) {
		t.Fatalf("model bytes = %q", string(got))
	}
	if c, s := rt.counts(); c != 2 || s != 2 {
		t.Fatalf("runtime created=%d started=%d, want 2/2", c, s)
	}
}

func TestPluginsAPI_ModelInstallRejectsBadURL(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})

	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/models/install", map[string]any{
		"url": "file:///tmp/model.gguf",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugins.models.url_invalid") {
		t.Fatalf("expected url_invalid code, got %s", rr.Body.String())
	}
}

func TestPluginsAPI_ModelInstallRejectsBadTemperature(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})

	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/models/install", map[string]any{
		"url":         "https://example.com/model.gguf",
		"temperature": -0.1,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugins.models.temperature_invalid") {
		t.Fatalf("expected temperature_invalid code, got %s", rr.Body.String())
	}
}

func TestPluginsAPI_ModelInstallRequiresRuntime(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/install", map[string]any{
		"manifest":        readManifestFixture(t, "llama.yaml"),
		"tierAssignments": map[string]any{"default": "media"},
	})

	rr := doJSON(t, &routeHandler{h}, http.MethodPost, "/api/plugins/llama-cpp/models/install", map[string]any{
		"url": "https://example.com/model.gguf",
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugins.runtime_unavailable") {
		t.Fatalf("expected runtime_unavailable code, got %s", rr.Body.String())
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

func waitForJobDone(t *testing.T, id string) *JobStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if job := jobs.Get(id); job != nil && job.Status != "running" {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return nil
}

type fakeModelRuntime struct {
	mu      sync.Mutex
	created []runtime.CreateContainerRequest
	started []string
}

// counts returns the recorded create/start counts under the lock (install now
// materialises in a background goroutine, so tests read these concurrently).
func (f *fakeModelRuntime) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.started)
}

// lastCreated returns a copy of the most recent create request under the lock.
func (f *fakeModelRuntime) lastCreated() runtime.CreateContainerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[len(f.created)-1]
}

// waitCounts blocks until the runtime has recorded at least the given create +
// start counts (background materialise), or fails after a short timeout.
func (f *fakeModelRuntime) waitCounts(t *testing.T, created, started int) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if c, s := f.counts(); c >= created && s >= started {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c, s := f.counts()
	t.Fatalf("runtime did not reach created>=%d started>=%d (got created=%d started=%d)", created, started, c, s)
}

// waitPluginState polls the store until the plugin reaches want, or fails.
func waitPluginState(t *testing.T, store *plugin.Store, name, want string) {
	t.Helper()
	var last string
	for i := 0; i < 400; i++ {
		if rec, err := store.Get(name); err == nil {
			last = rec.Plugin.State
			if last == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("plugin %q state = %q, want %q", name, last, want)
}

func (f *fakeModelRuntime) Ping(context.Context) error { return nil }

func (f *fakeModelRuntime) Info(context.Context) (runtime.Info, error) { return runtime.Info{}, nil }

func (f *fakeModelRuntime) PullImage(_ context.Context, ref string, _ func(runtime.PullEvent)) (string, error) {
	return ref, nil
}

func (f *fakeModelRuntime) RemoveImage(context.Context, string) error { return nil }

func (f *fakeModelRuntime) CreateContainer(_ context.Context, _ string, req runtime.CreateContainerRequest) (runtime.CreateContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return runtime.CreateContainerResponse{ID: "ctr-1"}, nil
}

func (f *fakeModelRuntime) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return nil
}

func (f *fakeModelRuntime) StopContainer(context.Context, string, int) error { return nil }

func (f *fakeModelRuntime) RestartContainer(context.Context, string, int) error { return nil }

func (f *fakeModelRuntime) RemoveContainer(context.Context, string, bool) error { return nil }

func (f *fakeModelRuntime) InspectContainer(context.Context, string) (runtime.ContainerInspect, error) {
	return runtime.ContainerInspect{}, nil
}

func (f *fakeModelRuntime) ListManagedContainers(context.Context) ([]runtime.ContainerSummary, error) {
	return nil, nil
}

func (f *fakeModelRuntime) WaitContainer(context.Context, string) (int, error) { return 0, nil }

func (f *fakeModelRuntime) CommitContainer(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (f *fakeModelRuntime) StreamLogs(context.Context, string, runtime.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeModelRuntime) SubscribeEvents(context.Context) (<-chan runtime.Event, <-chan error, error) {
	events := make(chan runtime.Event)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs, nil
}

func (f *fakeModelRuntime) EnsurePluginBridge(context.Context) (string, error) {
	return "smoothnas-plugins", nil
}

func (f *fakeModelRuntime) InspectContainerBridgeIP(context.Context, string) (string, error) {
	return "172.28.0.2", nil
}

// TestDetachRequest_SurvivesClientDisconnect guards the fix for long-running
// lifecycle ops being cancelled when the HTTP client (or the nginx proxy in
// front of tierd) disconnects: detachRequest must drop the request's
// cancellation/deadline while preserving request-scoped values, so a
// materialise/install/scale runs to completion instead of stranding the plugin
// in a partial state.
func TestDetachRequest_SurvivesClientDisconnect(t *testing.T) {
	type ctxKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-42"))
	r := httptest.NewRequest(http.MethodPost, "/api/plugins/install", nil).WithContext(parent)

	detached := detachRequest(r)

	// Client/proxy gives up and disconnects.
	cancel()

	if err := detached.Err(); err != nil {
		t.Fatalf("detached context cancelled by client disconnect: %v", err)
	}
	select {
	case <-detached.Done():
		t.Fatal("detached context Done() fired after client disconnect")
	default:
	}
	if _, ok := detached.Deadline(); ok {
		t.Fatal("detached context unexpectedly carries a deadline")
	}
	if got := detached.Value(ctxKey{}); got != "req-42" {
		t.Fatalf("request-scoped value not preserved: got %v, want req-42", got)
	}
}
