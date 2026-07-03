package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPluginsAPI_CatalogTokenScopedToAPIHost proves the optional catalog
// credential is (a) sent as a Bearer header on the GitHub API host — the
// rate-limited releases/latest call that returns 403 unauthenticated — and
// (b) NOT sent on release-asset downloads, which resolve to a different,
// pre-signed host where a second credential can break the request.
func TestPluginsAPI_CatalogTokenScopedToAPIHost(t *testing.T) {
	const token = "ghp_testtoken123"
	kb := readManifestFixture(t, "aimee-kb.yaml")

	// Asset host: a distinct server (distinct host:port) standing in for
	// objects.githubusercontent.com. It must receive NO Authorization header.
	var assetSawAuth bool
	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			assetSawAuth = true
		}
		_, _ = w.Write([]byte(kb))
	}))
	defer assetSrv.Close()

	// API host: must receive the Bearer credential.
	var apiSawAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/Community/smoothnas-plugin-demo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		apiSawAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.2.5",
			"html_url": "https://github.com/Community/smoothnas-plugin-demo/releases/tag/v0.2.5",
			"assets": []map[string]any{
				{"name": "smoothnas-plugin-aimee-kb.yaml", "browser_download_url": assetSrv.URL + "/download/kb.yaml"},
			},
		})
	})
	apiSrv := httptest.NewServer(mux)
	defer apiSrv.Close()

	h, _ := newPluginsHandlerForTest(t)
	h.catalogAPIBaseURL = apiSrv.URL
	h.catalogHTTPClient = apiSrv.Client()
	h.catalogToken = token

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/latest?repo=Community/smoothnas-plugin-demo", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	if apiSawAuth != "Bearer "+token {
		t.Fatalf("API host Authorization = %q, want %q", apiSawAuth, "Bearer "+token)
	}
	if assetSawAuth {
		t.Fatalf("asset host received an Authorization header; the credential must not leak to asset downloads")
	}
}

// TestPluginsAPI_CatalogRateLimitErrorHintsToken confirms a 403 with
// X-RateLimit-Remaining: 0 and no token configured produces an actionable
// error naming the token env var, not an opaque "403 Forbidden".
func TestPluginsAPI_CatalogRateLimitErrorHintsToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/Community/smoothnas-plugin-demo/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h, _ := newPluginsHandlerForTest(t)
	h.catalogAPIBaseURL = srv.URL
	h.catalogHTTPClient = srv.Client()

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/latest?repo=Community/smoothnas-plugin-demo", nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"rate limit", "SMOOTHNAS_GITHUB_TOKEN"} {
		if !strings.Contains(body, want) {
			t.Fatalf("error body %q missing %q", body, want)
		}
	}
}

// TestPluginsAPI_CatalogNoTokenNoAuthHeader confirms the default path is
// unchanged: with no token configured, no Authorization header is sent.
func TestPluginsAPI_CatalogNoTokenNoAuthHeader(t *testing.T) {
	var sawAuth bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/Community/smoothnas-plugin-demo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.2.5",
			"assets":   []map[string]any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h, _ := newPluginsHandlerForTest(t)
	h.catalogAPIBaseURL = srv.URL
	h.catalogHTTPClient = srv.Client()
	// catalogToken deliberately left empty.

	// No manifest assets -> fetch fails with a 502, but the request still
	// reaches the API host, which is all this test inspects.
	_ = doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/latest?repo=Community/smoothnas-plugin-demo", nil)
	if sawAuth {
		t.Fatalf("Authorization header sent with no token configured")
	}
}
