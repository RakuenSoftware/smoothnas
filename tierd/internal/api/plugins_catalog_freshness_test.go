package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTagIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.6", "v0.2.5", true},
		{"v0.3.0", "v0.2.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"0.2.6", "v0.2.5", true}, // leading v optional
		{"v0.2.5", "v0.2.5", false},
		{"v0.2.4", "v0.2.5", false},
		{"v0.2.5", "v0.2.6", false},
		{"latest", "v0.2.5", false},  // unparseable a -> floor wins
		{"v0.2.6", "nightly", false}, // unparseable b -> floor wins
		{"v0.2", "v0.2.0", false},    // not MAJOR.MINOR.PATCH
	}
	for _, c := range cases {
		if got := tagIsNewer(c.a, c.b); got != c.want {
			t.Errorf("tagIsNewer(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestBundledCatalog_ServesNewerCache proves the serve-newest-of-{builtin,cache}
// rule: a cached response with a strictly-newer tag is served (source github),
// while an older/equal cache leaves the embedded floor in place. Refresh stays
// disabled, so no background goroutine runs.
func TestBundledCatalog_ServesNewerCache(t *testing.T) {
	const repo = "RakuenSoftware/smoothnas-plugin-aimee"
	floor := builtinCatalogFor(repo)
	if floor == nil {
		t.Fatal("aimee not bundled")
	}

	validYAML := readManifestFixture(t, "aimee-kb.yaml")
	cache := func(t *testing.T, tag string) *PluginsHandler {
		h, _ := newPluginsHandlerForTest(t)
		blob, _ := json.Marshal(pluginCatalogLatestResponse{
			Repo: repo, TagName: tag, Source: catalogSourceGitHub,
			Manifests: []pluginCatalogManifest{{AssetName: "smoothnas-plugin.yaml", ManifestYAML: validYAML}},
		})
		if err := h.store.PutCatalogCache("rakuensoftware/smoothnas-plugin-aimee", tag, string(blob), 1); err != nil {
			t.Fatal(err)
		}
		return h
	}

	// Newer cache -> served.
	got := cache(t, "v99.0.0").catalogLatestForBundled(repo)
	if got.TagName != "v99.0.0" || got.Source != catalogSourceGitHub {
		t.Fatalf("newer cache not served: tag=%q source=%q", got.TagName, got.Source)
	}
	// Older cache -> embedded floor wins.
	got = cache(t, "v0.0.1").catalogLatestForBundled(repo)
	if got.TagName != floor.TagName || got.Source != catalogSourceBuiltin {
		t.Fatalf("stale cache displaced floor: tag=%q source=%q (floor=%q)", got.TagName, got.Source, floor.TagName)
	}
}

// TestBundledCatalog_RejectsInvalidCache proves the defense-in-depth guard: a
// cached row with a newer tag but an INVALID manifest (or wrong source) is not
// served — the validated embedded floor wins. Guards against a corrupt/
// hand-written plugin_catalog_cache row reaching the installer.
func TestBundledCatalog_RejectsInvalidCache(t *testing.T) {
	const repo = "RakuenSoftware/smoothnas-plugin-aimee"
	floor := builtinCatalogFor(repo)

	newerWith := func(t *testing.T, source, manifestYAML string) *pluginCatalogLatestResponse {
		h, _ := newPluginsHandlerForTest(t)
		blob, _ := json.Marshal(pluginCatalogLatestResponse{
			Repo: repo, TagName: "v99.0.0", Source: source,
			Manifests: []pluginCatalogManifest{{AssetName: "smoothnas-plugin.yaml", ManifestYAML: manifestYAML}},
		})
		if err := h.store.PutCatalogCache("rakuensoftware/smoothnas-plugin-aimee", "v99.0.0", string(blob), 1); err != nil {
			t.Fatal(err)
		}
		return h.catalogLatestForBundled(repo)
	}

	// Newer tag but a manifest that fails validation -> floor wins.
	got := newerWith(t, catalogSourceGitHub, "this: is not: a valid plugin manifest")
	if got.TagName != floor.TagName || got.Source != catalogSourceBuiltin {
		t.Fatalf("invalid cached manifest was served: tag=%q source=%q", got.TagName, got.Source)
	}

	// Newer tag, structurally valid manifest, but source != github (a row that
	// didn't come from a live fetch) -> floor wins.
	got = newerWith(t, catalogSourceBuiltin, readManifestFixture(t, "aimee-kb.yaml"))
	if got.TagName != floor.TagName || got.Source != catalogSourceBuiltin {
		t.Fatalf("non-github cache row was served: tag=%q source=%q", got.TagName, got.Source)
	}
}

// TestBundledCatalog_RefreshCachesLatest drives the synchronous refresh against
// a mock GitHub and asserts the fetched release is cached.
func TestBundledCatalog_RefreshCachesLatest(t *testing.T) {
	kb := readManifestFixture(t, "aimee-kb.yaml")
	srv := mockReleaseServer(t, "RakuenSoftware/smoothnas-plugin-aimee", "v99.1.0", kb)
	defer srv.Close()

	h, _ := newPluginsHandlerForTest(t)
	h.catalogAPIBaseURL = srv.URL
	h.catalogHTTPClient = srv.Client()

	h.refreshBundledCatalog(context.Background(), "RakuenSoftware/smoothnas-plugin-aimee")

	c, err := h.store.GetCatalogCache("rakuensoftware/smoothnas-plugin-aimee")
	if err != nil || c == nil {
		t.Fatalf("cache not written: %v", err)
	}
	if c.TagName != "v99.1.0" {
		t.Fatalf("cached tag = %q", c.TagName)
	}
}

// TestBundledCatalog_BackgroundRefresh exercises the full stale-while-revalidate
// loop: the first call serves the embedded floor immediately and kicks a
// background refresh; once it lands, a later call serves the newer cached
// version.
func TestBundledCatalog_BackgroundRefresh(t *testing.T) {
	const repo = "RakuenSoftware/smoothnas-plugin-aimee"
	kb := readManifestFixture(t, "aimee-kb.yaml")
	srv := mockReleaseServer(t, repo, "v99.2.0", kb)
	defer srv.Close()

	h, _ := newPluginsHandlerForTest(t)
	h.catalogAPIBaseURL = srv.URL
	h.catalogHTTPClient = srv.Client()
	h.catalogRefreshEnabled = true

	floor := builtinCatalogFor(repo)
	// First call: served from the floor, refresh kicked in the background.
	first := h.catalogLatestForBundled(repo)
	if first.TagName != floor.TagName {
		t.Fatalf("first call not floor: %q", first.TagName)
	}

	// Wait (bounded) for the background refresh to populate the cache.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, _ := h.store.GetCatalogCache("rakuensoftware/smoothnas-plugin-aimee")
		if c != nil && c.TagName == "v99.2.0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh never cached the newer release")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Next call serves the fresher cached version.
	second := h.catalogLatestForBundled(repo)
	if second.TagName != "v99.2.0" || second.Source != catalogSourceGitHub {
		t.Fatalf("second call not fresh: tag=%q source=%q", second.TagName, second.Source)
	}
}

// mockReleaseServer stands up a GitHub-API mock serving one release with a
// single manifest asset for the given repo/tag.
func mockReleaseServer(t *testing.T, repo, tag, manifestYAML string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"html_url": "https://github.com/" + repo + "/releases/tag/" + tag,
			"assets": []map[string]any{
				{"name": "smoothnas-plugin.yaml", "browser_download_url": srv.URL + "/a.yaml"},
			},
		})
	})
	mux.HandleFunc("/a.yaml", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(manifestYAML)) })
	srv = httptest.NewServer(mux)
	return srv
}
