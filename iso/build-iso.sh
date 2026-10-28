#!/bin/bash
# Build a SmoothNAS installer ISO based on Debian 13 (Trixie).
#
# Usage: ./iso/build-iso.sh <version>
#
# Wraps the generic smoothiso builder with SmoothNAS-specific config and
# hooks. The installer uses a whiptail text UI on the boot console;
# smoothiso/installer.sh drives the flow and sources the project hooks
# under iso/hooks/.
#
# The installer installs the smoothnas package (built here, or passed in
# SMOOTHNAS_DEB) and downloads everything it depends on — SmoothKernel,
# OpenZFS, smoothfs — from the Smooth* archive.
#
# Required sibling repo (override with SMOOTHISO_DIR if checked out elsewhere):
#   ../smoothiso        — generic Debian-installer ISO builder
#
# Override env vars: SMOOTHISO_DIR, SMOOTHNAS_DEB, DEBIAN_MIRROR.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
VERSION="${1:?Usage: ./iso/build-iso.sh <version>}"

# Debian architecture for the produced ISO and its smoothnas package.
DEB_ARCH="${DEB_ARCH:-amd64}"
case "$DEB_ARCH" in
    amd64|arm64) ;;
    *) echo "ERROR: unsupported DEB_ARCH '${DEB_ARCH}' (expected amd64 or arm64)" >&2; exit 1 ;;
esac
DEBIAN_MIRROR="${DEBIAN_MIRROR:-http://deb.debian.org/debian}"
CACHE_DIR="${PROJECT_DIR}/iso/cache"
WORK_DIR="${PROJECT_DIR}/iso/work"
OUTPUT_DIR="${PROJECT_DIR}/iso/output"
ISO_FILE="${OUTPUT_DIR}/smoothnas-${VERSION}-${DEB_ARCH}.iso"
HOOKS_DIR="${SCRIPT_DIR}/hooks"
SMOOTHISO_DIR="${SMOOTHISO_DIR:-${PROJECT_DIR}/../smoothiso}"
SMOOTHISO_PATCH_DIR="${SCRIPT_DIR}/smoothiso-patches"
SMOOTHISO_BUILD_DIR=""

