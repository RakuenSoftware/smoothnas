# SmoothNAS packages hook — sourced by smoothiso/installer.sh install_packages.
# Runs inside the installer (set -e is inherited from smoothiso). The chroot at
# $TARGET is mounted with /dev /dev/pts /proc /sys; debconf postinsts are
# stubbed. Reconfiguration of stubbed packages happens at firstboot.
#
# Everything SmoothNAS runs comes from one package, smoothnas, whose Depends
# pull in SmoothKernel, OpenZFS and smoothfs from the Smooth* archive and the
# Debian storage and sharing tools. Once installed, the NAS keeps all of it
# current with apt (unattended-upgrades). The payload carries the smoothnas
# package built with this ISO; everything else is downloaded.

[ -f /smoothnas/smoothnas.deb ] || die "Missing /smoothnas/smoothnas.deb in installer payload"
[ -f /smoothnas/smooth-archive-keyring.gpg ] || die "Missing the Smooth* archive key in installer payload"

ui_status "Installing packages" "Adding the SmoothNAS package archive." 3 6
mkdir -p "$TARGET/usr/share/keyrings"
cp /smoothnas/smooth-archive-keyring.gpg "$TARGET/usr/share/keyrings/smooth-archive-keyring.gpg"
chmod 644 "$TARGET/usr/share/keyrings/smooth-archive-keyring.gpg"
# The smoothnas package installs the NAS's own copy of this source; this one
# is only for the install and is removed below.
cat > "$TARGET/etc/apt/sources.list.d/smoothapt-install.list" << 'SOURCES'
deb [signed-by=/usr/share/keyrings/smooth-archive-keyring.gpg] https://updates.rakuensoftware.com/public common smoothnas
SOURCES
# Never install a Debian kernel beside SmoothKernel: DKMS packages recommend
# the distribution's headers metapackage, which would bring one in.
cat > "$TARGET/etc/apt/preferences.d/smoothnas-no-debian-kernel" << 'PREF'
Package: linux-image-* linux-headers-*
Pin: release o=Debian
Pin-Priority: -1
PREF
chroot "$TARGET" apt-get update -qq

mkdir -p "$TARGET/usr/share/smoothnas"
rm -rf "$TARGET/usr/share/smoothnas/tests"
if [ -d /smoothnas/tests ]; then
    cp -a /smoothnas/tests "$TARGET/usr/share/smoothnas/tests"
    chmod +x "$TARGET/usr/share/smoothnas/tests/"*.sh 2>/dev/null || true
fi

ui_status "Installing packages" "Adding the Ookla speedtest-cli repository." 3 6
echo "  Installing speedtest-cli (Ookla repo)..."
chroot "$TARGET" bash -lc \
    'curl -fsSL https://packagecloud.io/install/repositories/ookla/speedtest-cli/script.deb.sh | bash' \
    2>/dev/null || true
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y -qq \
    speedtest 2>/dev/null || true

# SmoothNAS, SmoothKernel, OpenZFS, smoothfs and the storage and sharing tools.
# DKMS builds ZFS and smoothfs for the installed SmoothKernel here where it
# can; firstboot completes it against the running kernel.
ui_status "Installing packages" "Installing SmoothNAS, SmoothKernel, OpenZFS and smoothfs." 3 6
cp /smoothnas/smoothnas.deb "$TARGET/var/cache/apt/archives/smoothnas.deb"
DEBIAN_FRONTEND=noninteractive chroot "$TARGET" apt-get install -y \
    /var/cache/apt/archives/smoothnas.deb 2>&1 || die "Failed to install SmoothNAS"
rm -f "$TARGET/etc/apt/sources.list.d/smoothapt-install.list"

# tcp_bbr is built as a module in Debian's trixie kernel; SmoothKernel ships
# BBR + FQ built-in but loading it on either kernel is a no-op when present.
grep -qxF 'tcp_bbr' "$TARGET/etc/modules" 2>/dev/null || \
    echo 'tcp_bbr' >> "$TARGET/etc/modules"
