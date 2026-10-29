#!/bin/bash
# Print the version of the first SmoothFS release that contains the ref
# SmoothNAS's protocol gate validates (scripts/ci/smoothfs-ref).
#
# The gate tests that ref, but NASes install smoothfs-dkms from the Smooth*
# archive, which only carries releases. If the ref is in no release, the
# gate has validated code that no NAS will run, so this fails the build.
# The printed version becomes the smoothnas package's minimum smoothfs
# dependency, so installing or upgrading SmoothNAS pulls in the fixes the
# gate validated.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REF="$(tr -d '[:space:]' < "${SCRIPT_DIR}/smoothfs-ref")"
[ -n "${REF}" ] || { echo "scripts/ci/smoothfs-ref is empty" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
git -C "${WORK}" init -q
git -C "${WORK}" fetch -q --filter=blob:none --tags \
    https://github.com/RakuenSoftware/smoothfs.git "+refs/heads/main:refs/remotes/origin/main"
git -C "${WORK}" fetch -q --filter=blob:none \
    https://github.com/RakuenSoftware/smoothfs.git "${REF}"

for tag in $(git -C "${WORK}" tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=version:refname); do
    if git -C "${WORK}" merge-base --is-ancestor "${REF}" "${tag}"; then
        echo "${tag#v}"
        exit 0
    fi
done

echo "SmoothFS ${REF} (scripts/ci/smoothfs-ref) is in no SmoothFS release." >&2
echo "The protocol gate validates that ref, but NASes install smoothfs from released" >&2
echo "packages only, so they would not get it. Merge it to smoothfs main and release" >&2
echo "it, or pin a released ref." >&2
exit 1
