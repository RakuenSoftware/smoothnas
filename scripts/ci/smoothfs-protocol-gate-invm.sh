#!/usr/bin/env bash
# Runs INSIDE the virtme-ng guest (clean >=6.18 kernel, no production
# smoothfs loaded). Brings up the prerequisites the gate scripts expect on a
# systemd host — but the guest has no init — then runs the *unmodified*,
# ISO-shipped gate scripts. vng propagates this script's exit code to the
# host, so the CI step fails iff the gate or soak fails.
#
# virtme routes guest stdout to the host but stderr to /dev/null, so fold
# stderr into stdout to capture everything.
set -uo pipefail
exec 2>&1

WORKDIR="${SMOOTHFS_CI_WORKDIR:-$PWD}"
cd "${WORKDIR}" || { echo "guest: cd ${WORKDIR} failed"; exit 1; }
GATE_LOG_DIR="${GATE_LOG_DIR:-${WORKDIR}/gate-logs}"
mkdir -p "${GATE_LOG_DIR}"

export PATH="/usr/local/go/bin:${PATH}"
export HOME="${HOME:-/root}"

# --------------------------------------------------------------------------
# systemctl shim. The gate scripts (cthon04.sh, mixed soak) drive nfsd via
# `systemctl start nfs-server`, but the guest runs no init. Intercept those
# calls and bring kernel nfsd / rpcbind up (or down) by hand.
# --------------------------------------------------------------------------
SHIM_DIR="$(mktemp -d)"
cat > "${SHIM_DIR}/systemctl" <<'SHIM'
#!/usr/bin/env bash
# Minimal systemctl shim for the CI guest (no init system).
verb="${1:-}"; shift || true
nfsd_up() {
    modprobe nfsd 2>/dev/null || true
    modprobe nfsv4 2>/dev/null || true
    # Dirs systemd's nfs-server + nfsdcld normally create. v4recovery is
    # required for NFSv4 client-state tracking; without nfsdcld + this dir the
    # v4 pseudo-fs LOOKUP fails and mounts get ENOENT ("No such file or
    # directory") even though NFSv3 works.
    mkdir -p /var/lib/nfs/rpc_pipefs /var/lib/nfs/v4recovery \
             /var/lib/nfs/sm /var/lib/nfs/sm.bak /run/rpcbind 2>/dev/null || true
    [ -f /var/lib/nfs/etab ] || : > /var/lib/nfs/etab 2>/dev/null || true
    mountpoint -q /proc/fs/nfsd || mount -t nfsd nfsd /proc/fs/nfsd 2>/dev/null || true
    mountpoint -q /var/lib/nfs/rpc_pipefs || \
        mount -t rpc_pipefs sunrpc /var/lib/nfs/rpc_pipefs 2>/dev/null || true
    pgrep -x rpcbind >/dev/null 2>&1 || rpcbind 2>/dev/null || true
    # NFSv4 client-tracking daemon — modern nfsd needs it running before it
    # will serve v4 (systemd starts nfsdcld.service as a dependency).
    pgrep -x nfsdcld >/dev/null 2>&1 || nfsdcld 2>/dev/null || true
    pgrep -x rpc.statd >/dev/null 2>&1 || rpc.statd 2>/dev/null || true
    # Enable v3 + v4.x (incl 4.2) explicitly. Only writable while nfsd has 0
    # threads, so this must precede rpc.nfsd.
    for v in +3 +4 +4.1 +4.2; do
        echo "$v" > /proc/fs/nfsd/versions 2>/dev/null || true
    done
    rpc.nfsd 8 2>/dev/null || true
    exportfs -r 2>/dev/null || true
    pgrep -x rpc.mountd >/dev/null 2>&1 || rpc.mountd 2>/dev/null || true
    # Surface what actually came up so an NFSv4 failure is diagnosable.
    echo "[systemctl-shim] nfsd versions: $(cat /proc/fs/nfsd/versions 2>/dev/null)" >&2
}
nfsd_down() {
    rpc.nfsd 0 2>/dev/null || true
    pkill -x rpc.mountd 2>/dev/null || true
}
case "${verb}" in
    start)
        for svc in "$@"; do
            case "${svc}" in
                rpcbind) pgrep -x rpcbind >/dev/null 2>&1 || rpcbind 2>/dev/null || true ;;
                nfs-server|nfs-kernel-server) nfsd_up ;;
            esac
        done ;;
    stop)
        for svc in "$@"; do
            case "${svc}" in
                nfs-server|nfs-kernel-server) nfsd_down ;;
                smbd|nmbd|samba|samba-ad-dc|winbind) pkill -x "${svc}" 2>/dev/null || true ;;
            esac
        done ;;
    *) : ;;  # status/is-active/etc. — treat as success
esac
exit 0
SHIM
chmod +x "${SHIM_DIR}/systemctl"
export PATH="${SHIM_DIR}:${PATH}"

echo "== guest: kernel $(uname -r) =="

echo "== guest: loading smoothfs =="
modprobe smoothfs || insmod "${SMOOTHFS_TEST_ROOT}/../smoothfs.ko" || {
    echo "guest: failed to load smoothfs"; exit 1; }
lsmod | grep -q '^smoothfs' || { echo "guest: smoothfs not loaded"; exit 1; }

export SMOOTHFS_TEST_ROOT="${SMOOTHFS_TEST_ROOT}"

rc=0

echo
echo "############################################################"
echo "#  SmoothFS protocol conformance gate"
echo "############################################################"
if bash "${WORKDIR}/scripts/smoothfs-protocol-gate.sh"; then
    echo "== protocol gate: PASS =="
else
    rc=$?
    echo "== protocol gate: FAIL (status ${rc}) =="
fi

if [ "${rc}" -eq 0 ]; then
    echo
    echo "############################################################"
    echo "#  Mixed NFS/SMB/local SmoothFS soak (${SMOOTHFS_SOAK_SECONDS:-120}s)"
    echo "############################################################"
    if bash "${WORKDIR}/scripts/smoothfs-mixed-protocol-soak.sh"; then
        echo "== mixed soak: PASS =="
    else
        rc=$?
        echo "== mixed soak: FAIL (status ${rc}) =="
    fi
fi

# Best-effort: preserve any smbd/test logs (guest /tmp is ephemeral) into the
# workspace-shared log dir so the workflow can upload them.
if [ "${rc}" -ne 0 ]; then
    echo "== guest: capturing diagnostics =="
    dmesg 2>/dev/null | grep -iE 'smoothfs|nfsd|cifs' | tail -120 > "${GATE_LOG_DIR}/guest-dmesg.log" 2>/dev/null || true
    for d in /tmp/cthon-smoothfs /tmp/smbtorture-smoothfs /tmp/smoothfs-vfs58 /tmp/smoothnas-smoothfs-soak; do
        [ -d "$d" ] && cp -a "$d" "${GATE_LOG_DIR}/$(basename "$d")-guest" 2>/dev/null || true
    done
fi

echo "== guest: gate exit ${rc} =="
exit "${rc}"
