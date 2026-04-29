#!/bin/bash
# Build a SmoothNAS installer ISO based on Debian 13 (Trixie).
#
# Usage: ./iso/build-iso.sh <version>
#
# Wraps the generic smoothiso builder with SmoothNAS-specific config and
# hooks. The installer presents the SmoothGUI installer (firefox-esr in
# kiosk mode) on the boot console; smoothiso/installer.sh drives the flow
# and sources the project hooks under iso/hooks/.
#
# Required sibling repos (override with env vars if checked out elsewhere):
#   ../smoothiso        — generic Debian-installer ISO builder
#   ../smoothgui        — React installer frontend
#   ../smoothkernel     — SmoothKernel and OpenZFS .deb artifacts
#
# Override env vars: SMOOTHISO_DIR, SMOOTHGUI_DIR, SMOOTHKERNEL_DIR,
# ZFS_ARTIFACT_DIR, SMOOTHFS_REPO_URL, SMOOTHFS_REPO_REF, SMOOTHFS_SRC_DIR,
# DEBIAN_MIRROR.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
VERSION="${1:?Usage: ./iso/build-iso.sh <version>}"

DEBIAN_MIRROR="${DEBIAN_MIRROR:-http://deb.debian.org/debian}"
CACHE_DIR="${PROJECT_DIR}/iso/cache"
WORK_DIR="${PROJECT_DIR}/iso/work"
OUTPUT_DIR="${PROJECT_DIR}/iso/output"
ISO_FILE="${OUTPUT_DIR}/smoothnas-${VERSION}.iso"
HOOKS_DIR="${SCRIPT_DIR}/hooks"
SMOOTHISO_DIR="${SMOOTHISO_DIR:-${PROJECT_DIR}/../smoothiso}"
SMOOTHGUI_DIR="${SMOOTHGUI_DIR:-${PROJECT_DIR}/../smoothgui}"
SMOOTHGUI_FRONTEND_DIR="${SMOOTHGUI_FRONTEND_DIR:-${SMOOTHGUI_DIR}/dist/installer}"
SMOOTHGUI_FRONTEND_REQUIRED="${SMOOTHGUI_FRONTEND_REQUIRED:-1}"
SMOOTHGUI_FRONTEND_PORT="${SMOOTHGUI_FRONTEND_PORT:-8080}"
SMOOTHGUI_FRONTEND_BIND="${SMOOTHGUI_FRONTEND_BIND:-127.0.0.1}"
DEFAULT_SMOOTHKERNEL_DIR="${PROJECT_DIR}/../smoothkernel/out-smoothnas"
if [ ! -d "$DEFAULT_SMOOTHKERNEL_DIR" ]; then
    DEFAULT_SMOOTHKERNEL_DIR="/home/virant/kbuild"
fi
SMOOTHKERNEL_DIR="${SMOOTHKERNEL_DIR:-$DEFAULT_SMOOTHKERNEL_DIR}"
DEFAULT_ZFS_ARTIFACT_DIR="$SMOOTHKERNEL_DIR"
if ! compgen -G "${DEFAULT_ZFS_ARTIFACT_DIR}/zfs_*.deb" >/dev/null; then
    DEFAULT_ZFS_ARTIFACT_DIR="${SMOOTHKERNEL_DIR}/zfs-2.4.1"
fi
ZFS_ARTIFACT_DIR="${ZFS_ARTIFACT_DIR:-$DEFAULT_ZFS_ARTIFACT_DIR}"
SMOOTHFS_REPO_URL="${SMOOTHFS_REPO_URL:-git@github.com:RakuenSoftware/smoothfs.git}"
SMOOTHFS_REPO_REF="${SMOOTHFS_REPO_REF:-6382817fd1a561b6d4c39421d954a65f27a18087}"
SMOOTHFS_SRC_DIR="${SMOOTHFS_SRC_DIR:-}"
SMOOTHFS_FETCH_DIR="${CACHE_DIR}/smoothfs-src"
SMOOTHFS_SOURCE_DIR=""

KERNEL_IMAGE_DEB=""
KERNEL_HEADERS_DEB=""
KERNEL_LIBC_DEB=""
SMOOTHFS_VERSION=""
ZFS_PACKAGE_FILES=()

