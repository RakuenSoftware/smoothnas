package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// localPluginsFS holds first-party plugins authored IN this repo, one directory
// per plugin under localplugins/, each with a single compose (or manifest) YAML.
// Unlike the bundled catalog snapshot (catalogdata/, synced from external plugin
// release repos), these have NO dedicated repo and NO release to sync: the file
// in this tree IS the source of truth. tierd serves them as a "local" catalog
// source so they appear in the Install wizard alongside repo-backed plugins.
//
//go:embed localplugins
var localPluginsFS embed.FS

// catalogSourceLocal marks a catalog entry served from the in-tree localplugins
// tree (distinct from "builtin" = synced repo snapshot, "github" = live fetch).
const catalogSourceLocal = "local"

// The embedded tree never changes at runtime, so it is parsed + validated once,
// lazily, and cached.
var (
	localPluginsOnce sync.Once
	localPluginsList []*pluginCatalogLatestResponse
	localPluginsErr  error
)

func loadLocalPlugins() ([]*pluginCatalogLatestResponse, error) {
	localPluginsOnce.Do(func() {
		localPluginsList, localPluginsErr = parseLocalPlugins(localPluginsFS)
	})
	return localPluginsList, localPluginsErr
}

// parseLocalPlugins reads every localplugins/<id>/<file>.yaml and runs it
// through the SAME ParseManifest + ValidateManifest the install path uses, so a
// malformed in-tree plugin fails loudly (a test exercises this over the real
// tree) rather than reaching an operator's install. Each plugin becomes one
// catalog response with source=local and no repo/release (there is none).
func parseLocalPlugins(fsys fs.FS) ([]*pluginCatalogLatestResponse, error) {
	dirs, err := fs.ReadDir(fsys, "localplugins")
	if err != nil {
		return nil, fmt.Errorf("read localplugins: %w", err)
	}
	var out []*pluginCatalogLatestResponse
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		entries, err := fs.ReadDir(fsys, path.Join("localplugins", id))
		if err != nil {
			return nil, fmt.Errorf("read local plugin %s: %w", id, err)
		}
		var fname string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if n := e.Name(); strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
				fname = n
				break
			}
		}
		if fname == "" {
			return nil, fmt.Errorf("local plugin %q: no .yaml/.yml file", id)
		}
		body, err := fs.ReadFile(fsys, path.Join("localplugins", id, fname))
		if err != nil {
			return nil, fmt.Errorf("read local plugin %s/%s: %w", id, fname, err)
		}
		manifest, err := plugin.ParseManifest(body)
		if err != nil {
			return nil, fmt.Errorf("parse local plugin %s/%s: %w", id, fname, err)
		}
		if err := plugin.ValidateManifest(manifest); err != nil {
			return nil, fmt.Errorf("validate local plugin %s/%s: %w", id, fname, err)
		}
		out = append(out, &pluginCatalogLatestResponse{
			// No repo/tag/release: an in-tree plugin has none. The console
			// renders the card from the parsed manifest's own metadata.
			Manifests: []pluginCatalogManifest{{
				AssetName:    fname,
				ManifestYAML: string(body),
				Manifest:     manifest,
			}},
			Source: catalogSourceLocal,
		})
	}
	// Deterministic order (by plugin name) for stable UI + snapshots.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Manifests[0].Manifest.Metadata.Name < out[j].Manifests[0].Manifest.Metadata.Name
	})
	return out, nil
}

// catalogLocal serves GET /api/plugins/catalog/local: the in-tree plugins as a
// JSON array of catalog responses (source=local). Always available, never
// depends on GitHub.
func (h *PluginsHandler) catalogLocal(w http.ResponseWriter, _ *http.Request) {
	list, err := loadLocalPlugins()
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(list)
}
