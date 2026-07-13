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

# Run a command in a virtme-ng guest with a hard wall-clock bound. virtme-ng
# can hang indefinitely if the guest fails to boot or never powers off, and a
# bare `vng` would then consume the whole job timeout with no output. Bound it,
# read stdin from /dev/null (no tty in CI — avoid any interactive wait), and
# treat a timeout as a normal failure. Usage: run_guest <secs> <vng-args...>
run_guest() {
    local secs="$1"; shift
    local rc=0
    timeout -k 30 "${secs}" vng "$@" </dev/null || rc=$?
    if [ "${rc}" -eq 124 ] || [ "${rc}" -eq 137 ]; then
        echo "ERROR: guest run exceeded ${secs}s wall-clock (rc=${rc}); see boot console above" >&2
        return 124
    fi
    return "${rc}"
}

# Host-side logs go to the checkout's gate-logs so the workflow's
# upload-artifact step (which reads ${{ github.workspace }}/gate-logs) finds
# them. Captured before we stage/cd elsewhere.
LOG_DIR="$(pwd)/gate-logs"
mkdir -p "${LOG_DIR}"

SMOOTHFS_CHECKOUT="${SMOOTHFS_CHECKOUT:-deps/smoothfs}"
SOAK_SECONDS="${SMOOTHFS_SOAK_SECONDS:-120}"

export DEBIAN_FRONTEND=noninteractive

# --------------------------------------------------------------------------
# Phase 0 — stage the checkout under the container root filesystem.
#
# Inside a GitHub `container:` job the workspace (/__w/...) is a bind mount,
# NOT part of the container's root fs. virtme-ng shares the container root
# into the guest (with a writable overlay) but cannot overlay a separate
# bind mount — so the guest would not see scripts / the smoothfs test tree
# there, and `--rwdir <workspace>` fails with "path must be defined inside a
# valid overlay". Copy everything under / (here /opt/gate) so virtme's
# default root share carries it into the guest.
# --------------------------------------------------------------------------
WORKDIR=/opt/gate
log "Phase 0: stage checkout to ${WORKDIR}"
rm -rf "${WORKDIR}"
mkdir -p "${WORKDIR}"
cp -a ./. "${WORKDIR}/"
cd "${WORKDIR}"

SMOOTHFS_DIR="${WORKDIR}/${SMOOTHFS_CHECKOUT}/src/smoothfs"
TEST_ROOT="${SMOOTHFS_DIR}/test"

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
log "Phase 4: virt diagnostics + guest boot smoke test"
echo "--- /dev/kvm ---";  ls -l /dev/kvm 2>&1 || echo "NO /dev/kvm (guest will fall back to slow TCG emulation)"
echo "--- qemu ---";      qemu-system-x86_64 --version 2>&1 | head -1 || true
echo "--- virtme-ng ---"; vng --version 2>&1 | head -1 || true

# GitHub runners are themselves VMs, and the mainline guest kernel stalls in
# early ACPI/timer init under *nested* KVM (it hung identically right after
# "ACPI: Core revision" with 1 vs 2 CPUs and with/without KASLR). Emulate the
# CPU with TCG instead (--disable-kvm) to sidestep nested virt entirely; the
# Samba/module builds run natively on the host, so only the in-guest tests pay
# the emulation cost. GATE_GUEST_ARGS carries the same config into Phase 8.
GATE_GUEST_ARGS=(--disable-kvm --append nokaslr)
smoke_out="${LOG_DIR}/smoke.log"
smoke_rc=0
run_guest 600 --verbose "${GATE_GUEST_ARGS[@]}" \
    --run "/boot/vmlinuz-${GUEST_KVER}" --cpus 2 --memory 2G --user root \
    -- bash -c 'echo GUEST_ALIVE; modprobe smoothfs && lsmod | grep -q "^smoothfs" && echo GATE_SMOKE_OK' \
    > "${smoke_out}" 2>&1 || smoke_rc=$?
cat "${smoke_out}"
if [ "${smoke_rc}" -ne 0 ]; then
    echo "infra smoke test failed (rc=${smoke_rc}): guest did not boot/load smoothfs" >&2
    exit 1
fi
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
cp -f /tmp/smoothfs-vfs-*.log "${LOG_DIR}/" 2>/dev/null || true
# Hard-bounded so a wedged in-guest test can't consume the whole job timeout
# (the release gate polls the workflow conclusion for up to 120 min).
run_guest 5400 "${GATE_GUEST_ARGS[@]}" \
    --run "/boot/vmlinuz-${GUEST_KVER}" --cpus 4 --memory 4G --user root \
    -- env \
      "SMOOTHFS_TEST_ROOT=${TEST_ROOT}" \
      "SMOOTHFS_SOAK_SECONDS=${SOAK_SECONDS}" \
      "SMOOTHFS_CI_WORKDIR=${WORKDIR}" \
      "GATE_LOG_DIR=${WORKDIR}/gate-logs" \
      bash "${WORKDIR}/scripts/ci/smoothfs-protocol-gate-invm.sh"

log "gate complete"