prepare_smoothiso_source() {
    SMOOTHISO_BUILD_DIR="$SMOOTHISO_DIR"
    if [ ! -d "$SMOOTHISO_PATCH_DIR" ]; then
        return
    fi

    local patch_count=0
    shopt -s nullglob
    local patches=("$SMOOTHISO_PATCH_DIR"/*.patch)
    shopt -u nullglob
    patch_count=${#patches[@]}
    if [ "$patch_count" -eq 0 ]; then
        return
    fi

    SMOOTHISO_BUILD_DIR="${CACHE_DIR}/smoothiso-patched"
    rm -rf "$SMOOTHISO_BUILD_DIR"
    mkdir -p "$CACHE_DIR" "$WORK_DIR"
    cp -a "$SMOOTHISO_DIR" "$SMOOTHISO_BUILD_DIR"

    local patch
    for patch in "${patches[@]}"; do
        if git -C "$SMOOTHISO_BUILD_DIR" apply --reverse --check "$patch" >/dev/null 2>&1; then
            echo "  Smoothiso patch already present: $(basename "$patch")"
            continue
        fi
        echo "  Applying smoothiso patch: $(basename "$patch")"
        git -C "$SMOOTHISO_BUILD_DIR" apply "$patch" || {
            echo "ERROR: failed to apply smoothiso patch $(basename "$patch")." >&2
            exit 1
        }
    done
}

prepare_smoothnas_payload() {
    local payload_dir="$1"
    local base_dir="${PROJECT_DIR}"

    mkdir -p "$payload_dir"

    if [ -z "${SMOOTHNAS_DEB:-}" ]; then
        SMOOTHNAS_DEB=$("${base_dir}/packaging/build-deb.sh" "$VERSION" "$DEB_ARCH" \
            "${base_dir}/bin/tierd" "${base_dir}/bin/docker-lxc-daemon" \
            "${base_dir}/tierd-ui/dist/smoothnas-ui" "${WORK_DIR}/deb")
    fi
    [ "$(dpkg-deb -f "$SMOOTHNAS_DEB" Architecture)" = "$DEB_ARCH" ] || {
        echo "ERROR: ${SMOOTHNAS_DEB} is not a ${DEB_ARCH} package." >&2
        exit 1
    }
    cp "$SMOOTHNAS_DEB" "$payload_dir/smoothnas.deb"
    cp "${base_dir}/packaging/smoothnas/usr/share/keyrings/smooth-archive-keyring.gpg" \
        "$payload_dir/smooth-archive-keyring.gpg"

    cp "$SCRIPT_DIR/90-smoothnas-net.conf" "$payload_dir/90-smoothnas-net.conf"

    mkdir -p "$payload_dir/tests"
    for test_script in \
        "${base_dir}/scripts/smoothfs-protocol-gate.sh" \
        "${base_dir}/scripts/smoothfs-mixed-protocol-soak.sh" \
        "${base_dir}/scripts/smoothfs-windows-smb-soak.ps1"; do
        [ -f "$test_script" ] && cp "$test_script" "$payload_dir/tests/"
    done
}

main() {
    echo "=== SmoothNAS ISO Builder v${VERSION} ==="

    if [ ! -d "$SMOOTHISO_DIR" ]; then
        echo "ERROR: smoothiso source tree not found at ${SMOOTHISO_DIR}."
        exit 1
    fi
    if [ ! -d "$HOOKS_DIR" ]; then
        echo "ERROR: SmoothNAS hooks directory not found at ${HOOKS_DIR}."
        exit 1
    fi

    prepare_smoothiso_source

    if [ ! -f "${PROJECT_DIR}/bin/tierd" ]; then
        local host_arch
        host_arch="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
        if [ "$host_arch" != "$DEB_ARCH" ]; then
            echo "ERROR: bin/tierd missing and host arch ${host_arch} != target ${DEB_ARCH}." >&2
            echo "       Pre-build the backend on a ${DEB_ARCH} host (CGO requires native toolchain)." >&2
            exit 1
        fi
        echo "  bin/tierd not found, building backend..."
        (cd "${PROJECT_DIR}/tierd" && CGO_ENABLED=1 go build -o ../bin/tierd ./cmd/tierd/) || {
            echo "ERROR: backend build failed."
            exit 1
        }
    fi
    if [ ! -f "${PROJECT_DIR}/bin/docker-lxc-daemon" ]; then
        local host_arch
        host_arch="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
        if [ "$host_arch" != "$DEB_ARCH" ]; then
            echo "ERROR: bin/docker-lxc-daemon missing and host arch ${host_arch} != target ${DEB_ARCH}." >&2
            echo "       Pre-build the runtime on a ${DEB_ARCH} host (CGO requires native liblxc)." >&2
            exit 1
        fi
        echo "  bin/docker-lxc-daemon not found, building SmoothNAS runtime..."
        "${PROJECT_DIR}/scripts/build-smoothnas-runtime.sh" || {
            echo "ERROR: SmoothNAS runtime build failed."
            exit 1
        }
    fi
    if [ ! -d "${PROJECT_DIR}/tierd-ui/dist/smoothnas-ui" ]; then
        echo "  tierd-ui/dist/smoothnas-ui not found, building frontend..."
        (cd "${PROJECT_DIR}/tierd-ui" && npm ci && npm run build) || {
            echo "ERROR: frontend build failed."
            exit 1
        }
    fi

    local payload_dir
    payload_dir="$(mktemp -d)"
    # Trap fires after `local` declared the variable but the shell expands
    # $payload_dir lazily, so any earlier exit from `set -u` (e.g. xorriso
    # FAILURE) hits the trap before assignment. Guard with `${var:-}`.
    trap 'rm -rf "${payload_dir:-}"' EXIT
    prepare_smoothnas_payload "$payload_dir"

    (
        cd "$SMOOTHISO_BUILD_DIR"
        SMOOTHNAS_PAYLOAD_DIR="$payload_dir" \
        INSTALLER_LANGUAGES="en:English nl:Nederlands" \
        INSTALLER_KERNEL_PACKAGES="" \
        INSTALLER_GPU_FIRMWARE_PKGS="${INSTALLER_GPU_FIRMWARE_PKGS:-}" \
        INSTALLER_GPU_KERNEL_MODULES="${INSTALLER_GPU_KERNEL_MODULES:-amdgpu radeon}" \
        ARCH="$DEB_ARCH" \
        PRODUCT_NAME="SmoothNAS" \
        PRODUCT_ID="smoothnas" \
        PRODUCT_HOSTNAME="smoothnas" \
        VG_NAME="smoothnas-vg" \
        DATA_DIR="/var/lib/tierd" \
        TLS_DIR="/etc/tierd/tls" \
        HOOKS_DIR="$HOOKS_DIR" \
        CACHE_DIR="$CACHE_DIR" \
        WORK_DIR="$WORK_DIR" \
        ISO_OUTPUT_FILE="$ISO_FILE" \
        VERSION="$VERSION" \
        DEBIAN_MIRROR="$DEBIAN_MIRROR" \
        BOOT_MENU_TITLE="SmoothNAS Install" \
        ISO_LABEL="SMOOTHNAS" \
        ./build-iso.sh
    )
}

main
