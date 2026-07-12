#!/usr/bin/env bash
# Drive the SmoothFS protocol gate + mixed soak on a stock GitHub-hosted
# runner. Runs inside a debian:trixie job container (see
# .github/workflows/smoothfs-protocol-gate.yml).
#
# SmoothFS needs a >=6.18 kernel (mnt_idmap VFS API) that no GitHub runner
# image ships. So we fetch a clean >=6.18 mainline kernel, build the module
# and the Samba VFS against it, then boot that kernel under virtme-ng/QEMU
# (KVM) and run the *unmodified*, ISO-shipped gate scripts inside the guest
# (smoothfs-protocol-gate.sh + smoothfs-mixed-protocol-soak.sh).
#
# Phases are ordered so a broken virtualisation/kernel/module path fails in
# the first ~10 minutes, before the expensive Samba source build.
set -euo pipefail

log() { echo "== [gate-ci] $* =="; }

WORKDIR="$(pwd)"
SMOOTHFS_CHECKOUT="${SMOOTHFS_CHECKOUT:-deps/smoothfs}"
SMOOTHFS_DIR="$(cd "${SMOOTHFS_CHECKOUT}/src/smoothfs" && pwd)"
TEST_ROOT="${SMOOTHFS_DIR}/test"
SOAK_SECONDS="${SMOOTHFS_SOAK_SECONDS:-120}"
LOG_DIR="${WORKDIR}/gate-logs"
mkdir -p "${LOG_DIR}"

export DEBIAN_FRONTEND=noninteractive

log "smoothfs source: ${SMOOTHFS_DIR}"
[ -f "${SMOOTHFS_DIR}/dkms.conf" ] || { echo "missing ${SMOOTHFS_DIR}/dkms.conf" >&2; exit 1; }
[ -d "${TEST_ROOT}" ] || { echo "missing test root ${TEST_ROOT}" >&2; exit 1; }

# --------------------------------------------------------------------------
# Phase 1 — apt sources + light build/virtualisation deps
# --------------------------------------------------------------------------
log "Phase 1: apt sources + light deps"
# Enable deb-src (needed later for `apt-get source samba`) and add
# trixie-backports (libngtcp2-dev the trixie Samba build wants).
if ! grep -q '^Types: deb deb-src' /etc/apt/sources.list.d/debian.sources 2>/dev/null; then
    sed -i 's|^Types: deb$|Types: deb deb-src|' /etc/apt/sources.list.d/debian.sources
fi
if [ ! -f /etc/apt/sources.list.d/backports.sources ]; then
    cat > /etc/apt/sources.list.d/backports.sources <<'EOF'
Types: deb deb-src
URIs: http://deb.debian.org/debian
Suites: trixie-backports
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF
fi
apt-get update -q

apt-get install -y --no-install-recommends \
    qemu-system-x86 virtme-ng virtiofsd busybox-static zstd cpio \
    build-essential bc flex bison libelf-dev libssl-dev kmod udev \
    xfsprogs e2fsprogs python3 curl ca-certificates iproute2 \
    sudo procps util-linux git
# Ubuntu mainline kernels are built with a specific gcc; match it best-effort.
apt-get install -y --no-install-recommends gcc-15 \
    || echo "gcc-15 unavailable; module build will use default gcc"
# virtme-init powers the guest off with `poweroff`; provide it via busybox so
# a clean shutdown propagates the in-guest exit code back to vng.
bb="$(command -v busybox || echo /bin/busybox)"
ln -sf "$bb" /usr/sbin/poweroff
ln -sf "$bb" /sbin/poweroff 2>/dev/null || true

# --------------------------------------------------------------------------
# Phase 2 — fetch a >=6.18 guest kernel (image + modules + headers)
# --------------------------------------------------------------------------
log "Phase 2: fetch >=6.18 guest kernel"
bash "${TEST_ROOT}/ci_fetch_guest_kernel.sh" | tee "${LOG_DIR}/kfetch.out"
GUEST_KVER="$(sed -n 's/^GUEST_KVER=//p' "${LOG_DIR}/kfetch.out" | tail -1)"
[ -n "${GUEST_KVER}" ] || { echo "failed to determine GUEST_KVER" >&2; exit 1; }
log "guest kernel: ${GUEST_KVER}"

