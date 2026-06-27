package api

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"testing"
)

// TestPluginsAPI_CatalogValidatesAimeeLive ingests the real published
// smoothnas-plugin-aimee catalog from api.github.com end to end. Network-gated
// behind SMOOTHNAS_LIVE_CATALOG_TEST=1 so offline CI is unaffected.
func TestPluginsAPI_CatalogValidatesAimeeLive(t *testing.T) {
	if os.Getenv("SMOOTHNAS_LIVE_CATALOG_TEST") != "1" {
		t.Skip("set SMOOTHNAS_LIVE_CATALOG_TEST=1 to hit live GitHub")
	}
	h, _ := newPluginsHandlerForTest(t)

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/latest?repo=RakuenSoftware/smoothnas-plugin-aimee", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got pluginCatalogLatestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Manifests) != 3 {
		t.Fatalf("manifests = %d, want 3: %#v", len(got.Manifests), got.Manifests)
	}
	names := make([]string, 0, 3)
	for _, m := range got.Manifests {
		if m.Manifest == nil {
			t.Fatalf("asset %q parsed nil", m.AssetName)
		}
		names = append(names, m.Manifest.Metadata.Name)
	}
	sort.Strings(names)
	want := []string{"aimee-combined", "aimee-kb", "aimee-server"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("live plugin names = %v, want %v", names, want)
		}
	}
	t.Logf("LIVE: ingested tag=%s repo=%s plugins=%v", got.TagName, got.Repo, names)
}
