package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"
)

func withAppliedVersionPath(t *testing.T, path string) {
	t.Helper()
	original := appliedVersionPath
	appliedVersionPath = path
	t.Cleanup(func() { appliedVersionPath = original })
}

func TestEffectiveCurrentVersionFallsBackToBuildTime(t *testing.T) {
	withAppliedVersionPath(t, filepath.Join(t.TempDir(), "applied-version"))
	u := New("0.0.0-dev")
	if got := u.effectiveCurrentVersion(); got != "0.0.0-dev" {
		t.Fatalf("missing file: got %q, want build-time fallback %q", got, "0.0.0-dev")
	}
}

func TestEffectiveCurrentVersionPrefersAppliedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied-version")
	withAppliedVersionPath(t, path)

	if err := os.WriteFile(path, []byte("  2026.0508.1253-c7718a8\n  "), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := New("0.0.0-dev")
	if got := u.effectiveCurrentVersion(); got != "2026.0508.1253-c7718a8" {
		t.Fatalf("with file: got %q, want trimmed file content", got)
	}

	// Empty file → fall back to build-time.
	if err := os.WriteFile(path, []byte("\n"), 0o644); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	if got := u.effectiveCurrentVersion(); got != "0.0.0-dev" {
		t.Fatalf("empty file: got %q, want build-time fallback", got)
	}
}

func TestWriteAppliedVersionCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "applied-version")
	withAppliedVersionPath(t, path)

	writeAppliedVersion("v0.0.46")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "v0.0.46"; got != want {
		t.Fatalf("file content: got %q, want %q", got, want)
	}

	// Empty input is a no-op (don't clobber any existing record).
	writeAppliedVersion("")
	if data, _ := os.ReadFile(path); strings.TrimSpace(string(data)) != "v0.0.46" {
		t.Fatal("empty input clobbered existing applied-version file")
	}
}

func TestTierdAssetNameForArch(t *testing.T) {
	cases := map[string]string{
		"amd64": "tierd-amd64",
		"arm64": "tierd-arm64",
		"":      "tierd",
		"riscv": "tierd", // unknown architectures fall back to legacy single-arch name
	}
	for arch, want := range cases {
		if got := tierdAssetNameForArch(arch); got != want {
			t.Errorf("tierdAssetNameForArch(%q) = %q, want %q", arch, got, want)
		}
	}
}

func TestManifestTierdSHAForArch(t *testing.T) {
	multi := Manifest{TierdAmd64SHA: "AAA", TierdArm64SHA: "BBB"}
	if got := multi.TierdSHAForArch("amd64"); got != "AAA" {
		t.Errorf("multi.amd64 = %q, want AAA", got)
	}
	if got := multi.TierdSHAForArch("arm64"); got != "BBB" {
		t.Errorf("multi.arm64 = %q, want BBB", got)
	}
	if got := multi.TierdSHAForArch("riscv"); got != "" {
		t.Errorf("multi.riscv = %q, want \"\"", got)
	}

	// Legacy single-arch manifest (`tierd_sha256` only) — both arches
	// fall through to the legacy hash.
	legacy := Manifest{TierdSHA: "OLD"}
	if got := legacy.TierdSHAForArch("amd64"); got != "OLD" {
		t.Errorf("legacy.amd64 = %q, want OLD", got)
	}
	if got := legacy.TierdSHAForArch("arm64"); got != "OLD" {
		t.Errorf("legacy.arm64 = %q, want OLD", got)
	}

	// Mixed: arch-specific takes precedence, legacy used only if arch field empty.
	mixed := Manifest{TierdAmd64SHA: "AAA", TierdSHA: "OLD"}
	if got := mixed.TierdSHAForArch("amd64"); got != "AAA" {
		t.Errorf("mixed.amd64 = %q, want AAA", got)
	}
	if got := mixed.TierdSHAForArch("arm64"); got != "OLD" {
		t.Errorf("mixed.arm64 (no arm64 field, has legacy) = %q, want OLD", got)
	}

	// Real-shape manifest decoded from JSON to lock the wire format.
	raw := `{"version":"v1","tierd_amd64_sha256":"AAA","tierd_arm64_sha256":"BBB","runtime_amd64_sha256":"RRA","runtime_arm64_sha256":"RRB","ui_sha256":"UUU"}`
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.TierdAmd64SHA != "AAA" || m.TierdArm64SHA != "BBB" || m.RuntimeAmd64SHA != "RRA" || m.RuntimeArm64SHA != "RRB" || m.UISHA != "UUU" {
		t.Fatalf("decoded manifest fields wrong: %+v", m)
	}
}

func withChannelFilePath(t *testing.T, path string) {
	t.Helper()
	original := channelFilePath
	channelFilePath = path
	t.Cleanup(func() {
		channelFilePath = original
	})
}

func withPrivateRepoInspector(t *testing.T, fn func(string) (*privateRepoInfo, error)) {
	t.Helper()
	original := inspectPrivateRepoForUser
	inspectPrivateRepoForUser = fn
	t.Cleanup(func() {
		inspectPrivateRepoForUser = original
	})
}

func withSystemUserLookup(t *testing.T, fn func(string) (*sgauth.User, error)) {
	t.Helper()
	original := lookupSystemUser
	lookupSystemUser = fn
	t.Cleanup(func() {
		lookupSystemUser = original
	})
}

func withGitHubTokenFilePath(t *testing.T, path string) {
	t.Helper()
	original := githubTokenFilePath
	githubTokenFilePath = path
	t.Cleanup(func() {
		githubTokenFilePath = original
	})
}

