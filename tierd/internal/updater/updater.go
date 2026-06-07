package updater

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/firewall"
)

const (
	owner            = "RakuenSoftware"
	repo             = "smoothnas"
	privateOwner     = "JBailes"
	privateRepo      = "SmoothNAS"
	privateRepoSSH   = "git@github.com:JBailes/SmoothNAS.git"
	privateBuildRoot = "/var/lib/tierd/private-build"
	stagingDir       = "/var/lib/tierd/update"

	binaryPath  = "/usr/local/bin/tierd"
	uiPath      = "/usr/share/tierd-ui"
	runtimePath = "/usr/lib/smoothnas/docker-lxc-daemon"

	cacheTTL         = 5 * time.Minute
	minCheckInterval = 1 * time.Minute
)

var channelFilePath = "/etc/tierd/update-channel"
var aptAutoUpgrades = "/etc/apt/apt.conf.d/20auto-upgrades"
var aptSecurityRules = "/etc/apt/apt.conf.d/52smoothnas-security-upgrades"
var aptBcachefsKeyPath = "/etc/apt/trusted.gpg.d/apt.bcachefs.org.asc"
var aptBcachefsSourcesPath = "/etc/apt/sources.list.d/apt.bcachefs.org.sources"
var smoothKernelHeadersProviderBuildRoot = "/var/lib/tierd/smoothkernel-headers-virtual"
var osReleasePath = "/etc/os-release"
var execCommand = exec.Command
var isPackageInstalled = packageInstalled
var applyFirewallRules = firewall.Apply
var enabledFirewallProtocols = firewall.GetEnabledProtocols

// smoothfsInstalledRefPath records the git ref of the currently installed
// smoothfs DKMS module so rebuild is skipped when the ref hasn't changed.
var smoothfsInstalledRefPath = "/var/lib/tierd/smoothfs-ref"

// smoothfsSrcPath is where the smoothfs DKMS source tree lives on the
// installed system; firstboot seeds it from the ISO, updates refresh it.
var smoothfsSrcPath = "/opt/smoothnas/smoothfs-src"

// Channel represents an update channel.
type Channel string

const (
	ChannelStable  Channel = "stable"
	ChannelTesting Channel = "testing"
	ChannelJBailes Channel = "jbailes"
)

// Manifest matches the manifest.json uploaded with each release.
//
// Releases are multi-arch: a single release tag ships both `tierd-amd64`
// and `tierd-arm64`, plus arch-independent UI assets. The manifest
// carries one SHA-256 per architecture. The legacy single-arch field
// (`tierd_sha256`) is preserved for back-compat with old manuals
// produced before the multi-arch split — `TierdSHAForArch` falls
// back to it when an arch-specific field is empty.
type Manifest struct {
	Version         string `json:"version"`
	Channel         string `json:"channel"`
	TierdAmd64SHA   string `json:"tierd_amd64_sha256,omitempty"`
	TierdArm64SHA   string `json:"tierd_arm64_sha256,omitempty"`
	TierdSHA        string `json:"tierd_sha256,omitempty"` // legacy single-arch fallback
	RuntimeAmd64SHA string `json:"runtime_amd64_sha256,omitempty"`
	RuntimeArm64SHA string `json:"runtime_arm64_sha256,omitempty"`
	RuntimeSHA      string `json:"runtime_sha256,omitempty"` // legacy single-arch fallback
	UISHA           string `json:"ui_sha256"`
	SmoothfsRef     string `json:"smoothfs_ref,omitempty"`
	SmoothfsSrcSHA  string `json:"smoothfs_src_sha256,omitempty"`
	SmoothkernelTag string `json:"smoothkernel_tag,omitempty"`
}

// TierdSHAForArch returns the manifest's SHA-256 for the given GOARCH,
// or the legacy single-arch hash if the arch-specific field is empty.
// Returns "" if the manifest has neither, which lets the caller fail
// with a clear "no checksum for arch" error rather than silently
// passing a verify against an empty digest.
func (m *Manifest) TierdSHAForArch(arch string) string {
	switch arch {
	case "amd64":
		if m.TierdAmd64SHA != "" {
			return m.TierdAmd64SHA
		}
	case "arm64":
		if m.TierdArm64SHA != "" {
			return m.TierdArm64SHA
		}
	}
	return m.TierdSHA
}

// RuntimeSHAForArch returns the manifest's SHA-256 for the bundled
// smoothnas-runtime daemon, or "" for old releases that did not ship it.
func (m *Manifest) RuntimeSHAForArch(arch string) string {
	switch arch {
	case "amd64":
		if m.RuntimeAmd64SHA != "" {
			return m.RuntimeAmd64SHA
		}
	case "arm64":
		if m.RuntimeArm64SHA != "" {
			return m.RuntimeArm64SHA
		}
	}
	return m.RuntimeSHA
}

// tierdAssetNameForArch returns the release-asset filename to download
// for the given GOARCH. Multi-arch releases ship `tierd-amd64` /
// `tierd-arm64`; older single-arch releases shipped just `tierd`.
// Callers should try the arch-specific name first and fall back to
// the legacy name when running against an old release.
func tierdAssetNameForArch(arch string) string {
	switch arch {
	case "amd64", "arm64":
		return "tierd-" + arch
	}
	return "tierd"
}

func runtimeAssetNameForArch(arch string) string {
	switch arch {
	case "amd64", "arm64":
		return "smoothnas-runtime-" + arch
	}
	return "smoothnas-runtime"
}

// ReleaseInfo is the public-facing release metadata.
type ReleaseInfo struct {
	Version   string `json:"version"`
	Body      string `json:"body"`
	Published string `json:"published"`
}

// UpdateStatus is the response for the check endpoint.
type UpdateStatus struct {
	Available      bool         `json:"available"`
	CurrentVersion string       `json:"current_version"`
	Channel        Channel      `json:"channel"`
	Latest         *ReleaseInfo `json:"latest,omitempty"`  // For current channel
	Stable         *ReleaseInfo `json:"stable,omitempty"`  // Always latest stable
	Testing        *ReleaseInfo `json:"testing,omitempty"` // Always latest testing
	JBailes        *ReleaseInfo `json:"jbailes,omitempty"` // Fork release channel when available
}

// DebianPackageStatus is the public-facing status for Debian package updates.
type DebianPackageStatus struct {
	SecurityAutomatic bool     `json:"security_automatic"`
	Upgradable        []string `json:"upgradable"`
	LastCheck         string   `json:"last_check,omitempty"`
}

// ApplyProgress tracks the current stage of an in-flight update.
type ApplyProgress struct {
	Stage string `json:"stage"`
	Error string `json:"error,omitempty"`
}

// appliedVersionPath stores the manifest.Version of the most recent
// successful update, so the UI and updater can show "what's actually
// installed" rather than the compile-time string baked into the binary.
//
// Release pipelines sometimes inject a stable semver into the binary
// shipped by a *testing* release (because the build records the latest
// stable tag, not the prerelease tag), so the binary can self-report
// `0.0.46` while the operator actually installed
// `testing-2026.0508.1253-c7718a8`. That mismatch made cross-scheme
// version comparison and the UI both lie. Persisting manifest.Version
// here is authoritative — it's the version string of the release the
// updater actually applied.
var appliedVersionPath = "/var/lib/tierd/applied-version"

// Updater checks for and applies SmoothNAS updates from GitHub Releases.
type Updater struct {
	currentVersion string
	githubBaseURL  string

	mu       sync.Mutex
	progress *ApplyProgress
	applying bool

	packageProgress *ApplyProgress
	packageApplying bool

	// Cached debian status (upgradable list from last check).
	cachedDebian *DebianPackageStatus

	// Cached check result.
	cachedStatus  *UpdateStatus
	cachedAt      time.Time
	lastAttemptAt time.Time
}

