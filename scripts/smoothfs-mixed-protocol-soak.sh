#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "ERROR: mixed protocol soak must run as root" >&2
    exit 1
fi

# Like the protocol gate, this soak only runs on a self-hosted runner
# provisioned with the SmoothFS stack. Its workflow is documented to "cleanly
# skip when no such runner exists", so exit 0 with a SKIP notice when a
# prerequisite is missing instead of failing CI on every branch.
skip() {
    echo "SKIP: mixed protocol soak — $1."
    exit 0
}

ROOT="${SMOOTHFS_SOAK_ROOT:-/tmp/smoothnas-smoothfs-soak}"
UUID="${SMOOTHFS_SOAK_UUID:-66666666-6666-6666-6666-666666666666}"
PORT="${SMOOTHFS_SOAK_SMB_PORT:-9445}"
SECONDS_TO_RUN="${SMOOTHFS_SOAK_SECONDS:-60}"
SHARE=smoothfs
SMBD_PID=""

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        skip "required command '$1' not present on this runner"
    fi
}

cleanup() {
    set +e
    exportfs -u "127.0.0.1:${ROOT}/server" 2>/dev/null
    umount -l "${ROOT}/nfs" "${ROOT}/cifs" "${ROOT}/server" "${ROOT}/fast" "${ROOT}/slow" 2>/dev/null
    if [[ -n "${SMBD_PID}" ]] && kill -0 "${SMBD_PID}" 2>/dev/null; then
        pkill -9 -f "smbd.*${ROOT}/samba/smb.conf" 2>/dev/null
        wait "${SMBD_PID}" 2>/dev/null
    fi
    rm -rf "${ROOT}"
}
trap cleanup EXIT

require_cmd modprobe
require_cmd mkfs.xfs
require_cmd exportfs
require_cmd smbd
require_cmd mount.cifs

# The soak mounts smoothfs directly; without the kernel module this runner
# cannot host the test, so skip rather than fail.
if ! lsmod 2>/dev/null | grep -q '^smoothfs' && ! modinfo smoothfs >/dev/null 2>&1; then
    skip "smoothfs kernel module not available on this runner"
fi

echo "=== preparing smoothfs two-tier loopback pool ==="
cleanup
mkdir -p "${ROOT}"/{fast,slow,server,nfs,cifs,samba/private}
truncate -s 2G "${ROOT}/fast.img" "${ROOT}/slow.img"
mkfs.xfs -q -f "${ROOT}/fast.img"
mkfs.xfs -q -f "${ROOT}/slow.img"
mount -o loop "${ROOT}/fast.img" "${ROOT}/fast"
mount -o loop "${ROOT}/slow.img" "${ROOT}/slow"
modprobe smoothfs
mount -t smoothfs -o "pool=soak,uuid=${UUID},tiers=${ROOT}/fast:${ROOT}/slow" none "${ROOT}/server"
chmod 1777 "${ROOT}/server"

# Bound a setup step that can wedge in the kernel (an nfsd thread stuck
# inside smoothfs blocks exportfs/mount.nfs in uninterruptible sleep, seen on
# gate runs 29265262327/29265253818: no output between "exporting" and the
# 5400s guest wall-clock kill). Fail in minutes with kernel stacks instead.
bounded() {
    local secs="$1"; shift
    local rc=0
    timeout -k 10 "${secs}" "$@" || rc=$?
    if (( rc == 124 || rc == 137 )); then
        echo "ERROR: '$*' wedged >${secs}s; D-state tasks and kernel stacks:" >&2
        ps -eo pid,stat,wchan:32,args | awk '$2 ~ /D/' >&2 || true
        local p
        for p in $(ps -eo pid,stat | awk '$2 ~ /D/ {print $1}'); do
            echo "--- /proc/${p}/stack ---" >&2
            cat "/proc/${p}/stack" 2>/dev/null >&2 || true
        done
        dmesg | tail -100 | sed 's/^/  dmesg: /' >&2
        exit 1
    fi
    return "${rc}"
}

echo "=== exporting over NFS and SMB ==="
systemctl start rpcbind nfs-server 2>/dev/null || systemctl start rpcbind nfs-kernel-server
bounded 120 exportfs -o "rw,async,no_root_squash,no_subtree_check,fsid=${UUID}" "127.0.0.1:${ROOT}/server"
bounded 300 mount -t nfs -o vers=4.2,timeo=50,retrans=3 "127.0.0.1:${ROOT}/server" "${ROOT}/nfs"

