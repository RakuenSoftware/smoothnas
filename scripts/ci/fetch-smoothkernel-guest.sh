#!/usr/bin/env bash
# Install the SmoothKernel a NAS gets for the protocol guest: the newest
# linux-image-smoothkernel and linux-headers-smoothkernel from the signed
# Smooth* archive, the way the smoothnas package brings them. apt checks the
# archive's signature and every package's hash.
#
# SMOOTHKERNEL_GUEST_DOWNLOAD_ONLY=1 downloads without installing (for
# inspection and local debugging; CI always installs).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
keyring=/usr/share/keyrings/smooth-archive-keyring.gpg
install -D -m 644 "${PROJECT_DIR}/packaging/smoothnas/usr/share/keyrings/smooth-archive-keyring.gpg" "$keyring"
echo "deb [signed-by=${keyring}] https://updates.rakuensoftware.com/public common main" \
    > /etc/apt/sources.list.d/smoothapt.list
apt-get update -qq >&2
packages=(linux-image-smoothkernel linux-headers-smoothkernel)
if [ "${SMOOTHKERNEL_GUEST_DOWNLOAD_ONLY:-0}" = 1 ]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y --download-only "${packages[@]}" >&2
    exit 0
fi
DEBIAN_FRONTEND=noninteractive apt-get install -y "${packages[@]}" >&2
kver="$(dpkg-query -W -f='${Depends}' linux-image-smoothkernel | sed -n 's/^linux-image-\([^ ,]*\).*/\1/p')"
[ -n "$kver" ] || { echo 'linux-image-smoothkernel names no kernel' >&2; exit 1; }
test -e "/boot/vmlinuz-${kver}"
test -d "/lib/modules/${kver}/build"
depmod "$kver" >&2
echo "GUEST_KVER=${kver}"
