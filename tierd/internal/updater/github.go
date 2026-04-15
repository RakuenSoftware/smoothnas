package updater

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var httpClient = newHTTPClient()
var githubTokenFilePath = "/etc/tierd/github-token"

// newHTTPClient creates an HTTP client with a custom DNS resolver that bypasses
// the systemd-resolved stub. On systemd-networkd systems /etc/resolv.conf
// points to the stub at [::1]:53 which may not be running, so we read the
// actual upstream servers from /run/systemd/resolve/resolv.conf instead.
func newHTTPClient() *http.Client {
	servers := upstreamDNSServers()
	if len(servers) == 0 {
		return &http.Client{Timeout: 15 * time.Second}
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			var lastErr error
			for _, srv := range servers {
				conn, err := d.DialContext(ctx, "udp", srv+":53")
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}

	dialer := &net.Dialer{
		Timeout:  30 * time.Second,
		Resolver: resolver,
	}

	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
	}
}

// upstreamDNSServers reads the actual upstream DNS servers, preferring
// systemd-resolved's upstream list over the stub resolv.conf.
func upstreamDNSServers() []string {
	servers := parseNameservers("/run/systemd/resolve/resolv.conf")

	var filtered []string
	for _, s := range servers {
		ip := net.ParseIP(s)
		if ip != nil && !ip.IsLoopback() {
			filtered = append(filtered, s)
		}
	}

	if len(filtered) > 0 {
		return filtered
	}

	return []string{"1.1.1.1", "8.8.8.8"}
}

// parseNameservers extracts nameserver entries from a resolv.conf-style file.
func parseNameservers(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var servers []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "nameserver ") {
			servers = append(servers, strings.TrimSpace(strings.TrimPrefix(line, "nameserver ")))
		}
	}
	return servers
}

// ghRelease is the subset of the GitHub API release response we need.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Body        string    `json:"body"`
	PublishedAt string    `json:"published_at"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	URL                string `json:"url"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func fetchReleases(baseURL, repoOwner, repoName string, authenticated bool) ([]ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100", baseURL, repoOwner, repoName)

	req, err := newGitHubRequest(http.MethodGet, url, "application/vnd.github+json", authenticated)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return releases, nil
}

func newGitHubRequest(method, url, accept string, authenticated bool) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if authenticated {
		if token := readGitHubToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return req, nil
}

func newPublicGitHubRequest(method, url, accept string) (*http.Request, error) {
	return newGitHubRequest(method, url, accept, false)
}

func newAuthenticatedGitHubRequest(method, url, accept string) (*http.Request, error) {
	return newGitHubRequest(method, url, accept, true)
}

func readGitHubToken() string {
	data, err := os.ReadFile(githubTokenFilePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// fetchLatestRelease lists recent releases and returns the newest stable
// release — one whose tag starts with "v" and is not a prerelease.
// This avoids relying on GitHub's /releases/latest endpoint which can
// return the wrong release when non-semver tags (like testing-*) are present.
func fetchLatestRelease(baseURL, owner, repo string) (*ghRelease, error) {
	releases, err := fetchReleases(baseURL, owner, repo, false)
	if err != nil {
		return nil, err
	}
	for i := range releases {
		if !releases[i].Prerelease && strings.HasPrefix(releases[i].TagName, "v") {
			return &releases[i], nil
		}
	}
	return nil, fmt.Errorf("no stable release found")
}

// fetchLatestPrerelease lists recent releases and returns the newest testing
// prerelease — one whose tag starts with "testing-" and is marked as prerelease.
func fetchLatestPrerelease(baseURL, owner, repo string) (*ghRelease, error) {
	releases, err := fetchReleases(baseURL, owner, repo, false)
	if err != nil {
		return nil, err
	}

	for i := range releases {
		if releases[i].Prerelease && strings.HasPrefix(releases[i].TagName, "testing-") {
			return &releases[i], nil
		}
	}
	return nil, fmt.Errorf("no testing prerelease found")
}

// fetchLatestArtifactRelease returns the newest fork release that contains the
// three updater artifacts needed for direct installation. It prefers testing
// prereleases so the JBailes channel tracks the fork's testing line, and falls
// back to any artifact-bearing release if no such prerelease exists.
func fetchLatestArtifactRelease(baseURL, owner, repo string) (*ghRelease, error) {
	releases, err := fetchReleases(baseURL, owner, repo, true)
	if err != nil {
		return nil, err
	}
	for i := range releases {
		if !releases[i].Prerelease || !strings.HasPrefix(releases[i].TagName, "testing-") {
			continue
		}
		if findAssetURL(releases[i].Assets, "manifest.json") == "" {
			continue
		}
		if findAssetURL(releases[i].Assets, "tierd") == "" {
			continue
		}
		if findAssetURL(releases[i].Assets, "tierd-ui.tar.gz") == "" {
			continue
		}
		return &releases[i], nil
	}
	for i := range releases {
		if findAssetURL(releases[i].Assets, "manifest.json") == "" {
			continue
		}
		if findAssetURL(releases[i].Assets, "tierd") == "" {
			continue
		}
		if findAssetURL(releases[i].Assets, "tierd-ui.tar.gz") == "" {
			continue
		}
		return &releases[i], nil
	}
	return nil, fmt.Errorf("no release with update artifacts found")
}

// findAssetURL returns the browser_download_url for the asset with the given name.
func findAssetURL(assets []ghAsset, name string) string {
	for _, a := range assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func findAsset(assets []ghAsset, name string) *ghAsset {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}

// downloadFile downloads a URL to a local file path.
func downloadFile(url, destPath string) error {
	req, err := newPublicGitHubRequest(http.MethodGet, url, "")
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	return nil
}

// downloadAsset downloads a release asset to destPath. When authenticated is
// true (private/JBailes channel), the GitHub API URL is used with a bearer
// token if one is configured. For public repos (stable/testing channels) pass
// authenticated=false so the browser_download_url is always used — GitHub
// returns HTTP 403 when a token with insufficient scope hits the API asset URL.
func downloadAsset(asset *ghAsset, destPath string, authenticated bool) error {
	if asset == nil {
		return fmt.Errorf("asset not found")
	}

	url := asset.BrowserDownloadURL
	var (
		req *http.Request
		err error
	)
	if authenticated && asset.URL != "" {
		if token := readGitHubToken(); token != "" {
			req, err = newAuthenticatedGitHubRequest(http.MethodGet, asset.URL, "application/octet-stream")
			if err != nil {
				return err
			}
			url = asset.URL
		} else {
			req, err = newPublicGitHubRequest(http.MethodGet, url, "")
			if err != nil {
				return err
			}
		}
	} else {
		req, err = newPublicGitHubRequest(http.MethodGet, url, "")
		if err != nil {
			return err
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	return nil
}
