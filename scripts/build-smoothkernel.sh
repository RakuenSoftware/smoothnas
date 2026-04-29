#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SMOOTHKERNEL_DIR="${SMOOTHKERNEL_DIR:-$(cd "${SCRIPT_DIR}/../../smoothkernel" && pwd)}"
LOW_IMPACT="${SCRIPT_DIR}/low-impact-build.sh"

if [[ ! -d "${SMOOTHKERNEL_DIR}" || ! -f "${SMOOTHKERNEL_DIR}/Makefile" ]]; then
    echo "ERROR: smoothkernel checkout not found at ${SMOOTHKERNEL_DIR}" >&2
    exit 1
fi

if [[ $# -eq 0 ]]; then
    set -- kernel
fi

if [[ -z "${BUILD_THREADS:-}" ]]; then
    cpu_count="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || echo 1)"
    if [[ "$cpu_count" =~ ^[0-9]+$ ]] && (( cpu_count > 1 )); then
        build_threads=$((cpu_count / 2))
        if (( build_threads > 8 )); then
            build_threads=8
        fi
        if (( build_threads < 1 )); then
            build_threads=1
        fi
    else
        build_threads=1
    fi
    export BUILD_THREADS="${build_threads}"
fi

echo "==> smoothkernel dir: ${SMOOTHKERNEL_DIR}"
echo "==> BUILD_THREADS=${BUILD_THREADS}"

for target in "$@"; do
    echo "==> make ${target}"
    (
        cd "${SMOOTHKERNEL_DIR}"
        "${LOW_IMPACT}" make "${target}"
    )
done