// New creates an Updater for the given running version.
func New(currentVersion string) *Updater {
	return &Updater{
		currentVersion: currentVersion,
		githubBaseURL:  "https://api.github.com",
	}
}

// effectiveCurrentVersion returns the most authoritative current-version
// string: the manifest.Version of the last successfully applied update
// (read from appliedVersionPath) if present, otherwise the build-time
// `currentVersion` baked into the running binary. See appliedVersionPath
// docstring for why this matters.
func (u *Updater) effectiveCurrentVersion() string {
	if data, err := os.ReadFile(appliedVersionPath); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return v
		}
	}
	return u.currentVersion
}

// writeAppliedVersion persists `v` as the version of the most recent
// successful update. Best-effort: a write failure is logged but does not
// fail the update (the binary install already happened, and the only
// downstream cost of a stale file is the UI showing the build-time
// version until the next successful apply).
func writeAppliedVersion(v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(appliedVersionPath), 0o755); err != nil {
		log.Printf("updater: persist applied version: mkdir: %v", err)
		return
	}
	if err := os.WriteFile(appliedVersionPath, []byte(v+"\n"), 0o644); err != nil {
		log.Printf("updater: persist applied version: write: %v", err)
	}
}

// Channel reads the configured update channel from disk. Defaults to stable.
func (u *Updater) Channel() Channel {
	data, err := os.ReadFile(channelFilePath)
	if err == nil {
		ch := Channel(strings.TrimSpace(string(data)))
		if ch == ChannelTesting {
			return ChannelTesting
		}
		if ch == ChannelJBailes {
			return ChannelJBailes
		}
		if ch == ChannelStable {
			return ChannelStable
		}
	}

	return defaultChannelForVersion(u.currentVersion)
}

// SetChannel persists the update channel to disk.
func (u *Updater) SetChannel(ch Channel) error {
	if ch != ChannelStable && ch != ChannelTesting && ch != ChannelJBailes {
		return fmt.Errorf("invalid channel %q: must be %q, %q, or %q", ch, ChannelStable, ChannelTesting, ChannelJBailes)
	}
	if ch == ChannelJBailes {
		// Skip the network round-trip when the last check already confirmed the
		// JBailes fork is accessible (the "Switch to JBailes" button is only
		// enabled when updateInfo.jbailes is set, so we already know it works).
		u.mu.Lock()
		cachedHasJBailes := u.cachedStatus != nil && u.cachedStatus.JBailes != nil
		u.mu.Unlock()

		if !cachedHasJBailes {
			if _, err := fetchLatestArtifactRelease(u.githubBaseURL, privateOwner, privateRepo); err != nil {
				return fmt.Errorf("fork release %s/%s is not accessible: %w", privateOwner, privateRepo, err)
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(channelFilePath), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	if err := os.WriteFile(channelFilePath, []byte(string(ch)+"\n"), 0644); err != nil {
		return fmt.Errorf("write channel file: %w", err)
	}

	// Re-evaluate the cached status for the new channel rather than clearing it.
	// This lets the next Check() call return the updated available/latest without
	// a full GitHub re-fetch, making channel switching instantaneous in the UI.
	u.mu.Lock()
	if u.cachedStatus != nil {
		updated := *u.cachedStatus
		updated.Channel = ch
		updated.Available = false
		updated.Latest = nil
		var rel *ReleaseInfo
		switch ch {
		case ChannelTesting:
			rel = updated.Testing
		case ChannelJBailes:
			rel = updated.JBailes
		default:
			rel = updated.Stable
		}
		if rel != nil {
			if newer, err := compareVersions(updated.CurrentVersion, rel.Version); err == nil && newer {
				updated.Available = true
				updated.Latest = rel
			}
		}
		u.cachedStatus = &updated
	}
	u.mu.Unlock()

	return nil
}

// fetchRelease gets the appropriate release based on channel.
func (u *Updater) fetchRelease() (*ghRelease, error) {
	if u.Channel() == ChannelTesting {
		return fetchLatestPrerelease(u.githubBaseURL, owner, repo)
	}
	if u.Channel() == ChannelJBailes {
		return fetchLatestArtifactRelease(u.githubBaseURL, privateOwner, privateRepo)
	}
	return fetchLatestRelease(u.githubBaseURL, owner, repo)
}

// Check queries GitHub for the latest releases on both channels.
func (u *Updater) Check() (*UpdateStatus, error) {
	u.mu.Lock()
	if u.cachedStatus != nil && time.Since(u.cachedAt) < cacheTTL {
		s := u.cachedStatus
		u.mu.Unlock()
		return s, nil
	}
	if !u.lastAttemptAt.IsZero() && time.Since(u.lastAttemptAt) < minCheckInterval {
		s := u.cachedStatus
		u.mu.Unlock()
		return s, nil
	}
	u.lastAttemptAt = time.Now()
	u.mu.Unlock()

	// Fetch public release channels and the fork release channel in parallel.
	var stableRel, testingRel, jbailesSrcRel *ghRelease
	var stableErr, testingErr error
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		stableRel, stableErr = fetchLatestRelease(u.githubBaseURL, owner, repo)
	}()
	go func() {
		defer wg.Done()
		testingRel, testingErr = fetchLatestPrerelease(u.githubBaseURL, owner, repo)
	}()
	go func() {
		defer wg.Done()
		jbailesSrcRel, _ = fetchLatestArtifactRelease(u.githubBaseURL, privateOwner, privateRepo)
	}()
	wg.Wait()
	if stableErr != nil {
		log.Printf("updater: stable channel fetch failed: %v", stableErr)
	}
	if testingErr != nil {
		log.Printf("updater: testing channel fetch failed: %v", testingErr)
	}

	// Grab stale cache for fallback: on any per-channel failure we serve the
	// previously fetched data rather than surfacing an error.
	u.mu.Lock()
	staleStatus := u.cachedStatus
	u.mu.Unlock()

	// If every channel failed, serve whatever stale data we have (or nothing).
	// Never return a hard error — the caller will just see no new info.
	if stableErr != nil && testingErr != nil && jbailesSrcRel == nil {
		return staleStatus, nil
	}

	status := &UpdateStatus{
		CurrentVersion: normalizeReleaseVersion(u.effectiveCurrentVersion()),
		Channel:        u.Channel(),
	}

	if stableRel != nil {
		tag := normalizeReleaseVersion(stableRel.TagName)
		status.Stable = &ReleaseInfo{
			Version:   tag,
			Body:      stableRel.Body,
			Published: stableRel.PublishedAt,
		}
	} else if staleStatus != nil {
		status.Stable = staleStatus.Stable
	}
	if testingRel != nil {
		tag := normalizeReleaseVersion(testingRel.TagName)
		status.Testing = &ReleaseInfo{
			Version:   tag,
			Body:      testingRel.Body,
			Published: testingRel.PublishedAt,
		}
	} else if staleStatus != nil {
		status.Testing = staleStatus.Testing
	}
	if jbailesSrcRel != nil {
		status.JBailes = &ReleaseInfo{
			Version:   normalizeReleaseVersion(jbailesSrcRel.TagName),
			Body:      jbailesSrcRel.Body,
			Published: jbailesSrcRel.PublishedAt,
		}
	} else if staleStatus != nil {
		status.JBailes = staleStatus.JBailes
	}

	// Determine if an update is available for the CURRENT channel.
	currentChannel := u.Channel()
	var rel *ghRelease
	if currentChannel == ChannelTesting {
		rel = testingRel
	} else if currentChannel == ChannelJBailes {
		if jbailesSrcRel == nil {
			rel = nil
		} else {
			rel = jbailesSrcRel
		}
	} else {
		rel = stableRel
	}

	if rel != nil {
		latestVersion := normalizeReleaseVersion(rel.TagName)
		newer, err := compareVersions(status.CurrentVersion, latestVersion)
		if err == nil && newer {
			status.Available = true
			status.Latest = &ReleaseInfo{
				Version:   latestVersion,
				Body:      rel.Body,
				Published: rel.PublishedAt,
			}
		}
	} else if staleStatus != nil {
		// Current channel fetch failed — carry forward stale availability info.
		status.Available = staleStatus.Available
		status.Latest = staleStatus.Latest
	}

	u.mu.Lock()
	u.cachedStatus = status
	u.cachedAt = time.Now()
	u.mu.Unlock()

	return status, nil
}

// StartApply begins the update process in a background goroutine. The initial
// state is set synchronously so that progress polls never see a stale "idle".
// Returns an error if an update is already in progress.
func (u *Updater) StartApply() error {
	u.mu.Lock()
	if u.applying || u.packageApplying {
		u.mu.Unlock()
		return fmt.Errorf("update already in progress")
	}
	u.applying = true
	u.progress = &ApplyProgress{Stage: "downloading"}
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.applying = false
			u.mu.Unlock()
		}()

		if err := u.doApply(); err != nil {
			log.Printf("update failed: %v", err)
			u.mu.Lock()
			u.progress = &ApplyProgress{Stage: "failed", Error: err.Error()}
			u.mu.Unlock()
		}
	}()

	return nil
}

