#!/bin/bash
# Build the smoothnas .deb: tierd, its web UI, the plugin runtime, their
# systemd units, and the apt configuration that keeps the NAS updated from
# the Smooth* archive.
#
# Usage: ./packaging/build-deb.sh <version> <arch> <tierd> <docker-lxc-daemon> <ui-dir> <out-dir>
#
# Depends lists everything the NAS runs on: SmoothKernel's metapackages,
# OpenZFS and smoothfs from the archive, and the Debian storage and sharing
# tools, so `apt-get install smoothnas` sets up a NAS and `apt upgrade` keeps
# all of it current.
set -euo pipefail

VERSION="${1:?Usage: build-deb.sh <version> <arch> <tierd> <docker-lxc-daemon> <ui-dir> <out-dir>}"
ARCH="${2:?}"
TIERD_BIN="${3:?}"
RUNTIME_BIN="${4:?}"
UI_DIR="${5:?}"
OUT_DIR="${6:?}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# smoothfs must be at least the release containing the SmoothFS the protocol
# gate validated; an older one lacks the fixes the gate passed on.
SMOOTHFS_MIN_VERSION="${SMOOTHFS_MIN_VERSION:-$("${SCRIPT_DIR}/../scripts/ci/smoothfs-min-version.sh")}"

DEPENDS="linux-image-smoothkernel, linux-headers-smoothkernel, \
openzfs-zfsutils, openzfs-zfs-dkms, openzfs-zfs-zed, \
smoothfs-dkms (>= ${SMOOTHFS_MIN_VERSION}), smoothfs-samba-vfs (>= ${SMOOTHFS_MIN_VERSION}), \
dkms, build-essential, libelf-dev, initramfs-tools, kmod, libc6, libpam0g, \
samba, smbclient, cifs-utils, nfs-kernel-server, targetcli-fb, python3-rtslib-fb, \
nginx, nftables, iptables, iproute2, openssl, curl, \
lxc, lxc-templates, skopeo, umoci, uidmap, docker-cli, \
btrfs-progs, xfsprogs, thin-provisioning-tools, smartmontools, hdparm, nvme-cli, \
gdisk, fio, psmisc, rsync, rclone (>= 1.60), sshpass, iperf3, mokutil, unattended-upgrades"
RECOMMENDS="bcachefs-tools, bcachefs-kernel-dkms, samba-testsuite"

[ -x "$TIERD_BIN" ] || { echo "ERROR: tierd binary not found: ${TIERD_BIN}" >&2; exit 1; }
[ -x "$RUNTIME_BIN" ] || { echo "ERROR: docker-lxc-daemon binary not found: ${RUNTIME_BIN}" >&2; exit 1; }
[ -f "$UI_DIR/index.html" ] || { echo "ERROR: UI not found in ${UI_DIR}" >&2; exit 1; }

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
chmod 755 "$root"

cp -a "$SCRIPT_DIR/smoothnas/." "$root/"
install -D -m 755 "$TIERD_BIN" "$root/usr/sbin/tierd"
install -D -m 755 "$RUNTIME_BIN" "$root/usr/lib/smoothnas/docker-lxc-daemon"
mkdir -p "$root/usr/share/tierd-ui"
cp -r "$UI_DIR/." "$root/usr/share/tierd-ui/"
chmod 755 "$root/DEBIAN/postinst" "$root/usr/sbin/smoothnas-tls-init" "$root/usr/lib/smoothnas/gpu-init.sh"

cat > "$root/DEBIAN/control" <<EOF
Package: smoothnas
Version: ${VERSION}
Architecture: ${ARCH}
Maintainer: RakuenSoftware <support@smoothnas.com>
Section: admin
Priority: optional
Installed-Size: $(du -sk --exclude=DEBIAN "$root" | cut -f1)
Depends: ${DEPENDS}
Recommends: ${RECOMMENDS}
Homepage: https://github.com/RakuenSoftware/smoothnas
Description: SmoothNAS storage appliance
 tierd, the SmoothNAS storage management daemon, its web UI and the plugin
 runtime.
EOF

mkdir -p "$OUT_DIR"
deb="$OUT_DIR/smoothnas_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$root" "$deb" >/dev/null
echo "$deb"
