# SmoothNAS packages hook — sourced by smoothiso/installer.sh install_packages.
# Runs inside the installer (set -e is inherited from smoothiso). The chroot at
# $TARGET is mounted with /dev /dev/pts /proc /sys; debconf postinsts are
# stubbed. Reconfiguration of stubbed packages happens at firstboot.

if [ ! -f /smoothnas/package-manifest ]; then
    die "Missing /smoothnas/package-manifest in installer payload"
fi
if [ ! -f /smoothnas/smoothfs-src/dkms.conf ]; then
    die "Missing /smoothnas/smoothfs-src in installer payload"
fi

# shellcheck disable=SC1091
. /smoothnas/package-manifest

if [ -z "${SMOOTHKERNEL_IMAGE_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_HEADERS_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_LIBC_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_ZFS_PACKAGES:-}" ] || \
   [ -z "${SMOOTHKERNEL_RELEASE_BASE_URL:-}" ] || \
   [ -z "${SMOOTHKERNEL_IMAGE_FILENAME:-}" ] || \
   [ -z "${SMOOTHKERNEL_HEADERS_FILENAME:-}" ] || \
   [ -z "${SMOOTHKERNEL_LIBC_FILENAME:-}" ] || \
   [ -z "${SMOOTHKERNEL_ZFS_FILENAMES:-}" ] || \
   [ -z "${SMOOTHFS_VERSION:-}" ]; then
    die "SmoothNAS package manifest is incomplete"
fi

# Download SmoothKernel and OpenZFS packages from GitHub releases and build a
# local apt repo on disk. Keeping the debs out of the initrd prevents it from
# exceeding GRUB's heap limit (~200 MB compressed).
ui_status "Installing packages" "Downloading SmoothKernel and OpenZFS packages." 3 6
echo "  Fetching packages from ${SMOOTHKERNEL_RELEASE_BASE_URL}..."

mkdir -p "$TARGET/opt/smoothnas/repo/pool"

_download_pkg() {
    local filename="$1"
    echo "  Downloading ${filename}..."
    # curl runs inside the chroot because the d-i installer environment does
    # not include it; smoothiso installs curl into $TARGET before sourcing
    # this hook, and busybox wget's axTLS fails GitHub's CDN TLS handshake.
    chroot "$TARGET" curl -fsSL -o "/opt/smoothnas/repo/pool/${filename}" \
        "${SMOOTHKERNEL_RELEASE_BASE_URL}/${filename}" || \
        die "Failed to download ${filename}"
}

_download_pkg "$SMOOTHKERNEL_IMAGE_FILENAME"
_download_pkg "$SMOOTHKERNEL_HEADERS_FILENAME"
_download_pkg "$SMOOTHKERNEL_LIBC_FILENAME"
for _zfs_pkg in $SMOOTHKERNEL_ZFS_FILENAMES; do
    _download_pkg "$_zfs_pkg"
done

# Ensure decompressors needed to extract control.tar.* are present in
# the chroot. xz-utils ships with debootstrap's required set; zstd is
# pulled in via apt below. (busybox in this chroot does not provide
# zstdcat as an applet, so we use the zstd binary directly.)
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y -qq \
    zstd xz-utils 2>/dev/null || true

echo "  Generating apt Packages index..."
# A .deb is an ar archive of debian-binary, control.tar.<comp>,
# data.tar.<comp>. Extract control.tar directly via ar+tar to sidestep
# the chroot's busybox dpkg-deb (which doesn't accept long flags, prints
# empty for short -f, and writes -e to <dir>/DEBIAN). busybox tar does
# NOT auto-detect compression, so pipe through the matching decompressor.
# Use `zstd -dc` / `xz -dc` / `gunzip -c` (zstdcat is not always present
# as a busybox applet).
chroot "$TARGET" sh -eu -c '
    cd /opt/smoothnas/repo
    : > Packages
    for _deb in pool/*.deb; do
        _tmp=$(mktemp -d)
        ( cd "$_tmp" && ar x "/opt/smoothnas/repo/$_deb" )
        _ctrl_archive=""
        for _f in "$_tmp"/control.tar*; do
            [ -e "$_f" ] && _ctrl_archive="$_f" && break
        done
        if [ -z "$_ctrl_archive" ]; then
            echo "ERROR: no control.tar in $_deb" >&2
            exit 1
        fi
        case "$_ctrl_archive" in
            *.zst) zstd -dc "$_ctrl_archive"  | tar -xf - -C "$_tmp" ;;
            *.xz)  xz -dc "$_ctrl_archive"    | tar -xf - -C "$_tmp" ;;
            *.gz)  gunzip -c "$_ctrl_archive" | tar -xf - -C "$_tmp" ;;
            *)     tar -xf "$_ctrl_archive" -C "$_tmp" ;;
        esac
        # Some debs use ./control, some control; accept either.
        if [ -f "$_tmp/control" ]; then
            _control="$_tmp/control"
        elif [ -f "$_tmp/./control" ]; then
            _control="$_tmp/./control"
        else
            echo "ERROR: control file not found after extracting $_deb" >&2
            ls -la "$_tmp" >&2
            exit 1
        fi
        {
            cat "$_control"
            printf "Filename: %s\nSize: %s\nSHA256: %s\n\n" \
                "$_deb" \
                "$(wc -c < "$_deb")" \
                "$(sha256sum "$_deb" | cut -d" " -f1)"
        } >> Packages
        rm -rf "$_tmp"
    done
    gzip -9c Packages > Packages.gz
' || die "Failed to generate apt Packages index"

# Stage smoothfs DKMS source, protocol tests, and manifest onto disk.
rm -rf "$TARGET/opt/smoothnas/smoothfs-src"
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

ui_status "Installing packages" "Installing SmoothNAS plugin runtime dependencies." 3 6
echo "  Installing plugin runtime dependencies..."
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y -qq \
    lxc lxc-templates skopeo umoci rsync nftables iptables iproute2 uidmap \
    2>/dev/null || true

# OpenZFS, smoothfs, and Samba VFS DKMS builds are deferred to firstboot
# (see /smoothiso-hooks/firstboot.sh) where the booted kernel matches the
# installed SmoothKernel headers.
echo "  Deferring OpenZFS / smoothfs DKMS builds to first boot."