func (u *Updater) doApply() error {
	// Fetch release metadata for the configured channel.
	rel, err := u.fetchRelease()
	if err != nil {
		return fmt.Errorf("fetch release: %w", err)
	}

	// Find assets. Multi-arch releases ship `tierd-{amd64,arm64}`;
	// older single-arch releases shipped `tierd`. Try the arch-specific
	// name first, fall back to the legacy name.
	arch := runtime.GOARCH
	manifestAsset := findAsset(rel.Assets, "manifest.json")
	binaryAsset := findAsset(rel.Assets, tierdAssetNameForArch(arch))
	if binaryAsset == nil {
		binaryAsset = findAsset(rel.Assets, "tierd")
	}
	runtimeAsset := findAsset(rel.Assets, runtimeAssetNameForArch(arch))
	if runtimeAsset == nil {
		runtimeAsset = findAsset(rel.Assets, "smoothnas-runtime")
	}
	uiAsset := findAsset(rel.Assets, "tierd-ui.tar.gz")
	smoothfsSrcAsset := findAsset(rel.Assets, "smoothfs-src.tar.gz") // optional, nil on older releases
	if manifestAsset == nil || binaryAsset == nil || uiAsset == nil {
		return fmt.Errorf("release is missing required assets (need manifest.json, tierd-%s or tierd, tierd-ui.tar.gz)", arch)
	}

	// Prepare staging directory.
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	// Download all artifacts.
	manifestPath := filepath.Join(stagingDir, "manifest.json")
	binaryStagePath := filepath.Join(stagingDir, "tierd")
	runtimeStagePath := filepath.Join(stagingDir, "smoothnas-runtime")
	uiStagePath := filepath.Join(stagingDir, "tierd-ui.tar.gz")

	// Only use authenticated API downloads for the private JBailes channel;
	// public releases use browser_download_url to avoid 403s from scoped tokens.
	authenticated := u.Channel() == ChannelJBailes

	smoothfsSrcStagePath := filepath.Join(stagingDir, "smoothfs-src.tar.gz")
	downloads := []struct {
		asset *ghAsset
		dest  string
	}{
		{manifestAsset, manifestPath},
		{binaryAsset, binaryStagePath},
		{uiAsset, uiStagePath},
	}
	if runtimeAsset != nil {
		downloads = append(downloads, struct {
			asset *ghAsset
			dest  string
		}{runtimeAsset, runtimeStagePath})
	}
	if smoothfsSrcAsset != nil {
		downloads = append(downloads, struct {
			asset *ghAsset
			dest  string
		}{smoothfsSrcAsset, smoothfsSrcStagePath})
	}
	for _, dl := range downloads {
		if err := downloadAsset(dl.asset, dl.dest, authenticated); err != nil {
			return err
		}
	}

	// Parse manifest.
	u.setStage("verifying")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Verify checksums against the per-arch manifest entry.
	binarySHA := manifest.TierdSHAForArch(arch)
	if binarySHA == "" {
		return fmt.Errorf("manifest carries no checksum for arch %q (have amd64=%t arm64=%t legacy=%t)",
			arch, manifest.TierdAmd64SHA != "", manifest.TierdArm64SHA != "", manifest.TierdSHA != "")
	}
	if err := verifyChecksum(binaryStagePath, binarySHA); err != nil {
		return fmt.Errorf("binary checksum: %w", err)
	}
	if err := verifyChecksum(uiStagePath, manifest.UISHA); err != nil {
		return fmt.Errorf("UI checksum: %w", err)
	}
	runtimeSHA := manifest.RuntimeSHAForArch(arch)
	runtimeUpdated := false
	if runtimeSHA != "" {
		if runtimeAsset == nil {
			return fmt.Errorf("release manifest carries runtime checksum for arch %q but release is missing %s", arch, runtimeAssetNameForArch(arch))
		}
		if err := verifyChecksum(runtimeStagePath, runtimeSHA); err != nil {
			return fmt.Errorf("runtime checksum: %w", err)
		}
	}

	// Install.
	u.setStage("installing")

	// Back up and replace the binary.
	if err := backupAndReplace(binaryStagePath, binaryPath, 0755); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	// Back up and replace UI assets.
	if err := replaceUI(uiStagePath, uiPath); err != nil {
		return fmt.Errorf("replace UI: %w", err)
	}
	if runtimeSHA != "" {
		if err := backupAndReplace(runtimeStagePath, runtimePath, 0755); err != nil {
			return fmt.Errorf("replace runtime: %w", err)
		}
		runtimeUpdated = true
	}

	// Record the version we just applied so subsequent checks reflect
	// what the operator actually installed, not whatever the binary's
	// compile-time version string happens to say.
	writeAppliedVersion(manifest.Version)

	// Ensure required OS packages are present. apt-get install is a no-op
	// for packages that are already installed, so this is safe to run every time.
	EnsureSystemPackages()
	firewallUpdated := refreshHostFirewall()

	// Rebuild the smoothfs DKMS kernel module if the release carries updated source.
	// Non-fatal: tierd is still restarted even if the module build fails.
	if manifest.SmoothfsRef != "" && smoothfsSrcAsset != nil {
		u.setStage("rebuilding kernel module")
		if err := verifySmoothfsTarball(smoothfsSrcStagePath, manifest.SmoothfsSrcSHA); err != nil {
			log.Printf("updater: smoothfs source check failed (skipping module rebuild): %v", err)
		} else if err := ensureSmoothfsModule(smoothfsSrcStagePath, manifest.SmoothfsRef); err != nil {
			log.Printf("updater: smoothfs module rebuild failed (non-fatal): %v", err)
		}
	}

	// Install the pinned SmoothKernel (and its matching OpenZFS stack) if the
	// box isn't already on it. Non-fatal: a kernel-download failure must not
	// break the tierd update. When a kernel is installed we reboot to activate
	// it instead of just restarting services.
	rebootRequired := false
	if manifest.SmoothkernelTag != "" {
		u.setStage("installing kernel")
		if installed, err := ensureSmoothKernel(u.githubBaseURL, manifest.SmoothkernelTag, runtime.GOARCH); err != nil {
			log.Printf("updater: smoothkernel install failed (non-fatal): %v", err)
		} else if installed {
			rebootRequired = true
		}
	}

	// Clean up staging directory.
	os.RemoveAll(stagingDir)

	// Invalidate the cached check result so the new version is reflected.
	u.mu.Lock()
	u.cachedStatus = nil
	u.mu.Unlock()

	return u.restartOrReboot(rebootRequired, runtimeUpdated, firewallUpdated)
}

