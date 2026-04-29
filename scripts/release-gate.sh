#!/usr/bin/env bash
set -euo pipefail

HOST="${SMOOTHNAS_HOST:-192.168.0.204}"
USER="${SMOOTHNAS_USER:-root}"
PASS="${SMOOTHNAS_PASS:-}"
DESTRUCTIVE="${SMOOTHNAS_RELEASE_GATE_DESTRUCTIVE:-0}"

failures=0

ssh_cmd=(ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8)
if [[ -n "${PASS}" ]]; then
  ssh_cmd=(sshpass -p "${PASS}" "${ssh_cmd[@]}")
fi

remote() {
  "${ssh_cmd[@]}" "${USER}@${HOST}" "$@"
}

check() {
  local name="$1"
  shift
  printf '==> %s\n' "${name}"
  if "$@"; then
    printf 'PASS %s\n\n' "${name}"
  else
    printf 'FAIL %s\n\n' "${name}"
    failures=$((failures + 1))
  fi
}

check_remote() {
  local name="$1"
  shift
  check "${name}" remote "$@"
}

check_remote "tierd service is active" "systemctl is-active --quiet tierd"
check_remote "SMB service is active" "systemctl is-active --quiet smbd"
check_remote "NFS service is active" "systemctl is-active --quiet nfs-server || systemctl is-active --quiet nfs-kernel-server"
check_remote "tierd health endpoint responds" "curl -fsS http://127.0.0.1:8420/api/health | grep -q '\"status\"'"
check_remote "SMB defaults are async and performance oriented" "testparm -s 2>/dev/null | grep -qi 'strict sync = No' && testparm -s 2>/dev/null | grep -qi 'case sensitive = Yes' && ! testparm -s 2>/dev/null | grep -qi 'vfs objects = smoothfs'"
check_remote "NFS exports default async when present" "if [ -s /etc/exports ]; then ! grep -E '\\bsync\\b' /etc/exports >/dev/null || grep -E '\\basync\\b' /etc/exports >/dev/null; fi"
check_remote "No active failed units" "test -z \"\$(systemctl --failed --no-legend)\""

check_remote "Quick NFS create/delete smoke if mounted" "set -e; d=/mnt/media/nfs-perf/.release-gate; if [ ! -d /mnt/media/nfs-perf ]; then exit 0; fi; rm -rf \"\$d\"; mkdir -p \"\$d\"; start=\$(date +%s%N); for i in \$(seq 1 1000); do printf gate > \"\$d/file-\$i\"; done; sync; rm -rf \"\$d\"; end=\$(date +%s%N); ms=\$(( (end-start)/1000000 )); echo \"nfs smoke: 1000 files in \${ms}ms\""
check_remote "Quick SMB create/delete smoke if mounted" "set -e; d=/mnt/media/smbperf/.release-gate; if [ ! -d /mnt/media/smbperf ]; then exit 0; fi; rm -rf \"\$d\"; mkdir -p \"\$d\"; start=\$(date +%s%N); for i in \$(seq 1 1000); do printf gate > \"\$d/file-\$i\"; done; sync; rm -rf \"\$d\"; end=\$(date +%s%N); ms=\$(( (end-start)/1000000 )); echo \"smb smoke: 1000 files in \${ms}ms\""

if [[ "${DESTRUCTIVE}" == "1" ]]; then
  check_remote "Destructive storage gate is explicitly armed" "test -n \"\${SMOOTHNAS_RELEASE_GATE_DISKS:-}\""
  printf 'Destructive storage lifecycle requires SMOOTHNAS_RELEASE_GATE_DISKS on the appliance and is intentionally not run implicitly.\n\n'
else
  printf 'SKIP destructive storage lifecycle. Set SMOOTHNAS_RELEASE_GATE_DESTRUCTIVE=1 and provide a dedicated disk list to arm it.\n\n'
fi

if [[ "${failures}" -ne 0 ]]; then
  printf '%d release gate check(s) failed.\n' "${failures}" >&2
  exit 1
fi

printf 'Release gate passed for %s@%s.\n' "${USER}" "${HOST}"
