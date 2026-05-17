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
}

func (h *PluginsHandler) catalogLatest(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	owner, name, ok := parseGitHubRepo(repo)
	if !ok {
		jsonErrorCoded(w, "repo must be owner/name", http.StatusBadRequest, "plugins.catalog.repo_invalid")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	resp, err := h.fetchLatestPluginRelease(ctx, owner, name)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadGateway, "plugins.catalog.fetch_failed")
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *PluginsHandler) fetchLatestPluginRelease(ctx context.Context, owner, name string) (*pluginCatalogLatestResponse, error) {
	base := h.catalogAPIBaseURL
	if strings.TrimSpace(base) == "" {
		base = defaultPluginCatalogAPIBaseURL
	}
	releaseURL, err := url.JoinPath(base, "repos", owner, name, "releases", "latest")
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
		return fmt.Errorf("%s returned %s", rawURL, resp.Status)
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
		return "", fmt.Errorf("%s returned %s", rawURL, resp.Status)
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

func (h *PluginsHandler) fetchCatalog(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "SmoothNAS")

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
