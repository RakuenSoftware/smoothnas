#!/usr/bin/env bash
set -euo pipefail

if [[ $# -eq 0 ]]; then
    echo "usage: $0 <command> [args...]" >&2
    exit 2
fi

if [[ "${SMOOTHNAS_LOW_IMPACT_DISABLE:-0}" == "1" || "${SMOOTHNAS_LOW_IMPACT_APPLIED:-0}" == "1" ]]; then
    exec "$@"
fi

nice_level="${BUILD_NICE_LEVEL:-15}"
ionice_class="${BUILD_IONICE_CLASS:-2}"
ionice_level="${BUILD_IONICE_LEVEL:-7}"

cmd=("$@")

if command -v ionice >/dev/null 2>&1; then
    if [[ "$ionice_class" == "3" ]]; then
        cmd=(ionice -c 3 "${cmd[@]}")
    else
        cmd=(ionice -c "$ionice_class" -n "$ionice_level" "${cmd[@]}")
    fi
fi

if command -v nice >/dev/null 2>&1; then
    cmd=(nice -n "$nice_level" "${cmd[@]}")
fi

exec env SMOOTHNAS_LOW_IMPACT_APPLIED=1 "${cmd[@]}"
