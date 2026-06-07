package updater

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// SmoothKernel is published as a separate repo; its releases carry the
// kernel/header/libc .debs plus the matching OpenZFS stack. The appliance
// pins one release tag (iso/smoothkernel-version), which the SmoothNAS
// release workflow copies into manifest.json as smoothkernel_tag so an
// already-installed box can move to a new kernel on update instead of only
// fresh ISO installs.
const (
	smoothkernelOwner = "RakuenSoftware"
	smoothkernelRepo  = "smoothkernel"
)

// zfsPackagePrefixes are the OpenZFS .deb name prefixes the appliance needs,
// mirroring the set build-iso.sh bakes into the ISO. Each is filename-tagged
// with the arch in the SmoothKernel release. Order matters only for
// readability; apt resolves the install set.
var zfsPackagePrefixes = []string{
	"libnvpair3_",
	"libuutil3_",
	"libzfs7_",
	"libzpool7_",
	"zfs_",
	"zfs-dkms_",
	"zfs-initramfs_",
}

// ensureSmoothKernel installs the kernel pinned by tag (and its matching
// OpenZFS stack) from the SmoothKernel GitHub release, when not already
// installed. It returns (true, nil) when it actually installed a new kernel
// — the caller reboots to activate it. It is a no-op (false, nil) when the
// pinned kernel is already installed, which keeps repeated applies from
// looping reboots.
//
// arch is the Go/Debian architecture ("amd64"/"arm64"), which matches the
// arch suffix the SmoothKernel release uses on every .deb.
func ensureSmoothKernel(baseURL, tag, arch string) (bool, error) {
	if tag == "" {
		return false, nil
	}

	rel, err := fetchReleaseByTag(baseURL, smoothkernelOwner, smoothkernelRepo, tag)
	if err != nil {
		return false, err
	}

	imgAsset := selectAsset(rel.Assets, "linux-image-", "_"+arch+".deb", "-dbg")
	if imgAsset == nil {
		return false, fmt.Errorf("smoothkernel %s: no linux-image asset for %s", tag, arch)
	}
	kver := kverFromImageName(imgAsset.Name) // e.g. "7.0.11-smoothkernel"
	if kver == "" {
		return false, fmt.Errorf("smoothkernel %s: could not parse kernel version from %q", tag, imgAsset.Name)
	}
	// Already installed → nothing to do (and nothing to reboot for).
	if isPackageInstalled("linux-image-" + kver) {
		return false, nil
	}

	hdrAsset := selectAsset(rel.Assets, "linux-headers-", "_"+arch+".deb", "")
	libcAsset := selectAsset(rel.Assets, "linux-libc-dev_", "_"+arch+".deb", "")
	if hdrAsset == nil || libcAsset == nil {
		return false, fmt.Errorf("smoothkernel %s: missing headers or libc asset for %s", tag, arch)
	}

	var zfsAssets []*ghAsset
	for _, prefix := range zfsPackagePrefixes {
		a := selectAsset(rel.Assets, prefix, "_"+arch+".deb", "")
		if a == nil {
			return false, fmt.Errorf("smoothkernel %s: missing OpenZFS asset %s* for %s", tag, prefix, arch)
		}
		zfsAssets = append(zfsAssets, a)
	}

	sums, err := fetchSHA256SUMS(rel.Assets)
	if err != nil {
		return false, fmt.Errorf("smoothkernel %s: %w", tag, err)
	}

	kernelDir := filepath.Join(stagingDir, "smoothkernel")
	if err := os.MkdirAll(kernelDir, 0755); err != nil {
		return false, fmt.Errorf("create kernel staging dir: %w", err)
	}
	defer os.RemoveAll(kernelDir)

	download := func(a *ghAsset) (string, error) {
		dest := filepath.Join(kernelDir, a.Name)
		if err := downloadAsset(a, dest, false); err != nil {
			return "", fmt.Errorf("download %s: %w", a.Name, err)
		}
		expected, ok := sums[a.Name]
		if !ok {
			return "", fmt.Errorf("no SHA256SUMS entry for %s", a.Name)
		}
		if err := verifyChecksum(dest, expected); err != nil {
			return "", fmt.Errorf("checksum %s: %w", a.Name, err)
		}
		return dest, nil
	}

	imgPath, err := download(imgAsset)
	if err != nil {
		return false, err
	}
	hdrPath, err := download(hdrAsset)
	if err != nil {
		return false, err
	}
	libcPath, err := download(libcAsset)
	if err != nil {
		return false, err
	}
	var zfsPaths []string
	for _, a := range zfsAssets {
		p, err := download(a)
		if err != nil {
			return false, err
		}
		zfsPaths = append(zfsPaths, p)
	}

	// Install the OpenZFS stack first. On an existing box the running kernel's
	// zfs-dkms may be an older source that does not build against the new
	// kernel; upgrading it (it rebuilds against the *current* kernel) means
	// the subsequent new-headers DKMS autoinstall builds the new ZFS, not the
	// old one — otherwise that autoinstall would fail and abort the apt run.
	if err := aptInstallLocalDebs(zfsPaths); err != nil {
		return false, fmt.Errorf("install OpenZFS stack: %w", err)
	}
	// Then the kernel + headers + libc. Installing the headers triggers DKMS
	// autoinstall, which builds zfs and smoothfs for the new kernel; the
	// linux-image postinst runs update-grub / update-initramfs.
	if err := aptInstallLocalDebs([]string{libcPath, imgPath, hdrPath}); err != nil {
		return false, fmt.Errorf("install kernel: %w", err)
	}

	log.Printf("updater: installed SmoothKernel %s (%s); reboot required to activate", tag, kver)
	return true, nil
}