// restartOrReboot finishes an apply. When a new kernel was installed it
// reboots the host (which activates the kernel and brings every service back
// up); otherwise it restarts the runtime (if changed) and tierd in place.
//
// The stage is set before the delay so the frontend polls at least once more
// and sees "restarting"/"rebooting" before the process dies — without it the
// new process starts at stage="idle" and the UI reports "Update process
// stopped unexpectedly".
func (u *Updater) restartOrReboot(rebootRequired, runtimeUpdated, firewallUpdated bool) error {
	if rebootRequired {
		u.setStage("rebooting")
		time.Sleep(4 * time.Second)
		if out, err := execCommand("systemctl", "reboot").CombinedOutput(); err != nil {
			return fmt.Errorf("reboot: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}

	u.setStage("restarting")
	time.Sleep(4 * time.Second)
	if runtimeUpdated || firewallUpdated {
		if out, err := execCommand("systemctl", "restart", "smoothnas-runtime.service").CombinedOutput(); err != nil {
			return fmt.Errorf("restart smoothnas-runtime: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	exec.Command("systemctl", "restart", "tierd.service").Start()

	return nil
}

// StartManualApply begins the update from locally provided artifacts.
// The caller provides the raw contents of manifest.json, the tierd binary,
// the tierd-ui.tar.gz archive, and optionally the smoothnas-runtime binary
// plus smoothfs-src.tar.gz. Returns an error if already applying.
func (u *Updater) StartManualApply(manifest, binary, ui, runtimeBinary, smoothfsSrc []byte) error {
	u.mu.Lock()
	if u.applying || u.packageApplying {
		u.mu.Unlock()
		return fmt.Errorf("update already in progress")
	}
	u.applying = true
	u.progress = &ApplyProgress{Stage: "verifying"}
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.applying = false
			u.mu.Unlock()
		}()

		if err := u.doManualApply(manifest, binary, ui, runtimeBinary, smoothfsSrc); err != nil {
			log.Printf("manual update failed: %v", err)
			u.mu.Lock()
			u.progress = &ApplyProgress{Stage: "failed", Error: err.Error()}
			u.mu.Unlock()
		}
	}()

	return nil
}

func (u *Updater) doManualApply(manifestData, binaryData, uiData, runtimeBinaryData, smoothfsSrcData []byte) error {
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Prepare staging directory and write files.
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	binaryStagePath := filepath.Join(stagingDir, "tierd")
	runtimeStagePath := filepath.Join(stagingDir, "smoothnas-runtime")
	uiStagePath := filepath.Join(stagingDir, "tierd-ui.tar.gz")

	if err := os.WriteFile(binaryStagePath, binaryData, 0644); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}
	if err := os.WriteFile(uiStagePath, uiData, 0644); err != nil {
		return fmt.Errorf("write UI archive: %w", err)
	}
	if len(runtimeBinaryData) > 0 {
		if err := os.WriteFile(runtimeStagePath, runtimeBinaryData, 0644); err != nil {
			return fmt.Errorf("write runtime binary: %w", err)
		}
	}

	// Verify checksums against the per-arch manifest entry. The caller
	// is expected to have uploaded the binary matching this host's arch;
	// we don't try to detect mismatch here, only validate the digest.
	arch := runtime.GOARCH
	binarySHA := manifest.TierdSHAForArch(arch)
	if binarySHA == "" {
		return fmt.Errorf("manifest carries no checksum for arch %q (have amd64=%t arm64=%t legacy=%t)",
			arch, manifest.TierdAmd64SHA != "", manifest.TierdArm64SHA != "", manifest.TierdSHA != "")
	}
	if err := verifyChecksum(binaryStagePath, binarySHA); err != nil {
		return fmt.Errorf("binary checksum: %w", err)
	}
	if err := verifyChecksum(uiStagePath, manifest.UISHA); err != nil {
		return fmt.Errorf("UI checksum: %w", err)
	}
	runtimeSHA := manifest.RuntimeSHAForArch(arch)
	runtimeUpdated := false
	if runtimeSHA != "" {
		if len(runtimeBinaryData) == 0 {
			return fmt.Errorf("manifest carries runtime checksum for arch %q but upload is missing smoothnas-runtime", arch)
		}
		if err := verifyChecksum(runtimeStagePath, runtimeSHA); err != nil {
			return fmt.Errorf("runtime checksum: %w", err)
		}
	}

	// Install.
	u.setStage("installing")

	if err := backupAndReplace(binaryStagePath, binaryPath, 0755); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	if err := replaceUI(uiStagePath, uiPath); err != nil {
		return fmt.Errorf("replace UI: %w", err)
	}
	if runtimeSHA != "" {
		if err := backupAndReplace(runtimeStagePath, runtimePath, 0755); err != nil {
			return fmt.Errorf("replace runtime: %w", err)
		}
		runtimeUpdated = true
	}

	writeAppliedVersion(manifest.Version)

	EnsureSystemPackages()
	firewallUpdated := refreshHostFirewall()

	if manifest.SmoothfsRef != "" && len(smoothfsSrcData) > 0 {
		u.setStage("rebuilding kernel module")
		smoothfsTarPath := filepath.Join(stagingDir, "smoothfs-src.tar.gz")
		if err := os.WriteFile(smoothfsTarPath, smoothfsSrcData, 0644); err != nil {
			log.Printf("updater: smoothfs: stage tarball: %v", err)
		} else if err := verifySmoothfsTarball(smoothfsTarPath, manifest.SmoothfsSrcSHA); err != nil {
			log.Printf("updater: smoothfs source check failed (skipping module rebuild): %v", err)
		} else if err := ensureSmoothfsModule(smoothfsTarPath, manifest.SmoothfsRef); err != nil {
			log.Printf("updater: smoothfs module rebuild failed (non-fatal): %v", err)
		}
	}

	rebootRequired := false
	if manifest.SmoothkernelTag != "" {
		u.setStage("installing kernel")
		if installed, err := ensureSmoothKernel(u.githubBaseURL, manifest.SmoothkernelTag, runtime.GOARCH); err != nil {
			log.Printf("updater: smoothkernel install failed (non-fatal): %v", err)
		} else if installed {
			rebootRequired = true
		}
	}

	os.RemoveAll(stagingDir)

	u.mu.Lock()
	u.cachedStatus = nil
	u.mu.Unlock()

	return u.restartOrReboot(rebootRequired, runtimeUpdated, firewallUpdated)
}

func refreshHostFirewall() bool {
	if err := applyFirewallRules(enabledFirewallProtocols()); err != nil {
		log.Printf("updater: refresh firewall ruleset failed: %v", err)
		return false
	}
	return true
}

// Progress returns the current update progress.
func (u *Updater) Progress() *ApplyProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.progress == nil {
		return &ApplyProgress{Stage: "idle"}
	}
	p := *u.progress
	return &p
}

// DebianStatus reports the current OS package update state, including whether
// unattended security upgrades are configured and the last-known upgradable list.
func (u *Updater) DebianStatus() *DebianPackageStatus {
	u.mu.Lock()
	cached := u.cachedDebian
	u.mu.Unlock()

	status := &DebianPackageStatus{
		SecurityAutomatic: automaticSecurityUpdatesEnabled(),
	}
	if cached != nil {
		status.Upgradable = cached.Upgradable
		status.LastCheck = cached.LastCheck
	}
	return status
}

// CheckDebianPackages refreshes the local package lists and records the list
// of upgradable packages in the background. Returns immediately; poll
// DebianPackageProgress() until stage is "idle" or "failed".
func (u *Updater) CheckDebianPackages() error {
	u.mu.Lock()
	if u.applying || u.packageApplying {
		u.mu.Unlock()
		return fmt.Errorf("update already in progress")
	}
	u.packageApplying = true
	u.packageProgress = &ApplyProgress{Stage: "checking packages"}
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.packageApplying = false
			u.mu.Unlock()
		}()

		pkgs, err := listUpgradablePackages()
		u.mu.Lock()
		if err != nil {
			log.Printf("debian package check failed: %v", err)
			u.packageProgress = &ApplyProgress{Stage: "failed", Error: err.Error()}
		} else {
			u.cachedDebian = &DebianPackageStatus{
				SecurityAutomatic: automaticSecurityUpdatesEnabled(),
				Upgradable:        pkgs,
				LastCheck:         time.Now().UTC().Format(time.RFC3339),
			}
			u.packageProgress = &ApplyProgress{Stage: "idle"}
		}
		u.mu.Unlock()
	}()

	return nil
}