func withExecCommand(t *testing.T, fn func(string, ...string) *exec.Cmd) {
	t.Helper()
	original := execCommand
	execCommand = fn
	t.Cleanup(func() {
		execCommand = original
	})
}

func withPackageInstalledCheck(t *testing.T, fn func(string) bool) {
	t.Helper()
	original := isPackageInstalled
	isPackageInstalled = fn
	t.Cleanup(func() {
		isPackageInstalled = original
	})
}

func withAPTConfigPaths(t *testing.T, autoPath, securityPath string) {
	t.Helper()
	originalAuto := aptAutoUpgrades
	originalSecurity := aptSecurityRules
	aptAutoUpgrades = autoPath
	aptSecurityRules = securityPath
	t.Cleanup(func() {
		aptAutoUpgrades = originalAuto
		aptSecurityRules = originalSecurity
	})
}

func withBcachefsRepoPaths(t *testing.T, keyPath, sourcesPath, releasePath string) {
	t.Helper()
	originalKey := aptBcachefsKeyPath
	originalSources := aptBcachefsSourcesPath
	originalRelease := osReleasePath
	aptBcachefsKeyPath = keyPath
	aptBcachefsSourcesPath = sourcesPath
	osReleasePath = releasePath
	t.Cleanup(func() {
		aptBcachefsKeyPath = originalKey
		aptBcachefsSourcesPath = originalSources
		osReleasePath = originalRelease
	})
}

func withSmoothKernelHeadersProviderBuildRoot(t *testing.T, path string) {
	t.Helper()
	original := smoothKernelHeadersProviderBuildRoot
	smoothKernelHeadersProviderBuildRoot = path
	t.Cleanup(func() {
		smoothKernelHeadersProviderBuildRoot = original
	})
}

func withOptionalPackages(t *testing.T, pkgs []string) {
	t.Helper()
	original := optionalPackages
	optionalPackages = pkgs
	t.Cleanup(func() {
		optionalPackages = original
	})
}

func flattenCalls(calls [][]string) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, strings.Join(call, " "))
	}
	return out
}

func TestGitCommandForUserSetsCredentialAndEnv(t *testing.T) {
	withSystemUserLookup(t, func(username string) (*sgauth.User, error) {
		if username != "JBailes" {
			t.Fatalf("username = %q, want JBailes", username)
		}
		return &sgauth.User{
			Username: "JBailes",
			UID:      "1001",
			GID:      "1002",
			Home:     "/home/JBailes",
		}, nil
	})

	cmd, err := gitCommandForUser("JBailes", "ls-remote", "git@github.com:JBailes/SmoothNAS.git", "HEAD")
	if err != nil {
		t.Fatalf("gitCommandForUser: %v", err)
	}
	if !strings.HasSuffix(cmd.Path, "/git") && cmd.Path != "git" {
		t.Fatalf("cmd.Path = %q, want git executable", cmd.Path)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected credential to be set on git command")
	}
	got := cmd.SysProcAttr.Credential
	want := &syscall.Credential{Uid: 1001, Gid: 1002}
	if got.Uid != want.Uid || got.Gid != want.Gid {
		t.Fatalf("credential = %#v, want %#v", got, want)
	}

	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{
		"HOME=/home/JBailes",
		"USER=JBailes",
		"LOGNAME=JBailes",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("expected env to contain %q", want)
		}
	}
}

func TestEnsureBcachefsRepoWritesSourceAndUpdatesApt(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "trusted.gpg.d", "apt.bcachefs.org.asc")
	sourcesPath := filepath.Join(dir, "sources.list.d", "apt.bcachefs.org.sources")
	releasePath := filepath.Join(dir, "os-release")
	if err := os.WriteFile(releasePath, []byte("VERSION_CODENAME=trixie\n"), 0644); err != nil {
		t.Fatalf("write os-release: %v", err)
	}
	withBcachefsRepoPaths(t, keyPath, sourcesPath, releasePath)
	withOptionalPackages(t, []string{"bcachefs-tools", "bcachefs-kernel-dkms"})

	var calls [][]string
	withExecCommand(t, func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		if name == "curl" && len(args) >= 3 {
			return exec.Command("bash", "-lc", "printf key > \"$1\"", "sh", args[2])
		}
		return exec.Command("bash", "-lc", "true")
	})

	ensureBcachefsRepo()

	source, err := os.ReadFile(sourcesPath)
	if err != nil {
		t.Fatalf("read bcachefs source: %v", err)
	}
	gotSource := string(source)
	for _, want := range []string{
		"URIs: https://apt.bcachefs.org/trixie/",
		"Suites: bcachefs-tools-release",
		"Signed-By: " + keyPath,
	} {
		if !strings.Contains(gotSource, want) {
			t.Fatalf("source missing %q:\n%s", want, gotSource)
		}
	}
	gotCalls := strings.Join(flattenCalls(calls), "\n")
	for _, want := range []string{
		"curl -fsSL -o " + keyPath + " https://apt.bcachefs.org/apt.bcachefs.org.asc",
		"apt-get update -qq",
	} {
		if !strings.Contains(gotCalls, want) {
			t.Fatalf("expected command %q, got:\n%s", want, gotCalls)
		}
	}
}