pick_artifact() {
    local pattern="$1"
    local exclude="${2:-}"
    local -a matches=()

    shopt -s nullglob
    for path in $pattern; do
        if [ -n "$exclude" ] && [[ "$path" == *"$exclude"* ]]; then
            continue
        fi
        matches+=("$path")
    done
    shopt -u nullglob

    if [ ${#matches[@]} -eq 0 ]; then
        return 1
    fi

    printf '%s\n' "${matches[$((${#matches[@]} - 1))]}"
}

prepare_smoothfs_source() {
    if [ -n "${SMOOTHFS_SRC_DIR}" ]; then
        if [ ! -f "${SMOOTHFS_SRC_DIR}/dkms.conf" ]; then
            echo "ERROR: smoothfs source tree not found at ${SMOOTHFS_SRC_DIR}."
            exit 1
        fi
        SMOOTHFS_SOURCE_DIR="${SMOOTHFS_SRC_DIR}"
        return
    fi

    echo "Fetching smoothfs source from ${SMOOTHFS_REPO_URL} @ ${SMOOTHFS_REPO_REF}..."
    rm -rf "${SMOOTHFS_FETCH_DIR}"
    git clone "${SMOOTHFS_REPO_URL}" "${SMOOTHFS_FETCH_DIR}" >/dev/null 2>&1 || {
        echo "ERROR: Failed to clone smoothfs repo ${SMOOTHFS_REPO_URL}."
        exit 1
    }
    (
        cd "${SMOOTHFS_FETCH_DIR}"
        git checkout --quiet "${SMOOTHFS_REPO_REF}"
    ) || {
        echo "ERROR: Failed to checkout smoothfs ref ${SMOOTHFS_REPO_REF}."
        exit 1
    }
    if [ ! -f "${SMOOTHFS_FETCH_DIR}/src/smoothfs/dkms.conf" ]; then
        echo "ERROR: smoothfs repo checkout is missing src/smoothfs/dkms.conf."
        exit 1
    fi
    SMOOTHFS_SOURCE_DIR="${SMOOTHFS_FETCH_DIR}/src/smoothfs"
}

resolve_appliance_artifacts() {
    KERNEL_IMAGE_DEB=$(pick_artifact "${SMOOTHKERNEL_DIR}/linux-image-*smoothnas_*.deb" "-dbg_") || {
        echo "ERROR: SmoothKernel image package not found under ${SMOOTHKERNEL_DIR}."
        exit 1
    }
    KERNEL_HEADERS_DEB=$(pick_artifact "${SMOOTHKERNEL_DIR}/linux-headers-*smoothnas_*.deb") || {
        echo "ERROR: SmoothKernel headers package not found under ${SMOOTHKERNEL_DIR}."
        exit 1
    }
    KERNEL_LIBC_DEB=$(pick_artifact "${SMOOTHKERNEL_DIR}/linux-libc-dev_*.deb") || {
        echo "ERROR: SmoothKernel linux-libc-dev package not found under ${SMOOTHKERNEL_DIR}."
        exit 1
    }

    case "$(basename "$KERNEL_IMAGE_DEB") $(basename "$KERNEL_HEADERS_DEB")" in
        *smoothnas-lts*)
            echo "ERROR: Refusing to use smoothnas-lts kernel artifacts."
            exit 1
            ;;
    esac

    ZFS_PACKAGE_FILES=()
    local pkg
    for pattern in \
        "libnvpair3_*.deb" \
        "libuutil3_*.deb" \
        "libzfs7_*.deb" \
        "libzpool7_*.deb" \
        "zfs_*.deb" \
        "zfs-dkms_*.deb" \
        "zfs-initramfs_*.deb"; do
        pkg=$(pick_artifact "${ZFS_ARTIFACT_DIR}/${pattern}") || {
            echo "ERROR: Required OpenZFS artifact ${pattern} not found under ${ZFS_ARTIFACT_DIR}."
            exit 1
        }
        ZFS_PACKAGE_FILES+=("$pkg")
    done

    prepare_smoothfs_source
    SMOOTHFS_VERSION=$(sed -n 's/^PACKAGE_VERSION="\([^"]*\)"$/\1/p' "${SMOOTHFS_SOURCE_DIR}/dkms.conf" | head -1)
    if [ -z "$SMOOTHFS_VERSION" ]; then
        echo "ERROR: Unable to determine smoothfs PACKAGE_VERSION from ${SMOOTHFS_SOURCE_DIR}/dkms.conf."
        exit 1
    fi
}

build_smoothgui_frontend() {
    if [ -d "$SMOOTHGUI_FRONTEND_DIR" ] && \
        { [ -f "${SMOOTHGUI_FRONTEND_DIR}/index.html" ] || [ -f "${SMOOTHGUI_FRONTEND_DIR}/index.installer.html" ]; }; then
        return 0
    fi

    if [ ! -d "$SMOOTHGUI_DIR" ]; then
        echo "ERROR: SmoothGUI source tree not found at ${SMOOTHGUI_DIR}."
        exit 1
    fi

    echo "Building SmoothGUI installer frontend..."
    (cd "$SMOOTHGUI_DIR" && npm ci && npm run build:installer) || {
        echo "ERROR: failed to build smoothgui installer frontend."
        exit 1
    }

    SMOOTHGUI_FRONTEND_DIR="${SMOOTHGUI_DIR}/dist/installer"
    if [ ! -d "$SMOOTHGUI_FRONTEND_DIR" ] || \
        { [ ! -f "${SMOOTHGUI_FRONTEND_DIR}/index.html" ] && [ ! -f "${SMOOTHGUI_FRONTEND_DIR}/index.installer.html" ]; }; then
        echo "ERROR: SmoothGUI installer frontend build output missing at ${SMOOTHGUI_FRONTEND_DIR}."
        exit 1
    fi
}

prepare_smoothnas_payload() {
    local payload_dir="$1"
    local base_dir="${PROJECT_DIR}"

    mkdir -p "$payload_dir"

    cp "${base_dir}/bin/tierd" "$payload_dir/tierd" || {
        echo "ERROR: bin/tierd not found at ${base_dir}/bin/tierd."
        exit 1
    }
    if [ -d "${base_dir}/tierd-ui/dist/smoothnas-ui" ]; then
        mkdir -p "${payload_dir}/tierd-ui"
        cp -r "${base_dir}/tierd-ui/dist/smoothnas-ui/." "${payload_dir}/tierd-ui/"
    else
        echo "ERROR: tierd-ui/dist/smoothnas-ui not found at ${base_dir}/tierd-ui/dist/smoothnas-ui."
        exit 1
    fi

    cp "$SCRIPT_DIR/90-smoothnas-net.conf" "$payload_dir/90-smoothnas-net.conf"

    mkdir -p "$payload_dir/tests"
    for test_script in \
        "${base_dir}/scripts/smoothfs-protocol-gate.sh" \
        "${base_dir}/scripts/smoothfs-mixed-protocol-soak.sh" \
        "${base_dir}/scripts/smoothfs-windows-smb-soak.ps1"; do
        [ -f "$test_script" ] && cp "$test_script" "$payload_dir/tests/"
    done

    cp -a "$SMOOTHFS_SOURCE_DIR" "${payload_dir}/smoothfs-src"

    mkdir -p "${payload_dir}/repo/pool"
    cp "$KERNEL_IMAGE_DEB" "$KERNEL_HEADERS_DEB" "$KERNEL_LIBC_DEB" "$payload_dir/repo/pool/"
    for pkg in "${ZFS_PACKAGE_FILES[@]}"; do
        cp "$pkg" "${payload_dir}/repo/pool/"
    done
    (
        cd "${payload_dir}/repo"
        dpkg-scanpackages pool /dev/null > Packages
        gzip -9c Packages > Packages.gz
    )

    local zfs_package_names=()
    local pkg_name=""
    for pkg_name in "${ZFS_PACKAGE_FILES[@]}"; do
        zfs_package_names+=("$(dpkg-deb -f "$pkg_name" Package)")
    done
    cat > "${payload_dir}/package-manifest" <<EOF
SMOOTHKERNEL_IMAGE_PACKAGE=$(dpkg-deb -f "$KERNEL_IMAGE_DEB" Package)
SMOOTHKERNEL_HEADERS_PACKAGE=$(dpkg-deb -f "$KERNEL_HEADERS_DEB" Package)
SMOOTHKERNEL_LIBC_PACKAGE=$(dpkg-deb -f "$KERNEL_LIBC_DEB" Package)
SMOOTHKERNEL_ZFS_PACKAGES="${zfs_package_names[*]}"
SMOOTHFS_VERSION=${SMOOTHFS_VERSION}
SMOOTHFS_REPO_URL=${SMOOTHFS_REPO_URL}
SMOOTHFS_REPO_REF=${SMOOTHFS_REPO_REF}
EOF
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

    resolve_appliance_artifacts
    build_smoothgui_frontend

    if [ ! -f "${PROJECT_DIR}/bin/tierd" ]; then
        echo "  bin/tierd not found, building backend..."
        (cd "${PROJECT_DIR}/tierd" && CGO_ENABLED=1 go build -o ../bin/tierd ./cmd/tierd/) || {
            echo "ERROR: backend build failed."
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
        cd "$SMOOTHISO_DIR"
        SMOOTHGUI_FRONTEND_DIR="$SMOOTHGUI_FRONTEND_DIR" \
        SMOOTHGUI_FRONTEND_REQUIRED="$SMOOTHGUI_FRONTEND_REQUIRED" \
        SMOOTHGUI_FRONTEND_PORT="$SMOOTHGUI_FRONTEND_PORT" \
        SMOOTHGUI_FRONTEND_BIND="$SMOOTHGUI_FRONTEND_BIND" \
        SMOOTHGUI_BROWSER_USER='smoothinstaller' \
        SMOOTHGUI_BROWSER_UID="${SMOOTHGUI_BROWSER_UID:-1000}" \
        SMOOTHGUI_BROWSER_GID="${SMOOTHGUI_BROWSER_GID:-1000}" \
        SMOOTHGUI_REQUIRE_VISIBLE_DISPLAY="${SMOOTHGUI_REQUIRE_VISIBLE_DISPLAY:-1}" \
        SMOOTHGUI_ALLOW_XVFB="${SMOOTHGUI_ALLOW_XVFB:-0}" \
        SMOOTHGUI_XORG_STARTUP_TIMEOUT="${SMOOTHGUI_XORG_STARTUP_TIMEOUT:-12}" \
        SMOOTHNAS_PAYLOAD_DIR="$payload_dir" \
        INSTALLER_KERNEL_PACKAGES="" \
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