// StartDebianPackageApply begins a safe Debian package upgrade in the background.
func (u *Updater) StartDebianPackageApply() error {
	u.mu.Lock()
	if u.applying || u.packageApplying {
		u.mu.Unlock()
		return fmt.Errorf("update already in progress")
	}
	u.packageApplying = true
	u.packageProgress = &ApplyProgress{Stage: "refreshing package lists"}
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.packageApplying = false
			u.mu.Unlock()
		}()

		if err := u.doDebianPackageApply(); err != nil {
			log.Printf("debian package update failed: %v", err)
			u.mu.Lock()
			u.packageProgress = &ApplyProgress{Stage: "failed", Error: err.Error()}
			u.mu.Unlock()
			return
		}

		u.mu.Lock()
		u.packageProgress = &ApplyProgress{Stage: "complete"}
		u.mu.Unlock()
	}()

	return nil
}

func (u *Updater) doDebianPackageApply() error {
	if err := EnsureAutomaticSecurityUpdates(); err != nil {
		return err
	}

	u.setPackageStage("refreshing package lists")
	if err := runAPT("update", "-qq"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}

	u.setPackageStage("installing Debian packages")
	if err := runAPT("upgrade", "-y", "-qq"); err != nil {
		return fmt.Errorf("apt-get upgrade: %w", err)
	}

	// Refresh the cached upgradable list so the UI shows zero packages remaining.
	if pkgs, err := listUpgradablePackages(); err == nil {
		u.mu.Lock()
		u.cachedDebian = &DebianPackageStatus{
			SecurityAutomatic: automaticSecurityUpdatesEnabled(),
			Upgradable:        pkgs,
			LastCheck:         time.Now().UTC().Format(time.RFC3339),
		}
		u.mu.Unlock()
	}

	return nil
}

// DebianPackageProgress returns the current Debian package update progress.
func (u *Updater) DebianPackageProgress() *ApplyProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.packageProgress == nil {
		return &ApplyProgress{Stage: "idle"}
	}
	p := *u.packageProgress
	return &p
}

func (u *Updater) setStage(stage string) {
	u.mu.Lock()
	u.progress = &ApplyProgress{Stage: stage}
	u.mu.Unlock()
}

func (u *Updater) setPackageStage(stage string) {
	u.mu.Lock()
	u.packageProgress = &ApplyProgress{Stage: stage}
	u.mu.Unlock()
}

// verifyChecksum checks that the SHA-256 of the file matches the expected hex digest.
func verifyChecksum(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

// backupAndReplace atomically replaces destPath with the file at srcPath.
func backupAndReplace(srcPath, destPath string, mode os.FileMode) error {
	// Back up existing file.
	if _, err := os.Stat(destPath); err == nil {
		os.Rename(destPath, destPath+".bak")
	}

	// Write new file to a temp location in the same directory, then rename.
	dir := filepath.Dir(destPath)
	tmp, err := os.CreateTemp(dir, ".tierd-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	src, err := os.Open(srcPath)
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	defer src.Close()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()

	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, destPath)
}

// replaceUI extracts a tar.gz archive to the UI directory, backing up the old one.
func replaceUI(archivePath, destDir string) error {
	// Back up existing UI.
	bakDir := destDir + ".bak"
	if err := os.RemoveAll(bakDir); err != nil {
		return fmt.Errorf("remove old UI backup: %w", err)
	}
	if _, err := os.Stat(destDir); err == nil {
		if err := os.Rename(destDir, bakDir); err != nil {
			return fmt.Errorf("backup UI: %w", err)
		}
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		target := filepath.Join(destDir, hdr.Name)

		// Guard against path traversal.
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(destDir) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			out, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			out.Close()
		}
	}

	return nil
}

// verifySmoothfsTarball verifies the tarball's SHA-256 if expectedSHA is set;
// a no-op (returns nil) when expectedSHA is empty, allowing graceful handling
// of manifests that predate the smoothfs_src_sha256 field.
func verifySmoothfsTarball(path, expectedSHA string) error {
	if expectedSHA == "" {
		return nil
	}
	return verifyChecksum(path, expectedSHA)
}

// parseDKMSVersion extracts the PACKAGE_VERSION value from dkms.conf content.
func parseDKMSVersion(dkmsConf string) (string, error) {
	for _, line := range strings.Split(dkmsConf, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "PACKAGE_VERSION") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		ver := dkmsConfValue(parts[1])
		if ver != "" {
			return ver, nil
		}
	}
	return "", fmt.Errorf("PACKAGE_VERSION not found in dkms.conf")
}

// dkmsConfValue extracts the assigned value from the right-hand side of a
// dkms.conf assignment. smoothfs annotates its version for release-please as
//
//	PACKAGE_VERSION="0.2.0" # x-release-please-version
//
// so the value is quoted and followed by an inline comment. A naive
// quote-trim leaves "0.2.0\" # x-release-please-version", which DKMS then
// treats as a (broken) version string. Return the contents of the first
// quoted span and ignore anything after it; fall back to stripping an inline
// comment for bare values.
func dkmsConfValue(rhs string) string {
	rhs = strings.TrimSpace(rhs)
	if len(rhs) > 0 && (rhs[0] == '"' || rhs[0] == '\'') {
		if end := strings.IndexByte(rhs[1:], rhs[0]); end >= 0 {
			return rhs[1 : 1+end]
		}
	}
	if i := strings.IndexByte(rhs, '#'); i >= 0 {
		rhs = rhs[:i]
	}
	return strings.Trim(strings.TrimSpace(rhs), `"'`)
}

// readSmoothfsVersionFromTar opens a smoothfs-src.tar.gz and extracts the
// PACKAGE_VERSION from the dkms.conf entry without fully extracting the archive.
func readSmoothfsVersionFromTar(tarPath string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read: %w", err)
		}
		clean := path.Clean(hdr.Name)
		if clean != "dkms.conf" && !strings.HasSuffix(clean, "/dkms.conf") {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return "", fmt.Errorf("read dkms.conf: %w", err)
		}
		return parseDKMSVersion(string(data))
	}
	return "", fmt.Errorf("dkms.conf not found in smoothfs-src archive")
}

