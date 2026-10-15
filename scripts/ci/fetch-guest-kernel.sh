#!/usr/bin/env bash
# Run the pinned SmoothFS kernel fetcher with bounded transport retries.
# Keep retries scoped to downloads; protocol or module test failures are final.
set -euo pipefail

curl() {
    command curl --retry 5 --retry-delay 2 --retry-max-time 300 \
        --connect-timeout 15 --max-time 120 "$@"
}
export -f curl
exec bash "${1:?usage: fetch-guest-kernel.sh <pinned-fetch-script>}"
