package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

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

// bundledCatalogRefreshTTL bounds how often a bundled repo's background GitHub
// refresh runs: a repo cached more recently than this is considered fresh.
const bundledCatalogRefreshTTL = 6 * time.Hour

// catalogLatestForBundled returns the response to serve for a bundled repo, or
// nil if the repo is not bundled (so the caller falls back to GitHub).
//
// It serves the NEWER of the embedded snapshot (the offline floor) and the
// cached last-successful GitHub fetch (a fresher version, if online refresh has
// run). It then opportunistically triggers a background refresh when that cache
// is missing/stale. The returned response is always immediately available and
// never depends on GitHub being reachable.
func (h *PluginsHandler) catalogLatestForBundled(repo string) *pluginCatalogLatestResponse {
	builtin := builtinCatalogFor(repo)
	if builtin == nil {
		return nil
	}
	chosen := builtin

	var cached *plugin.CatalogCacheEntry
	if h.store != nil {
		cached, _ = h.store.GetCatalogCache(strings.ToLower(repo))
		if cached != nil && tagIsNewer(cached.TagName, builtin.TagName) {
			var fresh pluginCatalogLatestResponse
			// Re-validate every manifest before serving a cached response, so a
			// corrupt/hand-written plugin_catalog_cache row (backup restore, a
			// future write path) can never surface an invalid or unexpected
			// manifest — the same invariant the embedded floor enforces. On any
			// failure we keep the validated floor.
			if json.Unmarshal([]byte(cached.Response), &fresh) == nil &&
				fresh.Source == catalogSourceGitHub &&
				len(fresh.Manifests) > 0 &&
				catalogResponseManifestsValid(&fresh) {
				chosen = &fresh
			}
		}
	}

	h.triggerBundledRefresh(repo, cached)
	return chosen
}

// triggerBundledRefresh launches a best-effort background GitHub refresh for a
// bundled repo when refresh is enabled and the cache is missing or older than
// the TTL. A per-repo in-flight guard prevents concurrent requests from
// stampeding the same repo. It never blocks the caller.
func (h *PluginsHandler) triggerBundledRefresh(repo string, cached *plugin.CatalogCacheEntry) {
	if !h.catalogRefreshEnabled || h.store == nil {
		return
	}
	if cached != nil && h.now().Sub(time.Unix(cached.FetchedAt, 0)) < bundledCatalogRefreshTTL {
		return // fresh enough
	}
	key := strings.ToLower(repo)

	h.catalogRefreshMu.Lock()
	if h.catalogRefreshInflight == nil {
		h.catalogRefreshInflight = map[string]bool{}
	}
	if h.catalogRefreshInflight[key] {
		h.catalogRefreshMu.Unlock()
		return
	}
	h.catalogRefreshInflight[key] = true
	h.catalogRefreshMu.Unlock()

	// Best-effort and detached: not tied to a shutdown WaitGroup (like tierd's
	// other background sweeps). Bounded by the 20s timeout; if the store is
	// closed during shutdown the cache write simply returns an error, which
	// refreshBundledCatalog logs — never a panic.
	go func() {
		defer func() {
			h.catalogRefreshMu.Lock()
			delete(h.catalogRefreshInflight, key)
			h.catalogRefreshMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		h.refreshBundledCatalog(ctx, repo)
	}()
}

// refreshBundledCatalog fetches a bundled repo's latest release from GitHub and,
// on success, caches it. Best-effort: a rate-limited or offline fetch simply
// leaves the previous cache (or the embedded floor) in place.
func (h *PluginsHandler) refreshBundledCatalog(ctx context.Context, repo string) {
	owner, name, ok := parseGitHubRepo(repo)
	if !ok {
		return
	}
	resp, err := h.fetchLatestPluginRelease(ctx, owner, name)
	if err != nil {
		return
	}
	resp.Source = catalogSourceGitHub
	blob, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if err := h.store.PutCatalogCache(strings.ToLower(repo), resp.TagName, string(blob), h.now().Unix()); err != nil {
		// Best-effort: a persistent write failure keeps re-fetching every TTL;
		// log it so a stuck cache is diagnosable rather than silent.
		log.Printf("warn: plugin catalog cache write failed for %s: %v", repo, err)
	}
}

// catalogResponseManifestsValid reports whether every manifest in a (cached)
// catalog response re-parses and validates, matching the guarantee the embedded
// snapshot is loaded with.
func catalogResponseManifestsValid(resp *pluginCatalogLatestResponse) bool {
	for _, m := range resp.Manifests {
		manifest, err := plugin.ParseManifest([]byte(m.ManifestYAML))
		if err != nil {
			return false
		}
		if err := plugin.ValidateManifest(manifest); err != nil {
			return false
		}
	}
	return true
}

// now returns the handler's clock (injectable for tests).
func (h *PluginsHandler) now() time.Time {
	if h.nowFunc != nil {
		return h.nowFunc()
	}
	return time.Now()
}

// tagIsNewer reports whether release tag a is a strictly newer semver than b.
// Tags look like "v1.2.3"; a leading "v" is optional. If either tag is not a
// clean MAJOR.MINOR.PATCH, it returns false so the validated embedded floor
// wins — a fresher-but-unparseable cache never displaces the bundled snapshot.
func tagIsNewer(a, b string) bool {
	av, aok := parseSemver(a)
	bv, bok := parseSemver(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func parseSemver(tag string) ([3]int, bool) {
	var out [3]int
	s := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n := 0
		// Cap component width so n*10 can't overflow int (9 digits <= 999,999,999
		// < 2^31, safe even on 32-bit GOARCH). An over-long component is treated
		// as unparseable, so the validated floor wins.
		if p == "" || len(p) > 9 {
			return out, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}
