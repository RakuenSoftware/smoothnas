package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBuiltinCatalog_SnapshotLoadsAndValidates locks the integrity of the
// bundled snapshot: every repo in index.json parses, and every manifest passes
// ParseManifest + ValidateManifest. A bad `scripts/sync-plugin-catalog.sh` run
// (truncated asset, schema drift) fails here instead of at an operator's
// install.
func TestBuiltinCatalog_SnapshotLoadsAndValidates(t *testing.T) {
	byRepo, err := parseBuiltinCatalog(builtinCatalogFS)
	if err != nil {
		t.Fatalf("parse bundled catalog: %v", err)
	}
	if len(byRepo) == 0 {
		t.Fatal("bundled catalog is empty")
	}
	// The five first-party repos must be present (kept in sync with the
	// frontend catalog list + scripts/sync-plugin-catalog.sh).
	for _, repo := range []string{
		"RakuenSoftware/smoothnas-plugin-aimee",
		"RakuenSoftware/smoothnas-plugin-gh-runner",
		"RakuenSoftware/smoothnas-plugin-llama-cpp",
		"RakuenSoftware/smoothnas-plugin-vllm",
		"RakuenSoftware/smoothnas-plugin-wolf",
	} {
		resp := builtinCatalogFor(repo)
		if resp == nil {
			t.Fatalf("bundled catalog missing %s", repo)
		}
		if resp.Source != catalogSourceBuiltin {
			t.Fatalf("%s source = %q, want builtin", repo, resp.Source)
		}
		if len(resp.Manifests) == 0 {
			t.Fatalf("%s has no manifests", repo)
		}
		for _, m := range resp.Manifests {
			// A bundled manifest is valid either as native (apiVersion+kind) or
			// as a plain-compose plugin (compose-migration: named, IsCompose).
			nativeOK := m.Manifest != nil && m.Manifest.APIVersion == "smoothnas.io/v1" && m.Manifest.Kind == "Plugin"
			composeOK := m.Manifest != nil && m.Manifest.IsCompose() && m.Manifest.Metadata.Name != ""
			if !nativeOK && !composeOK {
				t.Fatalf("%s asset %q failed manifest invariants", repo, m.AssetName)
			}
			if m.ManifestYAML == "" {
				t.Fatalf("%s asset %q has empty YAML", repo, m.AssetName)
			}
		}
	}

	// The aimee repo must include the two GPU LLM tiers.
	aimee := builtinCatalogFor("RakuenSoftware/smoothnas-plugin-aimee")
	names := map[string]bool{}
	for _, m := range aimee.Manifests {
		names[m.Manifest.Metadata.Name] = true
	}
	for _, want := range []string{"aimee-llm-gpu-small", "aimee-llm-gpu-mid"} {
		if !names[want] {
			t.Fatalf("aimee bundle missing %s (have %v)", want, names)
		}
	}
}

// TestBuiltinCatalog_ServedWithoutNetwork proves a bundled repo is served from
// the embedded snapshot with NO GitHub call: the HTTP client is rigged to fail
// every request, yet the catalog resolves with source=builtin. This is the
// core guarantee — a rate-limited / offline appliance can still install its own
// plugins.
func TestBuiltinCatalog_ServedWithoutNetwork(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	h.catalogHTTPClient = &http.Client{Transport: failTransport{t}}
	h.catalogAPIBaseURL = "http://127.0.0.1:0" // unroutable; must never be hit

	rr := doJSON(t, &routeHandler{h}, http.MethodGet,
		"/api/plugins/catalog/latest?repo=RakuenSoftware/smoothnas-plugin-aimee", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got pluginCatalogLatestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != catalogSourceBuiltin {
		t.Fatalf("source = %q, want builtin", got.Source)
	}
	if got.Repo != "RakuenSoftware/smoothnas-plugin-aimee" || len(got.Manifests) == 0 {
		t.Fatalf("unexpected response: %+v", got)
	}
}

// TestBuiltinCatalog_NonBundledFallsThroughToGitHub confirms a repo that isn't
// in the bundled snapshot still hits the live GitHub path and is stamped
// source=github.
func TestBuiltinCatalog_NonBundledFallsThroughToGitHub(t *testing.T) {
	kb := readManifestFixture(t, "aimee-kb.yaml")
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/Third/party-plugin/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.0.0",
			"html_url": "https://github.com/Third/party-plugin/releases/tag/v1.0.0",
			"assets": []map[string]any{
				{"name": "smoothnas-plugin.yaml", "browser_download_url": srv.URL + "/a.yaml"},
			},
		})
	})
	mux.HandleFunc("/a.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(kb)) })
	srv = httptest.NewServer(mux)
	defer srv.Close()

	h, _ := newPluginsHandlerForTest(t)
	h.catalogAPIBaseURL = srv.URL
	h.catalogHTTPClient = srv.Client()

	rr := doJSON(t, &routeHandler{h}, http.MethodGet,
		"/api/plugins/catalog/latest?repo=Third/party-plugin", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got pluginCatalogLatestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != catalogSourceGitHub {
		t.Fatalf("source = %q, want github", got.Source)
	}
	if got.TagName != "v1.0.0" {
		t.Fatalf("tag = %q", got.TagName)
	}
}

// failTransport fails every request, standing in for a rate-limited / offline
// network so a test can prove the bundled path makes no HTTP call.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("unexpected outbound HTTP request for a bundled catalog repo")
	return nil, http.ErrUseLastResponse
}
