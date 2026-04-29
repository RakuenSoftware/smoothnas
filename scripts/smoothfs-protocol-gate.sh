#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
    echo "ERROR: smoothfs protocol gate must run as root" >&2
    exit 1
fi

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
        echo "ERROR: required command missing: $1" >&2
        exit 1
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

TEST_ROOT="$(find_test_root)" || {
    echo "ERROR: could not locate smoothfs test root" >&2
    exit 1
}

require_cmd modprobe
require_cmd mount
require_cmd exportfs
require_cmd smbd
require_cmd smbclient
require_cmd smbtorture

if [[ ! -d /opt/cthon04 ]]; then
    echo "ERROR: /opt/cthon04 is required for the NFS cthon04 gate" >&2
    exit 1
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
