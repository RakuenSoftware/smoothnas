package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// builtinCatalogFS holds the first-party plugin-catalog snapshot shipped with
// the appliance: an index.json plus one manifest set per repo. Regenerate it
// with scripts/sync-plugin-catalog.sh. This is the guaranteed-installable floor
// the "Install plugins" catalog falls back to when GitHub is unreachable or
// rate-limited; see docs/proposals/pending/plugins-12-bundled-catalog.md.
//
//go:embed catalogdata
var builtinCatalogFS embed.FS

// builtinCatalogIndex is the on-disk index.json schema.
type builtinCatalogIndex struct {
	Repositories []builtinCatalogRepo `json:"repositories"`
}

type builtinCatalogRepo struct {
	ID         string   `json:"id"`
	Repo       string   `json:"repo"`
	TagName    string   `json:"tagName"`
	ReleaseURL string   `json:"releaseUrl"`
	Manifests  []string `json:"manifests"`
}

// The embedded snapshot never changes at runtime, so it is parsed + validated
// exactly once, lazily, and cached keyed by lowercased "owner/name".
var (
	builtinCatalogOnce   sync.Once
	builtinCatalogByRepo map[string]*pluginCatalogLatestResponse
	builtinCatalogErr    error
)

func loadBuiltinCatalog() (map[string]*pluginCatalogLatestResponse, error) {
	builtinCatalogOnce.Do(func() {
		builtinCatalogByRepo, builtinCatalogErr = parseBuiltinCatalog(builtinCatalogFS)
	})
	return builtinCatalogByRepo, builtinCatalogErr
}

// parseBuiltinCatalog reads catalogdata/index.json and every referenced
// manifest, running each through the SAME ParseManifest + ValidateManifest the
// live GitHub path uses, so a malformed bundled manifest fails loudly (a test
// exercises this over the real snapshot) rather than reaching an install.
func parseBuiltinCatalog(fsys fs.FS) (map[string]*pluginCatalogLatestResponse, error) {
	raw, err := fs.ReadFile(fsys, "catalogdata/index.json")
	if err != nil {
		return nil, fmt.Errorf("read builtin catalog index: %w", err)
	}
	var idx builtinCatalogIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse builtin catalog index: %w", err)
	}
	out := make(map[string]*pluginCatalogLatestResponse, len(idx.Repositories))
	for _, repo := range idx.Repositories {
		owner, name, ok := parseGitHubRepo(repo.Repo)
		if !ok {
			return nil, fmt.Errorf("builtin catalog: invalid repo %q", repo.Repo)
		}
		canonical := owner + "/" + name
		if len(repo.Manifests) == 0 {
			return nil, fmt.Errorf("builtin catalog %s: no manifests listed", canonical)
		}

		// Mirror the GitHub path's asset ordering (base manifest first).
		names := append([]string(nil), repo.Manifests...)
		sort.Slice(names, func(i, j int) bool {
			return manifestAssetSortKey(names[i]) < manifestAssetSortKey(names[j])
		})

		resp := &pluginCatalogLatestResponse{
			Repo:       canonical,
			TagName:    repo.TagName,
			ReleaseURL: repo.ReleaseURL,
			Manifests:  make([]pluginCatalogManifest, 0, len(names)),
			Source:     catalogSourceBuiltin,
		}
		for _, mname := range names {
			if !isPluginManifestAsset(mname) {
				return nil, fmt.Errorf("builtin catalog %s: %q is not a plugin manifest asset", canonical, mname)
			}
			body, err := fs.ReadFile(fsys, path.Join("catalogdata", repo.ID, mname))
			if err != nil {
				return nil, fmt.Errorf("read builtin manifest %s/%s: %w", repo.ID, mname, err)
			}
			manifest, err := plugin.ParseManifest(body)
			if err != nil {
				return nil, fmt.Errorf("parse builtin manifest %s/%s: %w", repo.ID, mname, err)
			}
			if err := plugin.ValidateManifest(manifest); err != nil {
				return nil, fmt.Errorf("validate builtin manifest %s/%s: %w", repo.ID, mname, err)
			}
			resp.Manifests = append(resp.Manifests, pluginCatalogManifest{
				AssetName:    mname,
				DownloadURL:  "", // bundled — no external URL
				ManifestYAML: string(body),
				Manifest:     manifest,
			})
		}
		key := strings.ToLower(canonical)
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("builtin catalog: duplicate repo %q", canonical)
		}
		out[key] = resp
	}
	return out, nil
}

// builtinCatalogFor returns the bundled snapshot for a repo (case-insensitive
// "owner/name"), or nil if the repo is not first-party/bundled. The returned
// value is a shallow copy so callers may set Source/merge freshness without
// mutating the shared cache; the Manifests entries are treated read-only.
func builtinCatalogFor(repo string) *pluginCatalogLatestResponse {
	byRepo, err := loadBuiltinCatalog()
	if err != nil {
		return nil
	}
	src := byRepo[strings.ToLower(strings.TrimSpace(repo))]
	if src == nil {
		return nil
	}
	cp := *src
	return &cp
}

// catalogLatestForBundled returns the response to serve for a bundled repo, or
// nil if the repo is not bundled (so the caller falls back to GitHub). Slice 1
// serves the embedded snapshot directly; later slices layer a freshness cache
// on top here without touching the handler.
func (h *PluginsHandler) catalogLatestForBundled(repo string) *pluginCatalogLatestResponse {
	return builtinCatalogFor(repo)
}