# --------------------------------------------------------------------------
# Phase 3 — build smoothfs.ko against the guest kernel + install into its
# module tree (so modprobe / the crash-replay reload cycle work in-guest).
# --------------------------------------------------------------------------
log "Phase 3: build smoothfs.ko"
cc="$(command -v gcc-15 || command -v gcc)"
make -C "${SMOOTHFS_DIR}" KDIR="/lib/modules/${GUEST_KVER}/build" CC="$cc" HOSTCC="$cc"
install -D -m0644 "${SMOOTHFS_DIR}/smoothfs.ko" "/lib/modules/${GUEST_KVER}/extra/smoothfs.ko"
depmod "${GUEST_KVER}"
ls -l "${SMOOTHFS_DIR}/smoothfs.ko"

# --------------------------------------------------------------------------
# Phase 4 — infra smoke: boot the guest under KVM and confirm the module
# loads. Cheap; fails fast if virtme/KVM/module are broken, before the
# expensive Samba build below.
# --------------------------------------------------------------------------
log "Phase 4: guest boot + module load smoke test"
smoke_out="${LOG_DIR}/smoke.log"
vng --run "/boot/vmlinuz-${GUEST_KVER}" --cpus 2 --memory 2G \
    --rwdir "${WORKDIR}" --user root \
    -- bash -c 'modprobe smoothfs && lsmod | grep -q "^smoothfs" && echo GATE_SMOKE_OK' \
    2>&1 | tee "${smoke_out}"
grep -q GATE_SMOKE_OK "${smoke_out}" || {
    echo "infra smoke test failed: smoothfs did not load in the guest" >&2
    exit 1
}
log "smoke test passed"

# --------------------------------------------------------------------------
# Phase 5 — protocol/soak userspace deps + Samba source for the VFS build
# --------------------------------------------------------------------------
log "Phase 5: protocol + soak userspace deps"
apt-get install -y --no-install-recommends \
    samba samba-testsuite smbclient cifs-utils \
    nfs-kernel-server nfs-common rpcbind \
    time groff-base golang-go \
    devscripts equivs debhelper dh-dkms
# trixie-backports libngtcp2 the trixie Samba build-dep set wants.
apt-get install -y --no-install-recommends -t trixie-backports \
    libngtcp2-dev libngtcp2-crypto-gnutls-dev || true
apt-get build-dep -y samba

# Lay down the matching Samba source tree at /tmp/samba-<version> for the VFS
# build (build.sh expects it there).
INSTALLED_FULL="$(dpkg-query -W -f='${Version}\n' samba 2>/dev/null | sed 's/^2://')"
INSTALLED_VER="$(echo "$INSTALLED_FULL" | sed 's/-.*//')"
if [ -n "$INSTALLED_VER" ] && [ ! -d "/tmp/samba-${INSTALLED_VER}" ]; then
    ( cd /tmp && apt-get source -y "samba=2:${INSTALLED_FULL}" )
fi

# --------------------------------------------------------------------------
# Phase 6 — cthon04 NFS conformance suite (built to /opt/cthon04)
# --------------------------------------------------------------------------
log "Phase 6: build cthon04"
if [ ! -x /opt/cthon04/basic/test1 ]; then
    rm -rf /opt/cthon04
    git clone --depth=1 https://github.com/leil-io/cthon04 /opt/cthon04
    make -C /opt/cthon04 \
        CFLAGS="-O2 -Wno-error=implicit-function-declaration -Wno-error=incompatible-pointer-types -Wno-error=int-conversion -Wno-error=implicit-int" \
        || echo "cthon04 tools/ build failed (basic/general/special do not need it)"
fi

# --------------------------------------------------------------------------
# Phase 7 — build the smoothfs Samba VFS module (installs smoothfs.so into
# the system Samba vfs dir; smb_vfs_module.sh needs it).
# --------------------------------------------------------------------------
log "Phase 7: build smoothfs Samba VFS module"
bash "${SMOOTHFS_DIR}/samba-vfs/build.sh"

# --------------------------------------------------------------------------
# Phase 8 — full gate: boot the guest and run the unmodified gate scripts.
# vng propagates the in-guest exit code, so `set -e` fails the job iff the
# gate or soak fails.
# --------------------------------------------------------------------------
log "Phase 8: protocol gate + mixed soak in-guest"
vng --run "/boot/vmlinuz-${GUEST_KVER}" --cpus 4 --memory 4G \
    --rwdir "${WORKDIR}" --user root \
    -- env \
      "SMOOTHFS_TEST_ROOT=${TEST_ROOT}" \
      "SMOOTHFS_SOAK_SECONDS=${SOAK_SECONDS}" \
      "SMOOTHFS_CI_WORKDIR=${WORKDIR}" \
      "GATE_LOG_DIR=${LOG_DIR}" \
      bash "${WORKDIR}/scripts/ci/smoothfs-protocol-gate-invm.sh"

log "gate complete"
