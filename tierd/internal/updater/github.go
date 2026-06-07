package updater

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

var httpClient = newHTTPClient()
var githubTokenFilePath = "/etc/tierd/github-token"

// newHTTPClient creates the HTTP client used for all GitHub API access.
//
// DNS path:
//   - If the systemd-resolved stub at 127.0.0.53 responds to a probe query,
//     return a default-resolver client. The stub is the OS-recommended path,
//     it honours configured search domains, and on most boxes Just Works.
//   - Otherwise build a custom resolver that talks UDP directly to upstream
//     nameservers from /run/systemd/resolve/resolv.conf. We probe each
//     candidate before adding it to the dial list — one broken upstream
//     (no responses, but with the routing table happy) used to make every
//     fetch wait the full 10 s lookup budget and then give up, because
//     UDP "connect" never fails and our failover loop only ran on Dial
//     errors.
//   - As a last resort, public 1.1.1.1 / 8.8.8.8.
//
// 15 s overall HTTP timeout matches the per-channel goroutines in Check().
func newHTTPClient() *http.Client {
	if probeDNSServer("127.0.0.53") {
		return &http.Client{Timeout: 15 * time.Second}
	}

	servers := workingUpstreamDNSServers()
	if len(servers) == 0 {
		log.Printf("updater: no working DNS resolver found; falling back to default; update checks may fail")
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

// workingUpstreamDNSServers reads candidate upstream nameservers and returns
// the subset that respond to a real DNS query. Order is preserved so the
// caller's failover loop tries the operator-configured ones first, then
// public fallbacks. Only called when the systemd-resolved stub is down.
func workingUpstreamDNSServers() []string {
	candidates := parseNameservers("/run/systemd/resolve/resolv.conf")
	candidates = append(candidates, "1.1.1.1", "8.8.8.8")

	var working []string
	for _, s := range candidates {
		ip := net.ParseIP(s)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if !probeDNSServer(s) {
			continue
		}
		working = append(working, s)
	}
	return working
}

// probeDNSServer sends a minimal A query for example.com to the given server
// over UDP and waits up to 1 s for a response. Returns true if any reply
// arrived in time. Used to filter out misconfigured / firewalled upstreams
// at startup so the resolver doesn't pick a black-hole server and stall
// every lookup until Go's 10 s lookup-timeout kicks in.
func probeDNSServer(server string) bool {
	conn, err := net.DialTimeout("udp", server+":53", 1*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(1 * time.Second)); err != nil {
		return false
	}
	// Hand-rolled DNS query for "example.com." A IN, recursion desired.
	// Cheaper than pulling in net/dns just for one query.
	q := []byte{
		0x12, 0x34, // ID
		0x01, 0x00, // RD=1
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	for _, label := range []string{"example", "com"} {
		q = append(q, byte(len(label)))
		q = append(q, []byte(label)...)
	}
	q = append(q, 0x00, 0x00, 0x01, 0x00, 0x01) // root, A, IN
	if _, err := conn.Write(q); err != nil {
		return false
	}
	buf := make([]byte, 512)
	_, err = conn.Read(buf)
	return err == nil
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

// maxHTTPAttempts bounds retries for transient GitHub failures. GitHub's
// release-download edge intermittently returns 5xx (e.g. a 504 gateway
// timeout that clears on retry); without this, a single blip aborts the
// whole update.
const maxHTTPAttempts = 4

// transientStatus reports whether an HTTP status code is worth retrying.
func transientStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

func drainAndClose(body io.ReadCloser) {
	io.Copy(io.Discard, body)
	body.Close()
}

// doWithRetry issues the request built by newReq and retries transient
// failures (network errors and 429/5xx) with linear backoff. The request is
// rebuilt each attempt so it carries a fresh body. A successful response
// (including non-transient non-200 statuses, which the caller turns into its
// own error) is returned for the caller to close.
func doWithRetry(newReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxHTTPAttempts; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else if transientStatus(resp.StatusCode) {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			drainAndClose(resp.Body)
		} else {
			return resp, nil
		}
		if attempt < maxHTTPAttempts {
			log.Printf("updater: transient HTTP failure (attempt %d/%d): %v; retrying", attempt, maxHTTPAttempts, lastErr)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return nil, lastErr
}

func fetchReleases(baseURL, repoOwner, repoName string, authenticated bool) ([]ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100", baseURL, repoOwner, repoName)

	resp, err := doWithRetry(func() (*http.Request, error) {
		return newGitHubRequest(http.MethodGet, url, "application/vnd.github+json", authenticated)
	})
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
//
// Multi-arch releases ship `tierd-amd64` / `tierd-arm64`; older single-arch
// releases shipped `tierd`. We accept either as long as one matches this
// host's architecture.
func fetchLatestArtifactRelease(baseURL, owner, repo string) (*ghRelease, error) {
	releases, err := fetchReleases(baseURL, owner, repo, true)
	if err != nil {
		return nil, err
	}
	hasArtifacts := func(assets []ghAsset) bool {
		if findAssetURL(assets, "manifest.json") == "" {
			return false
		}
		if findAssetURL(assets, "tierd-ui.tar.gz") == "" {
			return false
		}
		// Accept a multi-arch release that includes this host's binary
		// or a legacy single-arch release that includes plain `tierd`.
		archAsset := tierdAssetNameForArch(runtime.GOARCH)
		if findAssetURL(assets, archAsset) == "" && findAssetURL(assets, "tierd") == "" {
			return false
		}
		return true
	}
	for i := range releases {
		if !releases[i].Prerelease || !strings.HasPrefix(releases[i].TagName, "testing-") {
			continue
		}
		if hasArtifacts(releases[i].Assets) {
			return &releases[i], nil
		}
	}
	for i := range releases {
		if hasArtifacts(releases[i].Assets) {
			return &releases[i], nil
		}
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
	resp, err := doWithRetry(func() (*http.Request, error) {
		return newPublicGitHubRequest(http.MethodGet, url, "")
	})
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

	// Default to the public browser_download_url; switch to the authenticated
	// API asset URL only for the private channel when a token is configured.
	url := asset.BrowserDownloadURL
	accept := ""
	useAuth := false
	if authenticated && asset.URL != "" {
		if token := readGitHubToken(); token != "" {
			url = asset.URL
			accept = "application/octet-stream"
			useAuth = true
		}
	}

	resp, err := doWithRetry(func() (*http.Request, error) {
		return newGitHubRequest(http.MethodGet, url, accept, useAuth)
	})
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
