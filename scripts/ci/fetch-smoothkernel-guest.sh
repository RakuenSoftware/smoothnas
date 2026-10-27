#!/usr/bin/env bash
# Fetch the ISO's pinned kernel for the protocol guest, checking package hashes
# before installing anything. An explicit download directory supports inspection
# without changing the build host.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
tag="$(tr -d '[:space:]' < "${PROJECT_DIR}/iso/smoothkernel-version")"
base="${SMOOTHKERNEL_GUEST_BASE_URL:-https://github.com/RakuenSoftware/smoothkernel/releases/download/${tag}}"
workdir="${SMOOTHKERNEL_GUEST_DOWNLOAD_DIR:-$(mktemp -d)}"
if [ -z "${SMOOTHKERNEL_GUEST_DOWNLOAD_DIR:-}" ]; then
    trap 'rm -rf "$workdir"' EXIT
fi
mkdir -p "$workdir"
cd "$workdir"
fetch() {
    curl -fsSL --retry 5 --retry-delay 2 --retry-max-time 300 \
        --connect-timeout 15 --max-time 120 "$@"
}
fetch -o SHA256SUMS "${base}/SHA256SUMS"
# Ignore all other release assets, including ZFS and other architectures.
grep -E '^[0-9a-f]{64}  linux-(headers|image)-[^/[:space:]]+_amd64\.deb$' SHA256SUMS > guest.sha256
[ "$(wc -l < guest.sha256)" -eq 2 ] || { echo 'expected one amd64 image and headers package' >&2; exit 1; }
for kind in image headers; do
    [ "$(grep -c "  linux-${kind}-" guest.sha256)" -eq 1 ] || exit 1
done
while read -r hash package; do
    fetch -o "$package" "${base}/${package}"
done < guest.sha256
sha256sum --check guest.sha256 >&2
# Used by download validation and local debugging; CI always installs.
if [ "${SMOOTHKERNEL_GUEST_DOWNLOAD_ONLY:-0}" = 1 ]; then
    exit 0
fi
packages=()
while read -r hash package; do packages+=("$package"); done < guest.sha256
dpkg -i "${packages[@]}" >&2 || apt-get -f install -y >&2
image="$(awk '$2 ~ /^linux-image-/ { print $2 }' guest.sha256)"
kver="$(dpkg-deb -f "$image" Package)"
kver="${kver#linux-image-}"
test -e "/boot/vmlinuz-${kver}"
test -d "/lib/modules/${kver}/build"
depmod "$kver" >&2
echo "GUEST_KVER=${kver}"
