package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

const (
	defaultPluginCatalogAPIBaseURL = "https://api.github.com"
	maxPluginManifestBytes         = 1 << 20
)

// Catalog sources. "builtin" is the first-party snapshot bundled with the
// appliance (offline, can't be rate-limited); "github" is a live release fetch.
const (
	catalogSourceBuiltin = "builtin"
	catalogSourceGitHub  = "github"
)

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubLatestRelease struct {
	TagName string               `json:"tag_name"`
	HTMLURL string               `json:"html_url"`
	Assets  []githubReleaseAsset `json:"assets"`
}

type pluginCatalogManifest struct {
	AssetName    string           `json:"assetName"`
	DownloadURL  string           `json:"downloadUrl"`
	ManifestYAML string           `json:"manifestYaml"`
	Manifest     *plugin.Manifest `json:"manifest"`
}

type pluginCatalogLatestResponse struct {
	Repo       string                  `json:"repo"`
	TagName    string                  `json:"tagName"`
	ReleaseURL string                  `json:"releaseUrl"`
	Manifests  []pluginCatalogManifest `json:"manifests"`
	// Source is "builtin" when served from the bundled snapshot or "github"
	// when fetched live. Lets the UI badge bundled vs community entries.
	Source string `json:"source,omitempty"`
}

func (h *PluginsHandler) catalogLatest(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	owner, name, ok := parseGitHubRepo(repo)
	if !ok {
		jsonErrorCoded(w, "repo must be owner/name", http.StatusBadRequest, "plugins.catalog.repo_invalid")
		return
	}

	// First-party plugins resolve from the snapshot bundled with the appliance
	// so the catalog works offline and can never be blocked by GitHub's
	// unauthenticated rate limit. GitHub is consulted only for non-bundled
	// (third-party) repos; freshness for bundled repos is handled separately
	// (a background refresh — see catalogLatestForBundled).
	if resp := h.catalogLatestForBundled(owner + "/" + name); resp != nil {
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	resp, err := h.fetchLatestPluginRelease(ctx, owner, name)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadGateway, "plugins.catalog.fetch_failed")
		return
	}
	resp.Source = catalogSourceGitHub
	_ = json.NewEncoder(w).Encode(resp)
}

// apiBaseURL is the GitHub API base the catalog fetches from — the configured
// override (tests point it at a local server) or the public default.
func (h *PluginsHandler) apiBaseURL() string {
	if base := strings.TrimSpace(h.catalogAPIBaseURL); base != "" {
		return base
	}
	return defaultPluginCatalogAPIBaseURL
}

// sameHost reports whether rawURL targets the same host as base. Used to scope
// the catalog credential to the API host and keep it off asset-download hosts.
func sameHost(rawURL, base string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	return u.Host == b.Host
}

func (h *PluginsHandler) fetchLatestPluginRelease(ctx context.Context, owner, name string) (*pluginCatalogLatestResponse, error) {
	releaseURL, err := url.JoinPath(h.apiBaseURL(), "repos", owner, name, "releases", "latest")
	if err != nil {
		return nil, fmt.Errorf("build GitHub release URL: %w", err)
	}

	var release githubLatestRelease
	if err := h.fetchCatalogJSON(ctx, releaseURL, &release); err != nil {
		return nil, fmt.Errorf("fetch latest release for %s/%s: %w", owner, name, err)
	}

	assets := make([]githubReleaseAsset, 0, len(release.Assets))
	for _, asset := range release.Assets {
		if isPluginManifestAsset(asset.Name) {
			assets = append(assets, asset)
		}
	}
	sort.Slice(assets, func(i, j int) bool {
		return manifestAssetSortKey(assets[i].Name) < manifestAssetSortKey(assets[j].Name)
	})
	if len(assets) == 0 {
		return nil, fmt.Errorf("latest release for %s/%s has no smoothnas-plugin YAML assets", owner, name)
	}

	out := &pluginCatalogLatestResponse{
		Repo:       owner + "/" + name,
		TagName:    release.TagName,
		ReleaseURL: release.HTMLURL,
		Manifests:  make([]pluginCatalogManifest, 0, len(assets)),
	}
	for _, asset := range assets {
		raw, err := h.fetchCatalogText(ctx, asset.BrowserDownloadURL, maxPluginManifestBytes)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", asset.Name, err)
		}
		manifest, err := plugin.ParseManifest([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", asset.Name, err)
		}
		if err := plugin.ValidateManifest(manifest); err != nil {
			return nil, fmt.Errorf("validate %s: %w", asset.Name, err)
		}
		out.Manifests = append(out.Manifests, pluginCatalogManifest{
			AssetName:    asset.Name,
			DownloadURL:  asset.BrowserDownloadURL,
			ManifestYAML: raw,
			Manifest:     manifest,
		})
	}
	return out, nil
}

func (h *PluginsHandler) fetchCatalogJSON(ctx context.Context, rawURL string, out any) error {
	resp, err := h.fetchCatalog(ctx, rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return h.catalogHTTPError(rawURL, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (h *PluginsHandler) fetchCatalogText(ctx context.Context, rawURL string, maxBytes int64) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("asset download URL is empty")
	}
	resp, err := h.fetchCatalog(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", h.catalogHTTPError(rawURL, resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("manifest exceeds %d bytes", maxBytes)
	}
	return string(data), nil
}

// catalogHTTPError turns a non-2xx catalog response into an error. A GitHub
// rate-limit rejection (403/429 with X-RateLimit-Remaining: 0) is otherwise an
// opaque "403 Forbidden"; when no token is configured, say so and point at the
// fix, since the shared unauthenticated budget (60/hr per IP) is the usual cause.
func (h *PluginsHandler) catalogHTTPError(rawURL string, resp *http.Response) error {
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if h.catalogToken == "" {
			return fmt.Errorf("%s: GitHub API rate limit exhausted (unauthenticated: 60 requests/hr per IP). "+
				"Set SMOOTHNAS_GITHUB_TOKEN to a read-only token to raise the limit to 5000/hr", rawURL)
		}
		return fmt.Errorf("%s: GitHub API rate limit exhausted even with a token; retry after the reset window", rawURL)
	}
	return fmt.Errorf("%s returned %s", rawURL, resp.Status)
}

func (h *PluginsHandler) fetchCatalog(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "SmoothNAS")

	// Authenticate ONLY the GitHub API host (the rate-limited releases/latest
	// call). Release-asset downloads resolve to a different, pre-signed host
	// (objects.githubusercontent.com); attaching a second credential there can
	// break the signed request and isn't needed for public assets.
	if h.catalogToken != "" && sameHost(rawURL, h.apiBaseURL()) {
		req.Header.Set("Authorization", "Bearer "+h.catalogToken)
	}

	client := h.catalogHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func parseGitHubRepo(repo string) (owner, name string, ok bool) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validGitHubPathPart(parts[0]) || !validGitHubPathPart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func validGitHubPathPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func isPluginManifestAsset(name string) bool {
	base := path.Base(name)
	lower := strings.ToLower(base)
	if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
		return false
	}
	return lower == "smoothnas-plugin.yaml" ||
		lower == "smoothnas-plugin.yml" ||
		strings.HasPrefix(lower, "smoothnas-plugin-")
}

func manifestAssetSortKey(name string) string {
	lower := strings.ToLower(path.Base(name))
	switch lower {
	case "smoothnas-plugin.yaml", "smoothnas-plugin.yml":
		return "00-" + lower
	default:
		return "10-" + lower
	}
}