// fetchReleaseByTag returns the release with the exact tag from owner/repo.
func fetchReleaseByTag(baseURL, owner, repo, tag string) (*ghRelease, error) {
	releases, err := fetchReleases(baseURL, owner, repo, false)
	if err != nil {
		return nil, err
	}
	for i := range releases {
		if releases[i].TagName == tag {
			return &releases[i], nil
		}
	}
	return nil, fmt.Errorf("release %s not found in %s/%s", tag, owner, repo)
}

// selectAsset returns the first asset whose name has the given prefix and
// suffix and does not contain exclude (when exclude is non-empty).
func selectAsset(assets []ghAsset, prefix, suffix, exclude string) *ghAsset {
	for i := range assets {
		n := assets[i].Name
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		if exclude != "" && strings.Contains(n, exclude) {
			continue
		}
		return &assets[i]
	}
	return nil
}

// kverFromImageName extracts the uname-r kernel version from a linux-image
// .deb filename, e.g. "linux-image-7.0.11-smoothkernel_7.0.11-1_amd64.deb"
// → "7.0.11-smoothkernel".
func kverFromImageName(name string) string {
	s := strings.TrimPrefix(name, "linux-image-")
	if i := strings.Index(s, "_"); i >= 0 {
		s = s[:i]
	}
	if s == name {
		return ""
	}
	return s
}

// fetchSHA256SUMS downloads the release's SHA256SUMS asset and parses it into
// a filename→hash map.
func fetchSHA256SUMS(assets []ghAsset) (map[string]string, error) {
	a := selectAsset(assets, "SHA256SUMS", "", "")
	if a == nil {
		return nil, fmt.Errorf("release has no SHA256SUMS asset")
	}
	dest := filepath.Join(stagingDir, "smoothkernel-SHA256SUMS")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return nil, err
	}
	if err := downloadAsset(a, dest, false); err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	defer os.Remove(dest)

	f, err := os.Open(dest)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sums := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum format prefixes binary-mode files with '*'.
		name := strings.TrimPrefix(fields[1], "*")
		sums[name] = fields[0]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

// aptInstallLocalDebs installs a set of local .deb files via apt so that
// dependencies between them resolve. Paths must be absolute (they contain a
// slash, which apt requires to treat an argument as a local file).
func aptInstallLocalDebs(debs []string) error {
	if len(debs) == 0 {
		return nil
	}
	args := append([]string{"install", "-y", "-qq", "--allow-downgrades"}, debs...)
	cmd := execCommand("apt-get", args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apt-get install: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
