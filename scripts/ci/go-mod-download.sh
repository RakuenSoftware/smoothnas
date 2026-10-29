#!/bin/bash
# Download a Go module's dependencies, retrying transient proxy failures.
# proxy.golang.org intermittently resets streams ("stream error ...
# INTERNAL_ERROR"), which failed whole CI jobs; a retry gets through.
# Usage: scripts/ci/go-mod-download.sh <module dir>
set -euo pipefail
dir="${1:?usage: go-mod-download.sh <module dir>}"
for attempt in 1 2 3 4 5; do
    if go -C "$dir" mod download; then
        exit 0
    fi
    echo "go mod download in $dir failed (attempt $attempt); retrying" >&2
    sleep $((attempt * 10))
done
echo "::error title=go mod download failed::go mod download in $dir failed 5 times" >&2
exit 1
