package updater

import "testing"

func sampleKernelAssets() []ghAsset {
	names := []string{
		"linux-headers-7.0.11-smoothkernel_7.0.11-1_amd64.deb",
		"linux-headers-7.0.11-smoothkernel_7.0.11-1_arm64.deb",
		"linux-image-7.0.11-smoothkernel_7.0.11-1_amd64.deb",
		"linux-image-7.0.11-smoothkernel_7.0.11-1_arm64.deb",
		"linux-image-7.0.11-smoothkernel-dbg_7.0.11-1_amd64.deb",
		"linux-libc-dev_7.0.11-1_amd64.deb",
		"linux-libc-dev_7.0.11-1_arm64.deb",
		"libnvpair3_2.4.2-1_amd64.deb",
		"libuutil3_2.4.2-1_amd64.deb",
		"libzfs7_2.4.2-1_amd64.deb",
		"libzfs7-devel_2.4.2-1_amd64.deb",
		"libzpool7_2.4.2-1_amd64.deb",
		"zfs_2.4.2-1_amd64.deb",
		"zfs-dkms_2.4.2-1_amd64.deb",
		"zfs-initramfs_2.4.2-1_amd64.deb",
		"zfs-test_2.4.2-1_amd64.deb",
		"SHA256SUMS",
		"manifest.json",
	}
	assets := make([]ghAsset, len(names))
	for i, n := range names {
		assets[i] = ghAsset{Name: n, BrowserDownloadURL: "https://example/" + n}
	}
	return assets
}

func TestSelectAssetImageExcludesDbgAndArch(t *testing.T) {
	assets := sampleKernelAssets()

	img := selectAsset(assets, "linux-image-", "_amd64.deb", "-dbg")
	if img == nil || img.Name != "linux-image-7.0.11-smoothkernel_7.0.11-1_amd64.deb" {
		t.Fatalf("image select = %v, want non-dbg amd64 image", img)
	}

	arm := selectAsset(assets, "linux-image-", "_arm64.deb", "-dbg")
	if arm == nil || arm.Name != "linux-image-7.0.11-smoothkernel_7.0.11-1_arm64.deb" {
		t.Fatalf("arm image select = %v", arm)
	}
}

func TestSelectAssetZFSPrefixesAreExact(t *testing.T) {
	assets := sampleKernelAssets()

	// zfs_ must match zfs_*, not zfs-dkms_/zfs-initramfs_/zfs-test_.
	z := selectAsset(assets, "zfs_", "_amd64.deb", "")
	if z == nil || z.Name != "zfs_2.4.2-1_amd64.deb" {
		t.Fatalf("zfs_ select = %v, want zfs_2.4.2-1_amd64.deb", z)
	}
	// libzfs7_ must not pick up libzfs7-devel_.
	lz := selectAsset(assets, "libzfs7_", "_amd64.deb", "")
	if lz == nil || lz.Name != "libzfs7_2.4.2-1_amd64.deb" {
		t.Fatalf("libzfs7_ select = %v, want libzfs7_2.4.2-1_amd64.deb", lz)
	}

	for _, prefix := range zfsPackagePrefixes {
		if selectAsset(assets, prefix, "_amd64.deb", "") == nil {
			t.Errorf("missing required ZFS asset for prefix %q", prefix)
		}
	}
}

func TestKverFromImageName(t *testing.T) {
	cases := map[string]string{
		"linux-image-7.0.11-smoothkernel_7.0.11-1_amd64.deb": "7.0.11-smoothkernel",
		"linux-image-6.19.12-smoothkernel_6.19.12-1_arm64.deb": "6.19.12-smoothkernel",
		"not-an-image.deb": "",
	}
	for name, want := range cases {
		if got := kverFromImageName(name); got != want {
			t.Errorf("kverFromImageName(%q) = %q, want %q", name, got, want)
		}
	}
}
