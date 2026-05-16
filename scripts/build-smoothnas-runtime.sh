#!/usr/bin/env bash
# Build the SmoothNAS plugin runtime daemon from a pinned LXC2Docker commit.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

LXC2DOCKER_REPO="${LXC2DOCKER_REPO:-https://github.com/games-on-whales/LXC2Docker.git}"
LXC2DOCKER_REF="${LXC2DOCKER_REF:-main}"
BUILD_DIR="${PROJECT_DIR}/runtime/build/LXC2Docker"
OUT="${PROJECT_DIR}/bin/docker-lxc-daemon"

mkdir -p "$(dirname "$BUILD_DIR")" "${PROJECT_DIR}/bin"

if [ -d "${BUILD_DIR}/.git" ]; then
    git -C "$BUILD_DIR" fetch --quiet origin "$LXC2DOCKER_REF"
else
    git clone --quiet "$LXC2DOCKER_REPO" "$BUILD_DIR"
    git -C "$BUILD_DIR" fetch --quiet origin "$LXC2DOCKER_REF"
fi

git -C "$BUILD_DIR" checkout --quiet --detach FETCH_HEAD
git -C "$BUILD_DIR" reset --quiet --hard FETCH_HEAD
git -C "$BUILD_DIR" apply "${PROJECT_DIR}/runtime/patches/lxc2docker-safe-oci-template-name.patch"
git -C "$BUILD_DIR" apply "${PROJECT_DIR}/runtime/patches/lxc2docker-preserve-smoothnas-managed-containers.patch"
git -C "$BUILD_DIR" apply "${PROJECT_DIR}/runtime/patches/lxc2docker-legacy-dir-copy-fallback.patch"
git -C "$BUILD_DIR" apply "${PROJECT_DIR}/runtime/patches/lxc2docker-reattach-bridge-veth.patch"

CGO_CFLAGS="$(pkg-config --cflags lxc 2>/dev/null || true)"
CGO_LDFLAGS="$(pkg-config --libs lxc 2>/dev/null || printf '%s' '-llxc')"
export CGO_ENABLED=1 CGO_CFLAGS CGO_LDFLAGS

go -C "$BUILD_DIR" build -o "$OUT" ./cmd/docker-lxc-daemon
echo "built $OUT from LXC2Docker $LXC2DOCKER_REF ($(git -C "$BUILD_DIR" rev-parse --short HEAD))"