cat > "${ROOT}/samba/smb.conf" <<EOF
[global]
    workgroup = WORKGROUP
    server role = standalone server
    map to guest = Bad User
    log file = ${ROOT}/samba/log.%m
    pid directory = ${ROOT}/samba
    lock directory = ${ROOT}/samba
    state directory = ${ROOT}/samba
    cache directory = ${ROOT}/samba
    private dir = ${ROOT}/samba/private
    smb ports = ${PORT}
    bind interfaces only = yes
    interfaces = lo
    disable spoolss = yes
    load printers = no
    ea support = yes
    store dos attributes = yes
    kernel oplocks = yes
    server min protocol = SMB2_10

[${SHARE}]
    path = ${ROOT}/server
    read only = no
    guest ok = yes
    force user = root
    ea support = yes
    vfs objects = smoothfs
    create mask = 0664
    directory mask = 0775
EOF

smbd --foreground --no-process-group --configfile="${ROOT}/samba/smb.conf" &
SMBD_PID=$!
sleep 2
bounded 300 mount -t cifs "//127.0.0.1/${SHARE}" "${ROOT}/cifs" -o "guest,port=${PORT},vers=3.1.1,noserverino"

# Each writer keeps a sliding window of files rather than accumulating: the
# old delete-every-5th scheme grew the working set without bound, so a fast
# enough run filled the 2x2G pool mid-soak. ENOSPC then left zero-length
# .tmp files that the post-soak integrity check flagged — the soak failed on
# its own space budget, not on smoothfs. 3 writers x 25 files x 16M ≈ 1.2G
# keeps steady-state churn well inside the 4G pool at any throughput.
writer() {
    local dir="$1"
    local prefix="$2"
    local deadline=$((SECONDS + SECONDS_TO_RUN))
    local i=0
    local window=24
    mkdir -p "${dir}/${prefix}"
    while (( SECONDS < deadline )); do
        dd if=/dev/zero of="${dir}/${prefix}/file-${i}.tmp" bs=1M count=16 conv=fsync status=none
        mv "${dir}/${prefix}/file-${i}.tmp" "${dir}/${prefix}/file-${i}.bin"
        if (( i >= window )); then
            rm -f "${dir}/${prefix}/file-$((i - window)).bin"
        fi
        i=$((i + 1))
    done
}

echo "=== running concurrent local + NFS + SMB writers for ${SECONDS_TO_RUN}s ==="
writer "${ROOT}/server" local &
p1=$!
writer "${ROOT}/nfs" nfs &
p2=$!
writer "${ROOT}/cifs" smb &
p3=$!
soak_rc=0
for p in "${p1}" "${p2}" "${p3}"; do
    wait "${p}" || { echo "ERROR: a soak writer exited non-zero" >&2; soak_rc=1; }
done
[[ ${soak_rc} -eq 0 ]] || exit 1

sync
mapfile -t zeroes < <(find "${ROOT}/server" -type f -size 0)
if (( ${#zeroes[@]} > 0 )); then
    echo "ERROR: found ${#zeroes[@]} zero-length file(s) after soak:" >&2
    for f in "${zeroes[@]}"; do
        rel="${f#"${ROOT}"/server/}"
        stat -c '  union %s bytes  %n' "$f" >&2
        for tier in fast slow; do
            tf="${ROOT}/${tier}/${rel}"
            if [[ -e "${tf}" ]]; then
                stat -c "  ${tier}  %s bytes  %n" "${tf}" >&2
                getfattr -d -m 'trusted\.smoothfs\.' --absolute-names "${tf}" 2>/dev/null | sed 's/^/    /' >&2
            else
                echo "  ${tier}  <absent>" >&2
            fi
        done
    done
    dmesg | tail -100 | grep -i smoothfs | sed 's/^/  dmesg: /' >&2
    exit 1
fi

if dmesg | tail -300 | grep -E 'smoothfs:.*(BUG|WARN|corrupt|lost|panic|Oops)' >/tmp/smoothfs-soak-dmesg.txt; then
    echo "ERROR: suspicious smoothfs dmesg lines:" >&2
    cat /tmp/smoothfs-soak-dmesg.txt >&2
    exit 1
fi

echo "smoothfs mixed protocol soak: PASS"