// extractTarGzTo extracts a .tar.gz archive into destDir, which must already
// exist. Path traversal entries are silently skipped.
func extractTarGzTo(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		target := filepath.Join(destDir, filepath.FromSlash(path.Clean("/"+hdr.Name)))
		if target != filepath.Clean(destDir) && !strings.HasPrefix(target, cleanDest) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			_, writeErr := io.Copy(out, tr)
			out.Close()
			if writeErr != nil {
				return fmt.Errorf("write %s: %w", target, writeErr)
			}
		}
	}
	return nil
}

// ensureSmoothfsModule rebuilds and installs the smoothfs DKMS kernel module
// from the provided source tarball. It is a no-op when the installed ref
// already matches, so it is safe to call on every update.
//
// The rebuild runs dkms remove/add/build/install for the running kernel.
// After a successful build it attempts a hot-reload (modprobe -r / modprobe);
// this succeeds only when no smoothfs filesystems are mounted, which is
// unlikely on a live NAS. In the common case the new module loads on the
// next pool remount or system reboot.
//
// Failures are propagated to the caller; callers should log and treat them
// as non-fatal so the rest of the update (tierd binary + UI) still applies.
// smoothfsVersionMalformed reports whether a DKMS version directory name is
// not a usable version — i.e. it contains characters (quotes, whitespace, or
// '#') that the pre-fix parseDKMSVersion bug leaked from the release-please
// annotation `PACKAGE_VERSION="0.2.1" # x-release-please-version`. Clean
// versions like "0.2.1" and the "kernel-*" install symlink are left alone.
func smoothfsVersionMalformed(name string) bool {
	if name == "" || strings.HasPrefix(name, "kernel-") {
		return false
	}
	return strings.ContainsAny(name, "\"' #\t")
}

// cleanupMalformedSmoothfsDKMS removes smoothfs DKMS trees and their /usr/src
// source directories whose version component is malformed (see
// smoothfsVersionMalformed). No-op when none are present.
func cleanupMalformedSmoothfsDKMS() {
	const dkmsRoot = "/var/lib/dkms/smoothfs"
	entries, err := os.ReadDir(dkmsRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !smoothfsVersionMalformed(name) {
			continue
		}
		log.Printf("updater: smoothfs: removing malformed DKMS entry %q", name)
		os.RemoveAll(filepath.Join(dkmsRoot, name))
		os.RemoveAll("/usr/src/smoothfs-" + name)
	}
}

