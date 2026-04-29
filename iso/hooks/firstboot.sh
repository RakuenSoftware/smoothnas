# SmoothNAS firstboot extension — sourced by smoothiso/firstboot.sh.
# Runs once on the first boot, after smoothiso has reconfigured deferred
# packages and brought up SSH. Inherits set -euo pipefail from the parent.
#
# Builds the DKMS-backed storage stack (OpenZFS, smoothfs, smoothfs Samba VFS),
# generates the TLS certificate used by nginx, and finalises tierd PAM + admin
# bootstrap. The smoothiso parent writes the firstboot-done marker on success.

if [ ! -f /opt/smoothnas/package-manifest ]; then
    echo "ERROR: missing /opt/smoothnas/package-manifest" >&2
    exit 1
fi

# shellcheck disable=SC1091
. /opt/smoothnas/package-manifest

if [ -z "${SMOOTHKERNEL_HEADERS_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_LIBC_PACKAGE:-}" ] || \
   [ -z "${SMOOTHKERNEL_ZFS_PACKAGES:-}" ] || \
   [ -z "${SMOOTHFS_VERSION:-}" ]; then
    echo "ERROR: SmoothNAS package manifest is incomplete" >&2
    exit 1
fi

ensure_kernel_headers_ready() {
    local kver="$1"

    if [ -z "$kver" ]; then
        echo "ERROR: cannot validate kernel headers without a kernel version" >&2
        return 1
    fi
    if [ ! -d "/usr/src/linux-headers-$kver" ]; then
        echo "ERROR: kernel headers missing: /usr/src/linux-headers-$kver" >&2
        return 1
    fi
    if [ ! -e "/lib/modules/$kver/build/include/generated/autoconf.h" ] || \
       [ ! -e "/lib/modules/$kver/build/Module.symvers" ]; then
        echo "ERROR: kernel headers for $kver are not DKMS-ready" >&2
        return 1
    fi
}

install_smoothkernel_headers() {
    local target_kernel
    target_kernel=$(uname -r)
    DEBIAN_FRONTEND=noninteractive apt-get install -y \
        "$SMOOTHKERNEL_LIBC_PACKAGE" \
        "$SMOOTHKERNEL_HEADERS_PACKAGE"
    ensure_kernel_headers_ready "$target_kernel"
}

install_zfs_stack() {
    DEBIAN_FRONTEND=noninteractive apt-get install -y $SMOOTHKERNEL_ZFS_PACKAGES
    grep -qxF 'zfs' /etc/modules 2>/dev/null || echo 'zfs' >> /etc/modules
    modprobe zfs 2>/dev/null || true
}

install_smoothfs_dkms() {
    local target_kernel
    target_kernel=$(uname -r)
    mkdir -p /usr/src
    rm -rf "/usr/src/smoothfs-${SMOOTHFS_VERSION}"
    cp -a /opt/smoothnas/smoothfs-src "/usr/src/smoothfs-${SMOOTHFS_VERSION}"

    mkdir -p /usr/share/smoothfs-dkms
    cp /opt/smoothnas/smoothfs-src/scripts/enroll-signing-cert.sh /usr/share/smoothfs-dkms/
    cp /opt/smoothnas/smoothfs-src/test/module_signing.sh /usr/share/smoothfs-dkms/
    cp /opt/smoothnas/smoothfs-src/test/kernel_upgrade.sh /usr/share/smoothfs-dkms/
    chmod +x /usr/share/smoothfs-dkms/*.sh

    ensure_kernel_headers_ready "$target_kernel"
    dkms remove -m smoothfs -v "$SMOOTHFS_VERSION" --all 2>/dev/null || true
    dkms add -m smoothfs -v "$SMOOTHFS_VERSION"
    dkms build -m smoothfs -v "$SMOOTHFS_VERSION" -k "$target_kernel"
    dkms install -m smoothfs -v "$SMOOTHFS_VERSION" -k "$target_kernel"
    update-initramfs -u -k "$target_kernel"
    grep -qxF 'smoothfs' /etc/modules 2>/dev/null || echo 'smoothfs' >> /etc/modules
    modprobe smoothfs 2>/dev/null || true
}

enable_debian_source_repos() {
    # shellcheck disable=SC1091
    . /etc/os-release

    if apt-get source --print-uris samba >/dev/null 2>&1; then
        return 0
    fi

    if [ -f /etc/apt/sources.list.d/debian.sources ]; then
        awk '
            /^Types:/ {
                if ($0 ~ /deb-src/) {
                    print
                } else {
                    sub(/^Types:[[:space:]]*deb/, "Types: deb-src")
                    print
                }
                next
            }
            { print }
        ' /etc/apt/sources.list.d/debian.sources > /etc/apt/sources.list.d/smoothnas-deb-src.sources
    elif [ -n "${VERSION_CODENAME:-}" ]; then
        cat > /etc/apt/sources.list.d/smoothnas-deb-src.list <<SRC
deb-src http://deb.debian.org/debian ${VERSION_CODENAME} main contrib non-free-firmware
deb-src http://deb.debian.org/debian-security ${VERSION_CODENAME}-security main contrib non-free-firmware
deb-src http://deb.debian.org/debian ${VERSION_CODENAME}-updates main contrib non-free-firmware
SRC
    else
        echo "ERROR: cannot enable Debian source repositories for smoothfs-samba-vfs" >&2
        return 1
    fi

    apt-get update -qq
}

prepare_samba_source() {
    local samba_full samba_upstream

    samba_full=$(dpkg-query -W -f='${Version}' samba 2>/dev/null)
    if [ -z "$samba_full" ]; then
        echo "ERROR: samba is not installed" >&2
        return 1
    fi
    samba_upstream=$(echo "${samba_full#2:}" | sed 's/-.*//')
    if [ -d "/tmp/samba-${samba_upstream}" ]; then
        return 0
    fi

    enable_debian_source_repos
    (
        cd /tmp
        apt-get source "samba=${samba_full}"
    )
}

