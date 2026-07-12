#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "ERROR: smoothfs protocol gate must run as root" >&2
    exit 1
fi

# This gate only produces a meaningful result on a self-hosted runner that is
# provisioned as a SmoothFS protocol host (test tree, kernel module, NFS/SMB
# tooling). Its workflow is documented to "cleanly skip when no such runner
# exists"; honour that here by exiting 0 with a SKIP notice when a prerequisite
# is absent, rather than reddening CI on every branch that lands on an
# unprovisioned runner. A genuinely broken conformance run still fails loudly.
skip() {
    echo "SKIP: smoothfs protocol gate — $1."
    exit 0
}

find_test_root() {
    if [[ -n "${SMOOTHFS_TEST_ROOT:-}" ]]; then
        echo "${SMOOTHFS_TEST_ROOT}"
        return
    fi
    for candidate in \
        /opt/smoothnas/smoothfs-src/test \
        /usr/share/smoothfs-dkms \
        /usr/src/smoothfs-*/test \
        /home/virant/dev/smoothfs/src/smoothfs/test; do
        for path in ${candidate}; do
            if [[ -d "${path}" ]]; then
                echo "${path}"
                return
            fi
        done
    done
    return 1
}

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        skip "required command '$1' not present on this runner"
    fi
}

run_test() {
    local name="$1"
    local path="${TEST_ROOT}/${name}"
    if [[ ! -f "${path}" ]]; then
        echo "ERROR: missing smoothfs test ${name} under ${TEST_ROOT}" >&2
        exit 1
    fi
    echo
    echo "============================================================"
    echo "  ${name}"
    echo "============================================================"
    bash "${path}"
}

TEST_ROOT="$(find_test_root)" || \
    skip "no SmoothFS test root on this runner (set SMOOTHFS_TEST_ROOT or provision the smoothnas-protocol runner)"

require_cmd modprobe
require_cmd mount
require_cmd exportfs
require_cmd smbd
require_cmd smbclient
require_cmd smbtorture

if [[ ! -d /opt/cthon04 ]]; then
    skip "/opt/cthon04 (NFS cthon04 suite) not present on this runner"
fi

run_test cthon04.sh
run_test smbtorture.sh
run_test smb_vfs_module.sh

for spill in \
    tier_spill_basic_create.sh \
    tier_spill_nested_parent.sh \
    tier_spill_union_readdir.sh \
    tier_spill_unlink_finds_right_tier.sh \
    tier_spill_rename_xdev.sh \
    tier_spill_crash_replay.sh \
    write_staging_truncate.sh \
    metadata_tier_activity_gate.sh; do
    run_test "${spill}"
done

echo
echo "smoothfs protocol gate: PASS"
