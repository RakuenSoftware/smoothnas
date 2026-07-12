package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// TestLocalPlugins_TreeLoadsAndValidates locks the integrity of the in-tree
// localplugins/ tree: every plugin parses and passes ParseManifest +
// ValidateManifest. A malformed in-tree plugin fails here instead of at an
// operator's install.
func TestLocalPlugins_TreeLoadsAndValidates(t *testing.T) {
	list, err := parseLocalPlugins(localPluginsFS)
	if err != nil {
		t.Fatalf("parse local plugins: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("no in-tree local plugins found")
	}
	byName := map[string]*pluginCatalogLatestResponse{}
	for _, resp := range list {
		if resp.Source != catalogSourceLocal {
			t.Fatalf("local plugin source = %q, want %q", resp.Source, catalogSourceLocal)
		}
		if len(resp.Manifests) != 1 {
			t.Fatalf("local plugin has %d manifests, want 1", len(resp.Manifests))
		}
		m := resp.Manifests[0]
		if m.Manifest == nil || m.Manifest.Metadata.Name == "" {
			t.Fatalf("local plugin %q has no manifest name", m.AssetName)
		}
		if m.ManifestYAML == "" {
			t.Fatalf("local plugin %q has empty YAML", m.AssetName)
		}
		byName[m.Manifest.Metadata.Name] = resp
	}

	// The plex plugin must be present, be a compose plugin, embed its UI at
	// /web, and expose a claim-token config field on install.
	plex := byName["plex"]
	if plex == nil {
		t.Fatalf("in-tree plugins missing plex (have %v)", keys(byName))
	}
	m := plex.Manifests[0].Manifest
	if !m.IsCompose() {
		t.Fatal("plex must be a plain-compose plugin")
	}
	if m.UI == nil || m.UI.Embed.Path != "/web" {
		t.Fatalf("plex UI embed = %+v, want path /web", m.UI)
	}
	if !hasConfigKey(m, "PLEX_CLAIM") {
		t.Fatal("plex must expose a PLEX_CLAIM config field so the claim token can be entered at install")
	}
}

// TestLocalPlugins_ServedWithoutNetwork proves the endpoint serves the in-tree
// plugins with no GitHub dependency and stamps source=local.
func TestLocalPlugins_ServedWithoutNetwork(t *testing.T) {
	h, _ := newPluginsHandlerForTest(t)
	h.catalogHTTPClient = &http.Client{Transport: failTransport{t}}
	h.catalogAPIBaseURL = "http://127.0.0.1:0" // unroutable; must never be hit

	rr := doJSON(t, &routeHandler{h}, http.MethodGet, "/api/plugins/catalog/local", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got []pluginCatalogLatestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("empty local catalog response")
	}
	found := false
	for _, resp := range got {
		if resp.Source != catalogSourceLocal {
			t.Fatalf("source = %q, want local", resp.Source)
		}
		for _, mf := range resp.Manifests {
			if mf.Manifest != nil && mf.Manifest.Metadata.Name == "plex" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("plex not served by /api/plugins/catalog/local")
	}
}

func keys(m map[string]*pluginCatalogLatestResponse) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hasConfigKey(m *plugin.Manifest, key string) bool {
	// Compose plugins carry the operator schema at the plugin level (from
	// x-smoothnas.config); native manifests carry it per-service.
	for _, f := range m.Config {
		if f.Key == key {
			return true
		}
	}
	for _, s := range m.Services {
		for _, f := range s.Config {
			if f.Key == key {
				return true
			}
		}
	}
	return false
}