install_smoothfs_samba_vfs() {
    local src deb

    src=/opt/smoothnas/smoothfs-src/samba-vfs
    if [ ! -f "$src/build.sh" ]; then
        echo "ERROR: missing smoothfs Samba VFS source at $src" >&2
        return 1
    fi

    prepare_samba_source
    DEBIAN_FRONTEND=noninteractive apt-get build-dep -y samba
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq debhelper dpkg-dev

    (
        cd "$src"
        dpkg-buildpackage -us -uc -b
    )
    deb=$(ls -t /opt/smoothnas/smoothfs-src/smoothfs-samba-vfs_*_*.deb 2>/dev/null | head -n 1)
    if [ -z "$deb" ]; then
        echo "ERROR: smoothfs-samba-vfs package was not produced" >&2
        return 1
    fi
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$deb"
    if ! compgen -G '/usr/lib/*/samba/vfs/smoothfs.so' >/dev/null; then
        echo "ERROR: smoothfs Samba VFS module did not install" >&2
        return 1
    fi
}

install_firstboot_service_packages() {
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
        samba cifs-utils smbclient samba-testsuite
    systemctl disable --now smbd nmbd winbind samba-ad-dc 2>/dev/null || true
}

# Persist the language the operator picked during install. The kiosk wrote
# it to /etc/smoothnas/installer-lang inside the target filesystem; surface
# it at /etc/smoothnas/locale so the web UI's unauthenticated /api/locale
# endpoint can read it before login.
INSTALL_LANG=""
if [ -r /etc/smoothnas/installer-lang ]; then
    INSTALL_LANG=$(head -n 1 /etc/smoothnas/installer-lang | tr -d '[:space:]')
fi
case "$INSTALL_LANG" in
    en|nl|en-*|nl-*) ;;
    *) INSTALL_LANG="en" ;;
esac
mkdir -p /etc/smoothnas
printf '%s\n' "$INSTALL_LANG" > /etc/smoothnas/locale
chmod 644 /etc/smoothnas/locale
echo "Installer language persisted: $INSTALL_LANG"

apt-get update -qq
install_smoothkernel_headers
install_zfs_stack
install_smoothfs_dkms
install_firstboot_service_packages
install_smoothfs_samba_vfs

# Generate the self-signed TLS certificate referenced by nginx.
TLS_DIR="/etc/tierd/tls"
mkdir -p "$TLS_DIR"
if [ ! -f "$TLS_DIR/cert.pem" ]; then
    HOST=$(hostname)
    openssl req -x509 -nodes \
        -days 3650 \
        -newkey rsa:2048 \
        -keyout "$TLS_DIR/key.pem" \
        -out "$TLS_DIR/cert.pem" \
        -subj "/CN=${HOST}/O=SmoothNAS" \
        -addext "subjectAltName=DNS:${HOST},DNS:localhost,IP:127.0.0.1" \
        2>/dev/null
    chmod 600 "$TLS_DIR/key.pem"
    chmod 644 "$TLS_DIR/cert.pem"
fi

# Dedicated PAM service for tierd web UI logins.
if [ ! -f /etc/pam.d/tierd ]; then
    cat > /etc/pam.d/tierd << 'PAM'
# PAM service for tierd web UI authentication.
auth	[success=1 default=ignore]	pam_unix.so nullok
auth	requisite			pam_deny.so
auth	required			pam_permit.so
account	[success=1 new_authtok_reqd=done default=ignore]	pam_unix.so
account	requisite			pam_deny.so
account	required			pam_permit.so
PAM
fi

# Replace the temporary install-time root password (set equal to admin) with
# a random one. The web UI is the normal entry point; this is for rescue.
ROOT_PASS=$(openssl rand -base64 12 | tr -dc 'a-zA-Z0-9' | head -c 16)
echo "root:${ROOT_PASS}" | chpasswd

mkdir -p /var/lib/tierd
cat > /var/lib/tierd/initial-credentials << CREDS
SmoothNAS Initial Credentials
Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)

Root password: ${ROOT_PASS}

This is for emergency/rescue access only.
Log in to the web UI with the admin password you set during install.
Delete this file after recording the root password.
CREDS
chmod 600 /var/lib/tierd/initial-credentials

systemctl enable nginx.service 2>/dev/null || true
systemctl --no-block restart nginx 2>/dev/null || true