func ensureSmoothfsModule(srcTarPath, ref string) error {
	if ref == "" {
		return nil
	}

	if installed, _ := os.ReadFile(smoothfsInstalledRefPath); strings.TrimSpace(string(installed)) == ref {
		log.Printf("updater: smoothfs ref %.12s already installed, skipping rebuild", ref)
		return nil
	}

	version, err := readSmoothfsVersionFromTar(srcTarPath)
	if err != nil {
		return fmt.Errorf("read smoothfs version: %w", err)
	}

	// Remove any malformed DKMS trees left by the pre-fix parseDKMSVersion
	// bug, which used a version like `0.2.1" # x-release-please-version`. The
	// embedded quote made `make M=/var/lib/dkms/smoothfs/<ver>/build` a shell
	// syntax error, so those builds always failed and the dirs accumulated.
	cleanupMalformedSmoothfsDKMS()

	kernelOut, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return fmt.Errorf("uname -r: %w", err)
	}
	kver := strings.TrimSpace(string(kernelOut))

	dkmsSrc := fmt.Sprintf("/usr/src/smoothfs-%s", version)
	if err := os.RemoveAll(dkmsSrc); err != nil {
		return fmt.Errorf("remove old DKMS src %s: %w", dkmsSrc, err)
	}
	if err := os.MkdirAll(dkmsSrc, 0755); err != nil {
		return fmt.Errorf("create DKMS src dir: %w", err)
	}
	if err := extractTarGzTo(srcTarPath, dkmsSrc); err != nil {
		return fmt.Errorf("extract smoothfs source: %w", err)
	}

	// Remove any stale DKMS entry; ignore errors (entry may not exist yet).
	execCommand("dkms", "remove", "-m", "smoothfs", "-v", version, "--all").Run()

	if out, err := execCommand("dkms", "add", "-m", "smoothfs", "-v", version).CombinedOutput(); err != nil {
		return fmt.Errorf("dkms add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := execCommand("dkms", "build", "-m", "smoothfs", "-v", version, "-k", kver).CombinedOutput(); err != nil {
		return fmt.Errorf("dkms build: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := execCommand("dkms", "install", "-m", "smoothfs", "-v", version, "-k", kver).CombinedOutput(); err != nil {
		return fmt.Errorf("dkms install: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Update /opt/smoothnas/smoothfs-src so it reflects the new version.
	os.RemoveAll(smoothfsSrcPath)
	if err := os.MkdirAll(smoothfsSrcPath, 0755); err == nil {
		if err := extractTarGzTo(srcTarPath, smoothfsSrcPath); err != nil {
			log.Printf("updater: smoothfs: update %s (non-fatal): %v", smoothfsSrcPath, err)
		}
	}

	// Ensure /etc/modules entry exists for boot persistence.
	if err := appendModulesEntryIfMissing("/etc/modules", "smoothfs"); err != nil {
		log.Printf("updater: smoothfs: /etc/modules entry: %v", err)
	}

	// Persist the installed ref before attempting hot-reload so a reload
	// failure doesn't cause a spurious rebuild on the next update.
	if err := os.MkdirAll(filepath.Dir(smoothfsInstalledRefPath), 0755); err == nil {
		os.WriteFile(smoothfsInstalledRefPath, []byte(ref+"\n"), 0644)
	}

	// Hot-reload: succeeds only if no smoothfs filesystems are currently mounted.
	// Failure is expected and silently ignored on a live NAS.
	if execCommand("modprobe", "-r", "smoothfs").Run() == nil {
		if out, err := execCommand("modprobe", "smoothfs").CombinedOutput(); err != nil {
			log.Printf("updater: smoothfs: modprobe (non-fatal): %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	log.Printf("updater: smoothfs: installed version %s (ref %.12s)", version, ref)
	return nil
}

func appendModulesEntryIfMissing(path, module string) error {
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == module {
			return nil
		}
	}
	entry := module + "\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		entry = "\n" + entry
	}
	return appendToFile(path, entry)
}

// requiredPackages lists OS packages that tierd features depend on.
// apt-get install is a no-op for already-installed packages.
var requiredPackages = []string{
	"curl",                // Ookla repository bootstrap for speedtest CLI
	"btrfs-progs",         // mkfs.btrfs / btrfs subvolume for filesystem arrays
	"fio",                 // disk benchmarks
	"cifs-utils",          // SMB remote benchmark mounts
	"gdisk",               // sgdisk: disk preparation before array/pool creation
	"iperf3",              // local network throughput tests
	"nfs-kernel-server",   // NFS exports
	"psmisc",              // fuser: kill processes holding a mount during tier destroy
	"samba",               // SMB exports
	"unattended-upgrades", // automatic Debian security updates
	"xfsprogs",            // mkfs.xfs / xfs_growfs for tier storage
	"zfs-dkms",            // ZFS kernel module (built via DKMS for the running kernel)
	"zfs",                 // OpenZFS CLI tools from the bundled SmoothKernel repo
}

var optionalPackages = []string{
	"bcachefs-tools",       // bcachefs format/mount tooling from the bcachefs APT repo
	"bcachefs-kernel-dkms", // bcachefs out-of-tree kernel module
	"smoothfs-samba-vfs",   // exact-version Samba VFS module; install if the release repo provides it
}

// EnsureSystemPackages installs any missing OS-level dependencies.
// Failures are logged but not fatal. Safe to call at startup or concurrently.
func EnsureSystemPackages() {
	ensureDebianContrib()

	args := append([]string{"install", "-y", "-qq"}, requiredPackages...)
	cmd := execCommand("apt-get", args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("ensureSystemPackages: apt-get install failed: %v\n%s", err, out)
	}
	if err := EnsureAutomaticSecurityUpdates(); err != nil {
		log.Printf("ensureSystemPackages: failed to configure automatic security updates: %v", err)
	}
	ensureBcachefsRepo()
	ensureLinuxHeadersVirtualProvider()
	ensureOptionalPackages(optionalPackages)
	EnsureSambaVFSUpgradeGuard()
	ensureOoklaSpeedtest()
	ensureZFSModule()
}

func ensureOptionalPackages(pkgs []string) {
	for _, pkg := range pkgs {
		if isPackageInstalled(pkg) {
			continue
		}
		cmd := execCommand("apt-get", "install", "-y", "-qq", pkg)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("ensureOptionalPackages: install %s skipped: %v: %s", pkg, err, strings.TrimSpace(string(out)))
		}
	}
}

func ensureLinuxHeadersVirtualProvider() {
	if !packageListContains(optionalPackages, "bcachefs-kernel-dkms") {
		return
	}
	if isPackageInstalled("smoothkernel-headers-virtual") {
		return
	}
	out, err := execCommand("uname", "-r").Output()
	if err != nil {
		log.Printf("ensureLinuxHeadersVirtualProvider: uname -r: %v", err)
		return
	}
	kver := strings.TrimSpace(string(out))
	if !strings.Contains(kver, "smoothkernel") && !strings.Contains(kver, "smoothnas") {
		return
	}
	headersPkg := "linux-headers-" + kver
	headersVersion, ok := packageVersion(headersPkg)
	if !ok {
		log.Printf("ensureLinuxHeadersVirtualProvider: %s is not installed", headersPkg)
		return
	}

	buildDir := filepath.Join(smoothKernelHeadersProviderBuildRoot, "pkg")
	debianDir := filepath.Join(buildDir, "DEBIAN")
	debPath := filepath.Join(smoothKernelHeadersProviderBuildRoot, "smoothkernel-headers-virtual.deb")
	_ = os.RemoveAll(smoothKernelHeadersProviderBuildRoot)
	if err := os.MkdirAll(debianDir, 0755); err != nil {
		log.Printf("ensureLinuxHeadersVirtualProvider: mkdir %s: %v", debianDir, err)
		return
	}
	control := fmt.Sprintf("Package: smoothkernel-headers-virtual\nVersion: %s\nArchitecture: all\nMaintainer: SmoothNAS <root@localhost>\nProvides: linux-headers (= %s)\nDepends: %s (= %s)\nDescription: SmoothKernel linux-headers virtual provider\n Allows DKMS packages that depend on the generic linux-headers virtual package\n to use the installed SmoothKernel headers package.\n",
		headersVersion, headersVersion, headersPkg, headersVersion)
	if err := os.WriteFile(filepath.Join(debianDir, "control"), []byte(control), 0644); err != nil {
		log.Printf("ensureLinuxHeadersVirtualProvider: write control: %v", err)
		return
	}
	if out, err := execCommand("dpkg-deb", "--build", buildDir, debPath).CombinedOutput(); err != nil {
		log.Printf("ensureLinuxHeadersVirtualProvider: dpkg-deb: %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
	if out, err := execCommand("dpkg", "-i", debPath).CombinedOutput(); err != nil {
		log.Printf("ensureLinuxHeadersVirtualProvider: dpkg -i: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

func ensureBcachefsRepo() {
	if !packageListContains(optionalPackages, "bcachefs-tools") && !packageListContains(optionalPackages, "bcachefs-kernel-dkms") {
		return
	}
	codename := debianCodename()
	if codename == "" {
		log.Printf("ensureBcachefsRepo: could not detect Debian codename, skipping")
		return
	}

	changed := false
	if _, err := os.Stat(aptBcachefsKeyPath); err != nil {
		if err := os.MkdirAll(filepath.Dir(aptBcachefsKeyPath), 0755); err != nil {
			log.Printf("ensureBcachefsRepo: mkdir key dir: %v", err)
			return
		}
		cmd := execCommand("curl", "-fsSL", "-o", aptBcachefsKeyPath, "https://apt.bcachefs.org/apt.bcachefs.org.asc")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("ensureBcachefsRepo: download key: %v: %s", err, strings.TrimSpace(string(out)))
			return
		}
		changed = true
	}

	content := fmt.Sprintf("Types: deb\nURIs: https://apt.bcachefs.org/%s/\nSuites: bcachefs-tools-release\nComponents: main\nSigned-By: %s\n", codename, aptBcachefsKeyPath)
	if data, err := os.ReadFile(aptBcachefsSourcesPath); err != nil || string(data) != content {
		if err := os.MkdirAll(filepath.Dir(aptBcachefsSourcesPath), 0755); err != nil {
			log.Printf("ensureBcachefsRepo: mkdir sources dir: %v", err)
			return
		}
		if err := os.WriteFile(aptBcachefsSourcesPath, []byte(content), 0644); err != nil {
			log.Printf("ensureBcachefsRepo: write %s: %v", aptBcachefsSourcesPath, err)
			return
		}
		changed = true
	}
	if !changed {
		return
	}
	if out, err := execCommand("apt-get", "update", "-qq").CombinedOutput(); err != nil {
		log.Printf("ensureBcachefsRepo: apt-get update: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

func packageListContains(pkgs []string, name string) bool {
	for _, pkg := range pkgs {
		if pkg == name {
			return true
		}
	}
	return false
}

func debianCodename() string {
	data, err := os.ReadFile(osReleasePath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VERSION_CODENAME=") {
			return strings.Trim(strings.TrimPrefix(line, "VERSION_CODENAME="), `"`)
		}
	}
	return ""
}

// ensureDebianContrib ensures the Debian contrib component is present in apt
// sources. zfs-dkms lives in contrib; without it apt-get install silently
// fails with "package not found". If no existing source line already includes
// contrib, a dedicated drop-in file is written to sources.list.d.
func ensureDebianContrib() {
	// Check all source files for an existing contrib entry.
	files := []string{"/etc/apt/sources.list"}
	if glob, err := filepath.Glob("/etc/apt/sources.list.d/*.list"); err == nil {
		files = append(files, glob...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "deb ") && strings.Contains(line, " contrib") {
				return // already present
			}
		}
	}

	codename := debianCodename()
	if codename == "" {
		log.Printf("ensureDebianContrib: could not detect Debian codename, skipping")
		return
	}

	// Find the mirror from the first deb line in sources.list.
	mirror := "http://deb.debian.org/debian"
	if data, err := os.ReadFile("/etc/apt/sources.list"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "deb" {
				mirror = fields[1]
				break
			}
		}
	}

	content := fmt.Sprintf("deb %s %s main contrib non-free-firmware\n", mirror, codename)
	const dropIn = "/etc/apt/sources.list.d/smoothnas-contrib.list"
	if err := os.WriteFile(dropIn, []byte(content), 0644); err != nil {
		log.Printf("ensureDebianContrib: write %s: %v", dropIn, err)
		return
	}

	if out, err := execCommand("apt-get", "update", "-qq").CombinedOutput(); err != nil {
		log.Printf("ensureDebianContrib: apt-get update: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// ensureZFSModule ensures the ZFS kernel module is built, persistent, and loaded.
func ensureZFSModule() {
	const modulesFile = "/etc/modules"
	const moduleName = "zfs"

	// Add to /etc/modules if not already present (persists across reboots).
	data, _ := os.ReadFile(modulesFile)
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == moduleName {
			found = true
			break
		}
	}
	if !found {
		entry := "\n" + moduleName + "\n"
		if len(data) > 0 && data[len(data)-1] == '\n' {
			entry = moduleName + "\n"
		}
		if err := appendToFile(modulesFile, entry); err != nil {
			log.Printf("ensureZFSModule: failed to update %s: %v", modulesFile, err)
		}
	}

	// Install kernel headers for the running kernel so DKMS can build ZFS.
	// apt-get install is a no-op if already present.
	if kernelVer, err := exec.Command("uname", "-r").Output(); err == nil {
		headersPkg := "linux-headers-" + strings.TrimSpace(string(kernelVer))
		cmd := execCommand("apt-get", "install", "-y", "-qq", headersPkg)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("ensureZFSModule: install %s: %v: %s", headersPkg, err, strings.TrimSpace(string(out)))
		}
	}

	// If the module isn't loadable, the DKMS build was never triggered
	// (headers absent at install time). Reinstalling zfs-dkms re-runs its
	// postinst which registers and builds the module for the running kernel.
	if err := execCommand("modprobe", moduleName).Run(); err != nil {
		cmd := execCommand("apt-get", "install", "--reinstall", "-y", "-qq", "zfs-dkms")
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("ensureZFSModule: reinstall zfs-dkms: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	// Load immediately so pool creation works without a reboot.
	if out, err := execCommand("modprobe", moduleName).CombinedOutput(); err != nil {
		log.Printf("ensureZFSModule: modprobe zfs: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// EnsureSambaVFSUpgradeGuard pins Samba packages when the SmoothFS VFS module
// is present. The module links against Samba's private vendor-suffixed ABI, so
// Samba must not be upgraded independently of a rebuilt smoothfs.so.
func EnsureSambaVFSUpgradeGuard() {
	if !smoothfsSambaVFSInstalled() {
		return
	}
	version, ok := packageVersion("samba")
	if !ok {
		log.Printf("samba-vfs guard: smoothfs.so present but samba package version is unavailable")
		return
	}
	const path = "/etc/apt/preferences.d/smoothnas-samba-vfs"
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Printf("samba-vfs guard: create preferences dir: %v", err)
		return
	}
	content := fmt.Sprintf(sambaVFSPreferencesTemplate, version)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		log.Printf("samba-vfs guard: write %s: %v", path, err)
	}
}

func smoothfsSambaVFSInstalled() bool {
	matches, err := filepath.Glob("/usr/lib/*/samba/vfs/smoothfs.so")
	return err == nil && len(matches) > 0
}

func packageVersion(name string) (string, bool) {
	cmd := execCommand("dpkg-query", "-W", "-f=${Version}", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	version := strings.TrimSpace(string(out))
	return version, version != ""
}

func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// EnsureAutomaticSecurityUpdates installs and configures unattended upgrades
// so Debian security fixes are applied automatically.
func EnsureAutomaticSecurityUpdates() error {
	if !isPackageInstalled("unattended-upgrades") {
		if err := runAPT("update", "-qq"); err != nil {
			return fmt.Errorf("refresh package lists for unattended-upgrades: %w", err)
		}
		if err := runAPT("install", "-y", "-qq", "unattended-upgrades"); err != nil {
			return fmt.Errorf("install unattended-upgrades: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(aptAutoUpgrades), 0755); err != nil {
		return fmt.Errorf("create apt config dir: %w", err)
	}
	if err := os.WriteFile(aptAutoUpgrades, []byte(autoUpgradesConfig), 0644); err != nil {
		return fmt.Errorf("write %s: %w", aptAutoUpgrades, err)
	}
	if err := os.WriteFile(aptSecurityRules, []byte(securityOriginsConfig), 0644); err != nil {
		return fmt.Errorf("write %s: %w", aptSecurityRules, err)
	}

	cmd := execCommand("systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("enable apt timers: %v: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func ensureOoklaSpeedtest() {
	if _, err := exec.LookPath("speedtest"); err == nil {
		return
	}
	if isPackageInstalled("speedtest-cli") {
		log.Printf("ensureOoklaSpeedtest: skipping install because speedtest-cli conflicts with the official Ookla package")
		return
	}

	repoCmd := execCommand("bash", "-lc", "curl -fsSL https://packagecloud.io/install/repositories/ookla/speedtest-cli/script.deb.sh | bash")
	repoCmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if out, err := repoCmd.CombinedOutput(); err != nil {
		log.Printf("ensureOoklaSpeedtest: failed to configure repository: %v\n%s", err, out)
		return
	}

	installCmd := execCommand("apt-get", "install", "-y", "-qq", "speedtest")
	installCmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if out, err := installCmd.CombinedOutput(); err != nil {
		log.Printf("ensureOoklaSpeedtest: apt-get install failed: %v\n%s", err, out)
	}
}

func packageInstalled(name string) bool {
	cmd := execCommand("dpkg-query", "-W", "-f", "${Status}", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "install ok installed")
}

// listUpgradablePackages dry-runs apt-get upgrade and returns the names of
// packages that would be upgraded. The package lists must already be fresh
// (i.e. apt-get update has been run recently).
func listUpgradablePackages() ([]string, error) {
	cmd := execCommand("apt-get", "--simulate", "upgrade")
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list upgradable: %v: %s", err, strings.TrimSpace(string(out)))
	}

	var pkgs []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Inst ") {
			if fields := strings.Fields(line); len(fields) >= 2 {
				pkgs = append(pkgs, fields[1])
			}
		}
	}
	return pkgs, nil
}

func automaticSecurityUpdatesEnabled() bool {
	if !isPackageInstalled("unattended-upgrades") {
		return false
	}

	autoCfg, err := os.ReadFile(aptAutoUpgrades)
	if err != nil || string(autoCfg) != autoUpgradesConfig {
		return false
	}
	securityCfg, err := os.ReadFile(aptSecurityRules)
	if err != nil || string(securityCfg) != securityOriginsConfig {
		return false
	}
	return true
}

func runAPT(args ...string) error {
	cmd := execCommand("apt-get", args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

const autoUpgradesConfig = `APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
`

const securityOriginsConfig = `Unattended-Upgrade::Origins-Pattern {
	"origin=Debian,codename=${distro_codename}-security,label=Debian-Security";
};

Unattended-Upgrade::Package-Blacklist {
	"samba";
	"samba-*";
	"smbclient";
	"libsmbclient";
	"libsmbclient0";
	"libwbclient0";
	"python3-samba";
	"smoothfs-samba-vfs";
};
`

const sambaVFSPreferencesTemplate = `# Auto-generated by SmoothNAS. Do not edit.
# The smoothfs Samba VFS module is built against Samba's private ABI.
# Rebuild and reinstall smoothfs-samba-vfs before changing this pin.
Package: samba samba-* smbclient libsmbclient libsmbclient0 libwbclient0 python3-samba smoothfs-samba-vfs
Pin: version %s
Pin-Priority: 1001
`
