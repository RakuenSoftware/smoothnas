package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// TestPluginsAPI_CatalogValidatesAimeeThreePlugins drives SmoothNAS's real
// catalog-ingestion path (catalogLatest -> fetchLatestPluginRelease ->
// plugin.ParseManifest -> plugin.ValidateManifest) over the three published
// aimee plugin manifests (aimee-server, aimee-kb, aimee-combined), using the
// exact released YAML bytes as offline fixtures. This proves SmoothNAS can
// ingest all three from the smoothnas-plugin-aimee catalog and that each one
// parses and validates.
func TestPluginsAPI_CatalogValidatesAimeeThreePlugins(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)

	server := readManifestFixture(t, "aimee-server.yaml")
	kb := readManifestFixture(t, "aimee-kb.yaml")
	combined := readManifestFixture(t, "aimee-combined.yaml")

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/Community/smoothnas-plugin-demo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "SmoothNAS" {
			t.Fatalf("User-Agent = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.1.1",
			"html_url": "https://github.com/Community/smoothnas-plugin-demo/releases/tag/v0.1.1",
			"assets": []map[string]any{
				{"name": "smoothnas-plugin-aimee-server.yaml", "browser_download_url": srv.URL + "/assets/server.yaml"},
				{"name": "smoothnas-plugin-aimee-kb.yaml", "browser_download_url": srv.URL + "/assets/kb.yaml"},
				{"name": "smoothnas-plugin-aimee-combined.yaml", "browser_download_url": srv.URL + "/assets/combined.yaml"},
				{"name": "release-notes.txt", "browser_download_url": srv.URL + "/assets/notes.txt"},
			},
		})
	})
	mux.HandleFunc("/assets/server.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(server)) })
	mux.HandleFunc("/assets/kb.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(kb)) })
	mux.HandleFunc("/assets/combined.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(combined)) })
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
	if got.Repo != "Community/smoothnas-plugin-demo" {
		t.Fatalf("repo = %q", got.Repo)
	}
	if got.TagName != "v0.1.1" {
		t.Fatalf("tag = %q", got.TagName)
	}
	// The non-YAML release-notes.txt asset must be filtered out; exactly the
	// three plugin manifests survive.
	if len(got.Manifests) != 3 {
		t.Fatalf("manifests = %d, want 3 (asset filter/parse/validate dropped one): %#v", len(got.Manifests), got.Manifests)
	}

	gotNames := make([]string, 0, 3)
	for _, m := range got.Manifests {
		if m.Manifest == nil {
			t.Fatalf("asset %q parsed to nil manifest", m.AssetName)
		}
		// Every ingested manifest already passed ParseManifest + ValidateManifest
		// inside fetchLatestPluginRelease; assert the invariants it guarantees.
		if m.Manifest.APIVersion != "smoothnas.io/v1" {
			t.Fatalf("asset %q apiVersion = %q", m.AssetName, m.Manifest.APIVersion)
		}
		if m.Manifest.Kind != "Plugin" {
			t.Fatalf("asset %q kind = %q", m.AssetName, m.Manifest.Kind)
		}
		if !strings.Contains(m.ManifestYAML, "apiVersion: smoothnas.io/v1") {
			t.Fatalf("asset %q YAML missing apiVersion", m.AssetName)
		}
		gotNames = append(gotNames, m.Manifest.Metadata.Name)
	}

	sort.Strings(gotNames)
	want := []string{"aimee-combined", "aimee-kb", "aimee-server"}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("ingested plugin names = %v, want %v", gotNames, want)
		}
	}
}
