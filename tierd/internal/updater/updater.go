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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	owner            = "RakuenSoftware"
	repo             = "smoothnas"
	privateOwner     = "JBailes"
	privateRepo      = "SmoothNAS"
	privateRepoSSH   = "git@github.com:JBailes/SmoothNAS.git"
	privateBuildRoot = "/var/lib/tierd/private-build"
	stagingDir       = "/var/lib/tierd/update"

	binaryPath = "/usr/local/bin/tierd"
	uiPath     = "/usr/share/tierd-ui"

	cacheTTL         = 5 * time.Minute
	minCheckInterval = 1 * time.Minute
)

var channelFilePath = "/etc/tierd/update-channel"
var aptAutoUpgrades = "/etc/apt/apt.conf.d/20auto-upgrades"
var aptSecurityRules = "/etc/apt/apt.conf.d/52smoothnas-security-upgrades"
var execCommand = exec.Command
var isPackageInstalled = packageInstalled

// Channel represents an update channel.
type Channel string

const (
	ChannelStable  Channel = "stable"
	ChannelTesting Channel = "testing"
	ChannelJBailes Channel = "jbailes"
)

// Manifest matches the manifest.json uploaded with each release.
type Manifest struct {
	Version  string `json:"version"`
	Channel  string `json:"channel"`
	TierdSHA string `json:"tierd_sha256"`
	UISHA    string `json:"ui_sha256"`
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
		CurrentVersion: normalizeReleaseVersion(u.currentVersion),
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

	// Find assets.
	manifestAsset := findAsset(rel.Assets, "manifest.json")
	binaryAsset := findAsset(rel.Assets, "tierd")
	uiAsset := findAsset(rel.Assets, "tierd-ui.tar.gz")
	if manifestAsset == nil || binaryAsset == nil || uiAsset == nil {
		return fmt.Errorf("release is missing required assets (need manifest.json, tierd, tierd-ui.tar.gz)")
	}

	// Prepare staging directory.
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	// Download all artifacts.
	manifestPath := filepath.Join(stagingDir, "manifest.json")
	binaryStagePath := filepath.Join(stagingDir, "tierd")
	uiStagePath := filepath.Join(stagingDir, "tierd-ui.tar.gz")

	// Only use authenticated API downloads for the private JBailes channel;
	// public releases use browser_download_url to avoid 403s from scoped tokens.
	authenticated := u.Channel() == ChannelJBailes
	for _, dl := range []struct {
		asset *ghAsset
		dest  string
	}{
		{manifestAsset, manifestPath},
		{binaryAsset, binaryStagePath},
		{uiAsset, uiStagePath},
	} {
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

	// Verify checksums.
	if err := verifyChecksum(binaryStagePath, manifest.TierdSHA); err != nil {
		return fmt.Errorf("binary checksum: %w", err)
	}
	if err := verifyChecksum(uiStagePath, manifest.UISHA); err != nil {
		return fmt.Errorf("UI checksum: %w", err)
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

	// Ensure required OS packages are present. apt-get install is a no-op
	// for packages that are already installed, so this is safe to run every time.
	EnsureSystemPackages()

	// Clean up staging directory.
	os.RemoveAll(stagingDir)

	// Invalidate the cached check result so the new version is reflected.
	u.mu.Lock()
	u.cachedStatus = nil
	u.mu.Unlock()

	// Restart. Set the stage first, then sleep long enough for the frontend
	// to poll at least once more and see "restarting" before the process
	// dies. Without the sleep the new process starts with stage="idle" and
	// the frontend reports "Update process stopped unexpectedly".
	u.setStage("restarting")
	time.Sleep(4 * time.Second)
	exec.Command("systemctl", "restart", "tierd.service").Start()

	return nil
}

// StartManualApply begins the update from locally provided artifacts.
// The caller provides the raw contents of manifest.json, the tierd binary,
// and the tierd-ui.tar.gz archive. Returns an error if already applying.
func (u *Updater) StartManualApply(manifest, binary, ui []byte) error {
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

		if err := u.doManualApply(manifest, binary, ui); err != nil {
			log.Printf("manual update failed: %v", err)
			u.mu.Lock()
			u.progress = &ApplyProgress{Stage: "failed", Error: err.Error()}
			u.mu.Unlock()
		}
	}()

	return nil
}

func (u *Updater) doManualApply(manifestData, binaryData, uiData []byte) error {
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Prepare staging directory and write files.
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	binaryStagePath := filepath.Join(stagingDir, "tierd")
	uiStagePath := filepath.Join(stagingDir, "tierd-ui.tar.gz")

	if err := os.WriteFile(binaryStagePath, binaryData, 0644); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}
	if err := os.WriteFile(uiStagePath, uiData, 0644); err != nil {
		return fmt.Errorf("write UI archive: %w", err)
	}

	// Verify checksums.
	if err := verifyChecksum(binaryStagePath, manifest.TierdSHA); err != nil {
		return fmt.Errorf("binary checksum: %w", err)
	}
	if err := verifyChecksum(uiStagePath, manifest.UISHA); err != nil {
		return fmt.Errorf("UI checksum: %w", err)
	}

	// Install.
	u.setStage("installing")

	if err := backupAndReplace(binaryStagePath, binaryPath, 0755); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	if err := replaceUI(uiStagePath, uiPath); err != nil {
		return fmt.Errorf("replace UI: %w", err)
	}

	EnsureSystemPackages()

	os.RemoveAll(stagingDir)

	u.mu.Lock()
	u.cachedStatus = nil
	u.mu.Unlock()

	u.setStage("restarting")
	time.Sleep(4 * time.Second)
	exec.Command("systemctl", "restart", "tierd.service").Start()

	return nil
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

// requiredPackages lists OS packages that tierd features depend on.
// apt-get install is a no-op for already-installed packages.
var requiredPackages = []string{
	"curl",                // Ookla repository bootstrap for speedtest CLI
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
	"smoothfs-samba-vfs", // exact-version Samba VFS module; install if the release repo provides it
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

	// Detect the Debian codename from /etc/os-release.
	codename := ""
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VERSION_CODENAME=") {
				codename = strings.Trim(strings.TrimPrefix(line, "VERSION_CODENAME="), `"`)
				break
			}
		}
	}
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
