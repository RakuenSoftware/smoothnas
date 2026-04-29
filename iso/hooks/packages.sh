# SmoothNAS packages hook — sourced by smoothiso/installer.sh install_packages.
# Runs inside the installer (set -e is inherited from smoothiso). The chroot at
# $TARGET is mounted with /dev /dev/pts /proc /sys; debconf postinsts are
# stubbed. Reconfiguration of stubbed packages happens at firstboot.

if [ ! -f /smoothnas/package-manifest ]; then
    die "Missing /smoothnas/package-manifest in installer payload"
fi
if [ ! -d /smoothnas/repo ]; then
    die "Missing /smoothnas/repo in installer payload"
fi
if [ ! -f /smoothnas/smoothfs-src/dkms.conf ]; then
    die "Missing /smoothnas/smoothfs-src in installer payload"
fi

# Stage the local apt repo, smoothfs DKMS source, and protocol tests onto disk.
mkdir -p "$TARGET/opt/smoothnas"
rm -rf "$TARGET/opt/smoothnas/repo" "$TARGET/opt/smoothnas/smoothfs-src"
cp -a /smoothnas/repo "$TARGET/opt/smoothnas/repo"
cp -a /smoothnas/smoothfs-src "$TARGET/opt/smoothnas/smoothfs-src"
cp /smoothnas/package-manifest "$TARGET/opt/smoothnas/package-manifest"

mkdir -p "$TARGET/usr/share/smoothnas"
rm -rf "$TARGET/usr/share/smoothnas/tests"
if [ -d /smoothnas/tests ]; then
    cp -a /smoothnas/tests "$TARGET/usr/share/smoothnas/tests"
    chmod +x "$TARGET/usr/share/smoothnas/tests/"*.sh 2>/dev/null || true
fi

cat > "$TARGET/etc/apt/sources.list.d/smoothnas-local.list" << 'SOURCES'
deb [trusted=yes] file:/opt/smoothnas/repo ./
SOURCES

# shellcheck disable=SC1091
. /smoothnas/package-manifest

if [ -z "${SMOOTHKERNEL_IMAGE_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_HEADERS_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_LIBC_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_ZFS_PACKAGES:-}" ] || \
   [ -z "${SMOOTHFS_VERSION:-}" ]; then
    die "SmoothNAS package manifest is incomplete"
fi

ui_status "Installing packages" "Refreshing apt indexes (SmoothNAS local repo)." 3 6
chroot "$TARGET" apt-get update -qq

ui_status "Installing packages" "Installing DKMS toolchain and storage utilities." 3 6
echo "  Installing DKMS toolchain and storage utilities..."
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y -qq \
    dkms build-essential initramfs-tools libelf-dev kmod dpkg-dev \
    xfsprogs mokutil openssl \
    thin-provisioning-tools smartmontools hdparm nvme-cli gdisk fio psmisc rsync \
    iperf3 \
    2>/dev/null || true

ui_status "Installing packages" "Adding the Ookla speedtest-cli repository." 3 6
echo "  Installing speedtest-cli (Ookla repo)..."
chroot "$TARGET" bash -lc \
    'curl -fsSL https://packagecloud.io/install/repositories/ookla/speedtest-cli/script.deb.sh | bash' \
    2>/dev/null || true
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y -qq \
    speedtest 2>/dev/null || true

ui_status "Installing packages" "Installing SmoothKernel image and headers." 3 6
echo "  Installing SmoothKernel headers and image..."
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y \
    "$SMOOTHKERNEL_LIBC_PACKAGE" \
    "$SMOOTHKERNEL_HEADERS_PACKAGE" \
    "$SMOOTHKERNEL_IMAGE_PACKAGE" \
    2>&1 || die "Failed to install SmoothKernel"

# tcp_bbr is built as a module in Debian's trixie kernel; SmoothKernel ships
# BBR + FQ built-in but loading it on either kernel is a no-op when present.
grep -qxF 'tcp_bbr' "$TARGET/etc/modules" 2>/dev/null || \
    echo 'tcp_bbr' >> "$TARGET/etc/modules"

ui_status "Installing packages" "Installing nginx, nftables, NFS, and iSCSI service packages." 3 6
echo "  Installing service packages..."
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y -qq \
    nginx nftables \
    nfs-kernel-server \
    targetcli-fb python3-rtslib-fb \
    2>/dev/null || true

# OpenZFS, smoothfs, and Samba VFS DKMS builds are deferred to firstboot
# (see /smoothiso-hooks/firstboot.sh) where the booted kernel matches the
# installed SmoothKernel headers.
echo "  Deferring OpenZFS / smoothfs DKMS builds to first boot."
