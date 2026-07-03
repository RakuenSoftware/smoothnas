package api

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPluginsAPI_CatalogValidatesAimeeLive ingests the real published
// smoothnas-plugin-aimee catalog from api.github.com end to end. Network-gated
// behind SMOOTHNAS_LIVE_CATALOG_TEST=1 so offline CI is unaffected.
//
// The HTTP handler now serves aimee from the bundled snapshot, so this exercises
// the live GitHub fetch path directly (fetchLatestPluginRelease) — that path
// still runs for third-party repos and for the background freshness refresh, so
// it must keep working against the real release.
func TestPluginsAPI_CatalogValidatesAimeeLive(t *testing.T) {
	if os.Getenv("SMOOTHNAS_LIVE_CATALOG_TEST") != "1" {
		t.Skip("set SMOOTHNAS_LIVE_CATALOG_TEST=1 to hit live GitHub")
	}
	h, _ := newPluginsHandlerForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	got, err := h.fetchLatestPluginRelease(ctx, "RakuenSoftware", "smoothnas-plugin-aimee")
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	names := map[string]bool{}
	for _, m := range got.Manifests {
		if m.Manifest == nil {
			t.Fatalf("asset %q parsed nil", m.AssetName)
		}
		names[m.Manifest.Metadata.Name] = true
	}
	// The live release must at least carry the three core plugins (it also
	// ships the aimee-llm GPU tiers now).
	for _, want := range []string{"aimee-combined", "aimee-kb", "aimee-server"} {
		if !names[want] {
			t.Fatalf("live release missing %s (have %v)", want, names)
		}
	}
	t.Logf("LIVE: ingested tag=%s repo=%s plugins=%v", got.TagName, got.Repo, names)
}