func TestEnsureLinuxHeadersVirtualProviderBuildsLocalPackage(t *testing.T) {
	dir := t.TempDir()
	withSmoothKernelHeadersProviderBuildRoot(t, dir)
	withOptionalPackages(t, []string{"bcachefs-kernel-dkms"})
	withPackageInstalledCheck(t, func(name string) bool {
		return false
	})

	var calls [][]string
	withExecCommand(t, func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		switch name {
		case "uname":
			return exec.Command("bash", "-lc", "printf 6.19.12-smoothkernel")
		case "dpkg-query":
			return exec.Command("bash", "-lc", "printf 6.19.12-1")
		default:
			return exec.Command("bash", "-lc", "true")
		}
	})

	ensureLinuxHeadersVirtualProvider()

	control, err := os.ReadFile(filepath.Join(dir, "pkg", "DEBIAN", "control"))
	if err != nil {
		t.Fatalf("read control: %v", err)
	}
	gotControl := string(control)
	for _, want := range []string{
		"Package: smoothkernel-headers-virtual",
		"Provides: linux-headers (= 6.19.12-1)",
		"Depends: linux-headers-6.19.12-smoothkernel (= 6.19.12-1)",
	} {
		if !strings.Contains(gotControl, want) {
			t.Fatalf("control missing %q:\n%s", want, gotControl)
		}
	}
	gotCalls := strings.Join(flattenCalls(calls), "\n")
	for _, want := range []string{
		"dpkg-deb --build " + filepath.Join(dir, "pkg") + " " + filepath.Join(dir, "smoothkernel-headers-virtual.deb"),
		"dpkg -i " + filepath.Join(dir, "smoothkernel-headers-virtual.deb"),
	} {
		if !strings.Contains(gotCalls, want) {
			t.Fatalf("expected command %q, got:\n%s", want, gotCalls)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		current, latest string
		wantNewer       bool
	}{
		// Semver.
		{"0.1.0", "0.1.1", true},
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "1.0.0", true},
		{"0.1.0", "0.1.0", false},
		{"0.2.0", "0.1.0", false},
		{"1.0.0", "0.9.9", false},
		{"v0.1.0", "v0.2.0", true},
		{"0.1.0", "v0.1.1", true},
		// Skipping multiple versions.
		{"0.1.0", "0.1.4", true},
		{"0.1.0", "0.5.0", true},
		{"0.1.0", "2.0.0", true},
		// Date+time versions (YYYY.MMDD.HHMM-sha).
		{"2026.0401.0900-abc1234", "2026.0405.1200-def5678", true},
		{"2026.0405.1200-abc1234", "2026.0405.1200-abc1234", false},
		{"2026.0405.0900-abc1234", "2026.0405.1423-def5678", true},  // same date, later time
		{"2026.0405.1423-abc1234", "2026.0405.0900-def5678", false}, // same date, earlier time
		{"2026.0315.1200-abc1234", "2026.0401.0900-def5678", true},
		{"2025.1231.2359-abc1234", "2026.0101.0001-def5678", true},
		// Testing-prefixed tags.
		{"2026.0401.0900-abc1234", "testing-2026.0405.1200-def5678", true},
		{"testing-2026.0401.0900-abc1234", "testing-2026.0405.1200-def5678", true},
		// Cross-scheme: calendar testing build → semver stable release.
		// Numeric comparison would wrongly say 2026 > 0.4.2 (no update), so
		// cross-scheme always reports an update as available.
		{"2026.0410.2058-adaa145", "0.4.2", true},
		{"2026.0410.2058-adaa145", "0.4.0", true},
		// Cross-scheme: semver → calendar (switching to testing channel).
		{"0.4.2", "2026.0410.2058-adaa145", true},
	}

	for _, tt := range tests {
		newer, err := compareVersions(tt.current, tt.latest)
		if err != nil {
			t.Errorf("compareVersions(%q, %q): unexpected error: %v", tt.current, tt.latest, err)
			continue
		}
		if newer != tt.wantNewer {
			t.Errorf("compareVersions(%q, %q) = %v, want %v", tt.current, tt.latest, newer, tt.wantNewer)
		}
	}
}

func TestCompareVersionsInvalid(t *testing.T) {
	tests := []struct{ a, b string }{
		{"bad", "0.1.0"},
		{"0.1.0", "bad"},
		{"0.1", "0.1.0"},
	}
	for _, tt := range tests {
		if _, err := compareVersions(tt.a, tt.b); err == nil {
			t.Errorf("compareVersions(%q, %q): expected error", tt.a, tt.b)
		}
	}
}

func TestEnsureAutomaticSecurityUpdatesWritesConfig(t *testing.T) {
	var calls [][]string
	withExecCommand(t, func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.Command("bash", "-lc", "true")
	})
	withPackageInstalledCheck(t, func(name string) bool {
		return false
	})
	withAPTConfigPaths(
		t,
		filepath.Join(t.TempDir(), "20auto-upgrades"),
		filepath.Join(t.TempDir(), "52smoothnas-security-upgrades"),
	)

	if err := EnsureAutomaticSecurityUpdates(); err != nil {
		t.Fatalf("EnsureAutomaticSecurityUpdates: %v", err)
	}

	autoCfg, err := os.ReadFile(aptAutoUpgrades)
	if err != nil {
		t.Fatalf("read auto upgrades config: %v", err)
	}
	if string(autoCfg) != autoUpgradesConfig {
		t.Fatalf("auto config = %q, want %q", string(autoCfg), autoUpgradesConfig)
	}

	securityCfg, err := os.ReadFile(aptSecurityRules)
	if err != nil {
		t.Fatalf("read security config: %v", err)
	}
	if string(securityCfg) != securityOriginsConfig {
		t.Fatalf("security config = %q, want %q", string(securityCfg), securityOriginsConfig)
	}
	for _, want := range []string{`"samba";`, `"samba-*";`, `"smoothfs-samba-vfs";`} {
		if !strings.Contains(string(securityCfg), want) {
			t.Fatalf("security config missing %s:\n%s", want, securityCfg)
		}
	}

	got := strings.Join(flattenCalls(calls), "\n")
	for _, want := range []string{
		"apt-get update -qq",
		"apt-get install -y -qq unattended-upgrades",
		"systemctl enable --now apt-daily.timer apt-daily-upgrade.timer",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected commands to contain %q, got:\n%s", want, got)
		}
	}
}

