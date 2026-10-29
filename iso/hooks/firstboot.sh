# SmoothNAS firstboot extension — sourced by smoothiso/firstboot.sh.
# Runs once on the first boot, after smoothiso has reconfigured deferred
# packages and brought up SSH. Inherits set -euo pipefail from the parent.
#
# Completes the storage stack's kernel modules (OpenZFS and smoothfs come from
# the smoothnas package's dependencies as DKMS sources) for the running kernel
# and finalises tierd PAM + admin bootstrap. The smoothiso parent writes the
# firstboot-done marker on success.

# Promote the installer language selection to the runtime locale file.
# configure.sh writes /etc/smoothnas/installer-lang during installation;
# surface it at /etc/smoothnas/locale so the web UI's unauthenticated
# /api/locale endpoint can read it before login.
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

# Finish configuring what the installer's chroot could not (Samba and what
# depends on it, smoothnas included). smoothiso's firstboot tried already,
# quietly; this one must succeed, and says why if it does not.
DEBIAN_FRONTEND=noninteractive dpkg --configure --pending
[ "$(dpkg-query -W -f='${db:Status-Status}' smoothnas)" = installed ] || {
    echo "ERROR: smoothnas is not configured" >&2
    dpkg --audit >&2 || true
    exit 1
}

# Run the real postinsts the installer stubbed (packages.sh recorded them):
# openzfs-zfs-dkms's registers and builds ZFS with DKMS.
if [ -s /var/lib/smoothnas/reconfigure-at-firstboot ]; then
    while read -r pkg; do
        [ -n "$pkg" ] || continue
        echo "Reconfiguring $pkg (postinst deferred from install)"
        DEBIAN_FRONTEND=noninteractive dpkg-reconfigure -fnoninteractive "$pkg"
    done < /var/lib/smoothnas/reconfigure-at-firstboot
    rm -f /var/lib/smoothnas/reconfigure-at-firstboot
fi

# DKMS builds OpenZFS and smoothfs for every kernel with headers when their
# packages install; build whatever the running kernel (the installed
# SmoothKernel) still lacks, load the modules, and load them at boot.
kver=$(uname -r)
dkms autoinstall -k "$kver"
for module in zfs smoothfs; do
    modprobe "$module"
    grep -qxF "$module" /etc/modules 2>/dev/null || echo "$module" >> /etc/modules
done
flock /run/lock/smoothnas-initramfs.lock update-initramfs -u -k "$kver"
systemctl enable zfs.target zfs-import.target zfs-import-cache.service zfs-mount.service 2>/dev/null || true
# Sharing services start on demand (tierd manages them).
systemctl disable --now smbd nmbd winbind samba-ad-dc 2>/dev/null || true

# The TLS certificate nginx needs is generated before nginx starts by
# smoothnas-tls-init.service (smoothnas package), so the web UI is
# reachable while this script builds the storage stack.

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

# nginx is enabled at install time (configure.sh) and has been serving the
# web UI since boot; nothing to (re)start here.