func TestDoDebianPackageApplyRunsUpgrade(t *testing.T) {
	var calls [][]string
	withExecCommand(t, func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.Command("bash", "-lc", "true")
	})
	withPackageInstalledCheck(t, func(name string) bool {
		return true
	})
	withAPTConfigPaths(
		t,
		filepath.Join(t.TempDir(), "20auto-upgrades"),
		filepath.Join(t.TempDir(), "52smoothnas-security-upgrades"),
	)

	u := New("1.2.3")
	if err := u.doDebianPackageApply(); err != nil {
		t.Fatalf("doDebianPackageApply: %v", err)
	}

	got := strings.Join(flattenCalls(calls), "\n")
	for _, want := range []string{
		"systemctl enable --now apt-daily.timer apt-daily-upgrade.timer",
		"apt-get update -qq",
		"apt-get upgrade -y -qq",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected commands to contain %q, got:\n%s", want, got)
		}
	}
}

func TestCheckNoUpdate(t *testing.T) {
	releases := []ghRelease{
		{
			TagName:     "v0.1.0",
			Body:        "No changes",
			PublishedAt: "2026-01-01T00:00:00Z",
			Prerelease:  false,
			Assets:      []ghAsset{},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	u := New("0.1.0")
	u.githubBaseURL = srv.URL

	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status.Available {
		t.Error("expected no update available")
	}
	if status.CurrentVersion != "0.1.0" {
		t.Errorf("current version = %q, want %q", status.CurrentVersion, "0.1.0")
	}
	if status.Channel != ChannelStable {
		t.Errorf("channel = %q, want %q", status.Channel, ChannelStable)
	}
}

func TestCheckUpdateAvailable(t *testing.T) {
	releases := []ghRelease{
		{
			TagName:     "testing-2026.0405.1200-def5678",
			Body:        "Testing build",
			PublishedAt: "2026-04-05T00:00:00Z",
			Prerelease:  true,
			Assets:      []ghAsset{},
		},
		{
			TagName:     "v0.2.0",
			Body:        "New features",
			PublishedAt: "2026-02-01T00:00:00Z",
			Prerelease:  false,
			Assets:      []ghAsset{},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	u := New("0.1.0")
	u.githubBaseURL = srv.URL

	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !status.Available {
		t.Error("expected update available")
	}
	if status.Latest == nil {
		t.Fatal("expected latest info")
	}
	// Should pick the stable v0.2.0, not the testing release.
	if status.Latest.Version != "0.2.0" {
		t.Errorf("latest version = %q, want %q", status.Latest.Version, "0.2.0")
	}
	if status.Latest.Body != "New features" {
		t.Errorf("body = %q, want %q", status.Latest.Body, "New features")
	}
}

func TestCheckTestingDefaultsToTestingChannelAndNotes(t *testing.T) {
	releases := []ghRelease{
		{
			TagName:     "testing-2026.0405.1200-def5678",
			Body:        "Testing release notes",
			PublishedAt: "2026-04-05T00:00:00Z",
			Prerelease:  true,
			Assets:      []ghAsset{},
		},
		{
			TagName:     "v0.2.0",
			Body:        "Stable release notes",
			PublishedAt: "2026-02-01T00:00:00Z",
			Prerelease:  false,
			Assets:      []ghAsset{},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))

	u := New("2026.0401.0900-abc1234")
	u.githubBaseURL = srv.URL

	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status.Channel != ChannelTesting {
		t.Fatalf("channel = %q, want %q", status.Channel, ChannelTesting)
	}
	if !status.Available {
		t.Fatal("expected update available")
	}
	if status.CurrentVersion != "2026.0401.0900-abc1234" {
		t.Fatalf("current version = %q, want %q", status.CurrentVersion, "2026.0401.0900-abc1234")
	}
	if status.Latest == nil {
		t.Fatal("expected latest info")
	}
	if status.Latest.Version != "2026.0405.1200-def5678" {
		t.Errorf("latest version = %q, want %q", status.Latest.Version, "2026.0405.1200-def5678")
	}
	if status.Latest.Body != "Testing release notes" {
		t.Errorf("body = %q, want %q", status.Latest.Body, "Testing release notes")
	}
}

func TestCheckStableIgnoresTestingReleases(t *testing.T) {
	// When on the stable channel, only v*-tagged non-prerelease releases
	// should be considered, even if a newer testing release exists.
	releases := []ghRelease{
		{
			TagName:     "testing-2026.0405.1200-abc1234",
			Body:        "Testing build",
			PublishedAt: "2026-04-05T00:00:00Z",
			Prerelease:  true,
			Assets:      []ghAsset{},
		},
		{
			TagName:     "v0.2.0",
			Body:        "Stable release",
			PublishedAt: "2026-02-01T00:00:00Z",
			Prerelease:  false,
			Assets:      []ghAsset{},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	u := New("0.1.0")
	u.githubBaseURL = srv.URL

	// Default channel is stable.
	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !status.Available {
		t.Error("expected update available")
	}
	if status.Latest.Version != "0.2.0" {
		t.Errorf("latest version = %q, want %q", status.Latest.Version, "0.2.0")
	}
}

func TestFetchLatestPrerelease(t *testing.T) {
	releases := []ghRelease{
		{
			TagName:     "v0.2.0",
			Prerelease:  false,
			PublishedAt: "2026-02-01T00:00:00Z",
		},
		{
			TagName:     "testing-2026.04.05-abc1234",
			Body:        "Testing build",
			Prerelease:  true,
			PublishedAt: "2026-04-05T00:00:00Z",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	rel, err := fetchLatestPrerelease(srv.URL, "RakuenSoftware", "smoothnas")
	if err != nil {
		t.Fatalf("fetchLatestPrerelease: %v", err)
	}
	if rel.TagName != "testing-2026.04.05-abc1234" {
		t.Errorf("tag = %q, want %q", rel.TagName, "testing-2026.04.05-abc1234")
	}
	if !rel.Prerelease {
		t.Error("expected prerelease=true")
	}
}

func TestFetchLatestPrereleaseNone(t *testing.T) {
	releases := []ghRelease{
		{TagName: "v0.2.0", Prerelease: false},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	_, err := fetchLatestPrerelease(srv.URL, "RakuenSoftware", "smoothnas")
	if err == nil {
		t.Error("expected error when no prereleases exist")
	}
}

func TestFetchLatestReleaseIgnoresTestingTags(t *testing.T) {
	// A prerelease with a testing- tag should be skipped by fetchLatestRelease.
	releases := []ghRelease{
		{TagName: "testing-2026.0405.1200-abc1234", Prerelease: true},
		{TagName: "v0.2.0", Prerelease: false},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	rel, err := fetchLatestRelease(srv.URL, "RakuenSoftware", "smoothnas")
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.2.0" {
		t.Errorf("tag = %q, want %q", rel.TagName, "v0.2.0")
	}
}

func TestFetchLatestPrereleaseIgnoresStableTags(t *testing.T) {
	// A non-prerelease with a v tag should be skipped by fetchLatestPrerelease.
	releases := []ghRelease{
		{TagName: "v0.3.0", Prerelease: false},
		{TagName: "testing-2026.0405.1200-abc1234", Prerelease: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	rel, err := fetchLatestPrerelease(srv.URL, "RakuenSoftware", "smoothnas")
	if err != nil {
		t.Fatalf("fetchLatestPrerelease: %v", err)
	}
	if rel.TagName != "testing-2026.0405.1200-abc1234" {
		t.Errorf("tag = %q, want %q", rel.TagName, "testing-2026.0405.1200-abc1234")
	}
}

func TestChannelDefaults(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))

	u := New("0.1.0")
	if u.Channel() != ChannelStable {
		t.Errorf("default channel = %q, want %q", u.Channel(), ChannelStable)
	}
}

func TestChannelDefaultsToTestingForTestingBuilds(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))

	u := New("2026.0405.1200-abc1234")
	if u.Channel() != ChannelTesting {
		t.Errorf("default channel = %q, want %q", u.Channel(), ChannelTesting)
	}
}

func TestSetChannelInvalid(t *testing.T) {
	u := New("0.1.0")
	if err := u.SetChannel("nightly"); err == nil {
		t.Error("expected error for invalid channel")
	}
}

func TestSetChannelRejectsInaccessibleJBailes(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	u := New("0.1.0")
	u.githubBaseURL = srv.URL
	err := u.SetChannel(ChannelJBailes)
	if err == nil {
		t.Fatal("expected error for inaccessible JBailes release channel")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetChannelPersistsJBailes(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ghRelease{
			{
				TagName:     "testing-2026.0405.1200-def5678",
				Body:        "JBailes release",
				PublishedAt: "2026-04-05T12:00:00Z",
				Prerelease:  true,
				Assets: []ghAsset{
					{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
					{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
					{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
				},
			},
		})
	}))
	defer srv.Close()

	u := New("0.1.0")
	u.githubBaseURL = srv.URL
	if err := u.SetChannel(ChannelJBailes); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	if got := u.Channel(); got != ChannelJBailes {
		t.Fatalf("channel = %q, want %q", got, ChannelJBailes)
	}
}

func TestCheckIncludesJBailesChannel(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RakuenSoftware/smoothnas/releases":
			json.NewEncoder(w).Encode([]ghRelease{
				{
					TagName:     "testing-2026.0405.1200-def5678",
					Body:        "Testing build",
					PublishedAt: "2026-04-05T00:00:00Z",
					Prerelease:  true,
				},
				{
					TagName:     "v0.2.0",
					Body:        "Stable build",
					PublishedAt: "2026-02-01T00:00:00Z",
					Prerelease:  false,
				},
			})
		case "/repos/JBailes/SmoothNAS/releases":
			json.NewEncoder(w).Encode([]ghRelease{
				{
					TagName:     "testing-2026.0405.1300-abc1234",
					Body:        "Repo: JBailes/SmoothNAS",
					PublishedAt: "2026-04-05T13:00:00Z",
					Prerelease:  true,
					Assets: []ghAsset{
						{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
						{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
						{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := New("2026.0401.0900-abc1234")
	u.githubBaseURL = srv.URL
	if err := os.WriteFile(channelFilePath, []byte(string(ChannelJBailes)+"\n"), 0600); err != nil {
		t.Fatalf("write channel file: %v", err)
	}

	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status.Channel != ChannelJBailes {
		t.Fatalf("channel = %q, want %q", status.Channel, ChannelJBailes)
	}
	if status.JBailes == nil {
		t.Fatal("expected JBailes release info")
	}
	if status.JBailes.Version != "2026.0405.1300-abc1234" {
		t.Fatalf("jbailes version = %q", status.JBailes.Version)
	}
	if status.Latest == nil || status.Latest.Version != status.JBailes.Version {
		t.Fatalf("latest = %#v, want JBailes release", status.Latest)
	}
	if !status.Available {
		t.Fatal("expected JBailes update to be available")
	}
}

func TestCheckKeepsPublicChannelsWhenJBailesIsInaccessible(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RakuenSoftware/smoothnas/releases":
			json.NewEncoder(w).Encode([]ghRelease{
				{
					TagName:     "testing-2026.0405.1200-def5678",
					Body:        "Testing build",
					PublishedAt: "2026-04-05T00:00:00Z",
					Prerelease:  true,
				},
				{
					TagName:     "v0.2.0",
					Body:        "Stable build",
					PublishedAt: "2026-02-01T00:00:00Z",
					Prerelease:  false,
				},
			})
		case "/repos/JBailes/SmoothNAS/releases":
			w.WriteHeader(http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := New("2026.0401.0900-abc1234")
	u.githubBaseURL = srv.URL
	if err := os.WriteFile(channelFilePath, []byte(string(ChannelJBailes)+"\n"), 0600); err != nil {
		t.Fatalf("write channel file: %v", err)
	}

	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status.Channel != ChannelJBailes {
		t.Fatalf("channel = %q, want %q", status.Channel, ChannelJBailes)
	}
	if status.Stable == nil || status.Stable.Version != "0.2.0" {
		t.Fatalf("stable = %#v, want v0.2.0 metadata", status.Stable)
	}
	if status.Testing == nil || status.Testing.Version != "2026.0405.1200-def5678" {
		t.Fatalf("testing = %#v, want testing metadata", status.Testing)
	}
	if status.JBailes != nil {
		t.Fatalf("jbailes = %#v, want nil when fork releases are inaccessible", status.JBailes)
	}
	if status.Latest != nil {
		t.Fatalf("latest = %#v, want nil when current JBailes channel is inaccessible", status.Latest)
	}
	if status.Available {
		t.Fatal("expected no update when current JBailes channel is inaccessible")
	}
}

func TestCheckKeepsPublicChannelsVisibleWithJBailesScopedToken(t *testing.T) {
	withChannelFilePath(t, filepath.Join(t.TempDir(), "update-channel"))

	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	withGitHubTokenFilePath(t, tokenFile)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RakuenSoftware/smoothnas/releases":
			if got := r.Header.Get("Authorization"); got != "" {
				http.Error(w, "public repo should not receive scoped token", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode([]ghRelease{
				{
					TagName:     "testing-2026.0405.1200-def5678",
					Body:        "Testing build",
					PublishedAt: "2026-04-05T00:00:00Z",
					Prerelease:  true,
				},
				{
					TagName:     "v0.2.0",
					Body:        "Stable build",
					PublishedAt: "2026-02-01T00:00:00Z",
					Prerelease:  false,
				},
			})
		case "/repos/JBailes/SmoothNAS/releases":
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				http.Error(w, "missing fork token", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode([]ghRelease{
				{
					TagName:     "testing-2026.0405.1300-abc1234",
					Body:        "Repo: JBailes/SmoothNAS",
					PublishedAt: "2026-04-05T13:00:00Z",
					Prerelease:  true,
					Assets: []ghAsset{
						{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
						{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
						{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := New("2026.0401.0900-abc1234")
	u.githubBaseURL = srv.URL
	if err := u.SetChannel(ChannelJBailes); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}

	status, err := u.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status.Stable == nil || status.Stable.Version != "0.2.0" {
		t.Fatalf("stable = %#v, want v0.2.0 metadata", status.Stable)
	}
	if status.Testing == nil || status.Testing.Version != "2026.0405.1200-def5678" {
		t.Fatalf("testing = %#v, want testing metadata", status.Testing)
	}
	if status.JBailes == nil || status.JBailes.Version != "2026.0405.1300-abc1234" {
		t.Fatalf("jbailes = %#v, want JBailes metadata", status.JBailes)
	}
}

func TestFetchLatestArtifactRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ghRelease{
			{
				TagName:     "testing-2026.0405.1400-naked",
				PublishedAt: "2026-04-05T14:00:00Z",
				Prerelease:  true,
				Assets: []ghAsset{
					{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
				},
			},
			{
				TagName:     "testing-2026.0405.1300-abc1234",
				PublishedAt: "2026-04-05T13:00:00Z",
				Prerelease:  true,
				Assets: []ghAsset{
					{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
					{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
					{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
				},
			},
		})
	}))
	defer srv.Close()

	rel, err := fetchLatestArtifactRelease(srv.URL, "JBailes", "SmoothNAS")
	if err != nil {
		t.Fatalf("fetchLatestArtifactRelease: %v", err)
	}
	if rel.TagName != "testing-2026.0405.1300-abc1234" {
		t.Fatalf("tag = %q, want artifact-bearing release", rel.TagName)
	}
}

func TestFetchLatestArtifactReleasePrefersTestingPrerelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ghRelease{
			{
				TagName:     "v0.2.0",
				PublishedAt: "2026-04-06T14:00:00Z",
				Prerelease:  false,
				Assets: []ghAsset{
					{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
					{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
					{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
				},
			},
			{
				TagName:     "testing-2026.0405.1300-abc1234",
				PublishedAt: "2026-04-05T13:00:00Z",
				Prerelease:  true,
				Assets: []ghAsset{
					{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
					{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
					{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
				},
			},
		})
	}))
	defer srv.Close()

	rel, err := fetchLatestArtifactRelease(srv.URL, "JBailes", "SmoothNAS")
	if err != nil {
		t.Fatalf("fetchLatestArtifactRelease: %v", err)
	}
	if rel.TagName != "testing-2026.0405.1300-abc1234" {
		t.Fatalf("tag = %q, want testing prerelease artifact", rel.TagName)
	}
}

func TestFetchReleasesUsesGitHubToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	withGitHubTokenFilePath(t, tokenFile)

	sawAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		sawAuth = true
		json.NewEncoder(w).Encode([]ghRelease{})
	}))
	defer srv.Close()

	if _, err := fetchReleases(srv.URL, "JBailes", "SmoothNAS", true); err != nil {
		t.Fatalf("fetchReleases: %v", err)
	}
	if !sawAuth {
		t.Fatal("expected Authorization header to be sent")
	}
}

func TestFetchReleasesSkipsGitHubTokenForPublicRepos(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	withGitHubTokenFilePath(t, tokenFile)

	sawAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			sawAuth = true
			t.Fatalf("authorization = %q, want no bearer token", got)
		}
		json.NewEncoder(w).Encode([]ghRelease{})
	}))
	defer srv.Close()

	if _, err := fetchReleases(srv.URL, "RakuenSoftware", "smoothnas", false); err != nil {
		t.Fatalf("fetchReleases: %v", err)
	}
	if sawAuth {
		t.Fatal("did not expect Authorization header on public repo request")
	}
}

func TestDownloadAssetUsesAPIURLWhenGitHubTokenPresent(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	withGitHubTokenFilePath(t, tokenFile)

	var hitAPI, hitBrowser bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/assets/1":
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("authorization = %q, want bearer token", got)
			}
			hitAPI = true
			io.WriteString(w, "asset-body")
		case "/download/tierd":
			hitBrowser = true
			http.Error(w, "browser url should not be used", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "tierd")
	asset := &ghAsset{
		URL:                srv.URL + "/api/assets/1",
		Name:               "tierd",
		BrowserDownloadURL: srv.URL + "/download/tierd",
	}
	if err := downloadAsset(asset, dest, true); err != nil {
		t.Fatalf("downloadAsset: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded asset: %v", err)
	}
	if string(data) != "asset-body" {
		t.Fatalf("downloaded body = %q", string(data))
	}
	if !hitAPI {
		t.Fatal("expected authenticated asset API URL to be used")
	}
	if hitBrowser {
		t.Fatal("did not expect browser download URL to be used when token is present")
	}
}

func TestDownloadAssetUsesBrowserURLForPublicAsset(t *testing.T) {
	// Even when a GitHub token is configured, public-channel assets
	// (authenticated=false) must use browser_download_url to avoid HTTP 403
	// from the GitHub API when the token has insufficient scope.
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	withGitHubTokenFilePath(t, tokenFile)

	var hitAPI, hitBrowser bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/assets/1":
			hitAPI = true
			http.Error(w, "should not use API URL for public asset", http.StatusForbidden)
		case "/download/tierd":
			hitBrowser = true
			io.WriteString(w, "browser-body")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "tierd")
	asset := &ghAsset{
		URL:                srv.URL + "/api/assets/1",
		Name:               "tierd",
		BrowserDownloadURL: srv.URL + "/download/tierd",
	}
	if err := downloadAsset(asset, dest, false); err != nil {
		t.Fatalf("downloadAsset: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded asset: %v", err)
	}
	if string(data) != "browser-body" {
		t.Fatalf("downloaded body = %q, want browser-body", string(data))
	}
	if hitAPI {
		t.Fatal("did not expect API URL to be used for public asset download")
	}
	if !hitBrowser {
		t.Fatal("expected browser_download_url to be used for public asset")
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")

	content := []byte("hello world")
	os.WriteFile(path, content, 0644)

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	if err := verifyChecksum(path, expected); err != nil {
		t.Fatalf("verifyChecksum: %v", err)
	}

	if err := verifyChecksum(path, "badhash"); err == nil {
		t.Error("expected checksum mismatch error")
	}
}

func TestFindAssetURL(t *testing.T) {
	assets := []ghAsset{
		{Name: "tierd", BrowserDownloadURL: "https://example.com/tierd"},
		{Name: "manifest.json", BrowserDownloadURL: "https://example.com/manifest.json"},
		{Name: "tierd-ui.tar.gz", BrowserDownloadURL: "https://example.com/tierd-ui.tar.gz"},
	}

	if got := findAssetURL(assets, "tierd"); got != "https://example.com/tierd" {
		t.Errorf("findAssetURL(tierd) = %q", got)
	}
	if got := findAssetURL(assets, "missing"); got != "" {
		t.Errorf("findAssetURL(missing) = %q, want empty", got)
	}
}

func withSmoothfsInstalledRefPath(t *testing.T, path string) {
	t.Helper()
	original := smoothfsInstalledRefPath
	smoothfsInstalledRefPath = path
	t.Cleanup(func() { smoothfsInstalledRefPath = original })
}

func withSmoothfsSrcPath(t *testing.T, p string) {
	t.Helper()
	original := smoothfsSrcPath
	smoothfsSrcPath = p
	t.Cleanup(func() { smoothfsSrcPath = original })
}

func TestParseDKMSVersion(t *testing.T) {
	cases := []struct {
		conf    string
		want    string
		wantErr bool
	}{
		{`PACKAGE_NAME=smoothfs` + "\n" + `PACKAGE_VERSION=1.2.3` + "\n", "1.2.3", false},
		{`PACKAGE_VERSION="2.0.0"`, "2.0.0", false},
		{`PACKAGE_VERSION='3.1.4'`, "3.1.4", false},
		// Real smoothfs format: quoted value with a release-please inline
		// comment. Regression for the bug where the comment leaked into the
		// version and produced a broken DKMS entry.
		{`PACKAGE_VERSION="0.2.1" # x-release-please-version`, "0.2.1", false},
		{`PACKAGE_NAME="smoothfs"` + "\n" + `PACKAGE_VERSION="0.2.1" # x-release-please-version` + "\n", "0.2.1", false},
		{`PACKAGE_VERSION=0.2.1 # bare with comment`, "0.2.1", false},
		{`PACKAGE_VERSION = "1.0.0"`, "1.0.0", false},
		{`# comment` + "\n" + `PACKAGE_NAME=smoothfs`, "", true},
		{``, "", true},
	}
	for _, tc := range cases {
		got, err := parseDKMSVersion(tc.conf)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseDKMSVersion(%q): want error, got %q", tc.conf, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDKMSVersion(%q): unexpected error: %v", tc.conf, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDKMSVersion(%q) = %q, want %q", tc.conf, got, tc.want)
		}
	}
}

func TestReadSmoothfsVersionFromTar(t *testing.T) {
	// Build a minimal tar.gz with a dkms.conf at the root.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "smoothfs-src.tar.gz")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dkmsConf := []byte("PACKAGE_NAME=smoothfs\nPACKAGE_VERSION=1.2.3\n")
	_ = tw.WriteHeader(&tar.Header{
		Name: "dkms.conf",
		Mode: 0644,
		Size: int64(len(dkmsConf)),
	})
	tw.Write(dkmsConf)
	tw.Close()
	gz.Close()
	os.WriteFile(tarPath, buf.Bytes(), 0644)

	got, err := readSmoothfsVersionFromTar(tarPath)
	if err != nil {
		t.Fatalf("readSmoothfsVersionFromTar: %v", err)
	}
	if got != "1.2.3" {
		t.Errorf("got %q, want 1.2.3", got)
	}
}

func TestEnsureSmoothfsModuleNoopForEmptyRef(t *testing.T) {
	var cmds []string
	withExecCommand(t, func(name string, args ...string) *exec.Cmd {
		cmds = append(cmds, name)
		return exec.Command("bash", "-lc", "true")
	})
	if err := ensureSmoothfsModule("/dev/null", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmds) > 0 {
		t.Errorf("expected no commands for empty ref, got: %v", cmds)
	}
}

func TestEnsureSmoothfsModuleSkipsWhenRefMatches(t *testing.T) {
	dir := t.TempDir()
	refFile := filepath.Join(dir, "smoothfs-ref")
	withSmoothfsInstalledRefPath(t, refFile)

	const ref = "82aa4365c56b0f79a525c2f0cf458d9a3c3311a0"
	os.WriteFile(refFile, []byte(ref+"\n"), 0644)

	var cmds []string
	withExecCommand(t, func(name string, args ...string) *exec.Cmd {
		cmds = append(cmds, name)
		return exec.Command("bash", "-lc", "true")
	})

	if err := ensureSmoothfsModule("/dev/null", ref); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmds) > 0 {
		t.Errorf("expected no commands when ref already installed, got: %v", cmds)
	}
}

func TestManifestSmoothfsFields(t *testing.T) {
	raw := `{
		"version":"v1",
		"tierd_amd64_sha256":"AAA",
		"runtime_amd64_sha256":"RRR",
		"ui_sha256":"UUU",
		"smoothfs_ref":"82aa4365c56b0f79a525c2f0cf458d9a3c3311a0",
		"smoothfs_src_sha256":"CCC"
	}`
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.SmoothfsRef != "82aa4365c56b0f79a525c2f0cf458d9a3c3311a0" {
		t.Errorf("SmoothfsRef = %q", m.SmoothfsRef)
	}
	if m.SmoothfsSrcSHA != "CCC" {
		t.Errorf("SmoothfsSrcSHA = %q", m.SmoothfsSrcSHA)
	}
	if got := m.RuntimeSHAForArch("amd64"); got != "RRR" {
		t.Errorf("RuntimeSHAForArch(amd64) = %q", got)
	}

	// Old manifests without smoothfs fields decode cleanly with zero values.
	old := `{"version":"v0","tierd_sha256":"OLD","ui_sha256":"UUU"}`
	var m2 Manifest
	if err := json.Unmarshal([]byte(old), &m2); err != nil {
		t.Fatalf("decode old: %v", err)
	}
	if m2.SmoothfsRef != "" || m2.SmoothfsSrcSHA != "" {
		t.Errorf("old manifest should have empty smoothfs fields, got ref=%q sha=%q", m2.SmoothfsRef, m2.SmoothfsSrcSHA)
	}
	if got := m2.RuntimeSHAForArch("amd64"); got != "" {
		t.Errorf("old manifest should have empty runtime checksum, got %q", got)
	}
}
