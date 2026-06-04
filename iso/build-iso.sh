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
# Required sibling repos (override with env vars if checked out elsewhere):
#   ../smoothiso        — generic Debian-installer ISO builder
#   ../smoothkernel     — SmoothKernel and OpenZFS .deb artifacts
#
# Override env vars: SMOOTHISO_DIR, SMOOTHKERNEL_DIR,
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
ISO_FILE="${OUTPUT_DIR}/smoothnas-${VERSION}-${DEB_ARCH}.iso"
HOOKS_DIR="${SCRIPT_DIR}/hooks"
SMOOTHISO_DIR="${SMOOTHISO_DIR:-${PROJECT_DIR}/../smoothiso}"
SMOOTHISO_PATCH_DIR="${SCRIPT_DIR}/smoothiso-patches"
# Where to find prebuilt SmoothKernel + OpenZFS .debs. The CI release
# workflow downloads them from a pinned RakuenSoftware/smoothkernel
# GitHub release; operators can point this at a local out/ directory
# from `make kernel && make zfs` in a sibling smoothkernel checkout.
DEFAULT_SMOOTHKERNEL_DIR="${PROJECT_DIR}/../smoothkernel/out"
SMOOTHKERNEL_DIR="${SMOOTHKERNEL_DIR:-$DEFAULT_SMOOTHKERNEL_DIR}"
ZFS_ARTIFACT_DIR="${ZFS_ARTIFACT_DIR:-$SMOOTHKERNEL_DIR}"
# GitHub repo that publishes SmoothKernel releases. Used to construct the
# per-deb download URLs embedded in the installer payload manifest, so
# packages.sh can fetch them at install time instead of bundling them in the
# initrd (which would bloat it past GRUB's heap limit).
SMOOTHKERNEL_GITHUB_REPO="${SMOOTHKERNEL_GITHUB_REPO:-RakuenSoftware/smoothkernel}"
SMOOTHFS_REPO_URL="${SMOOTHFS_REPO_URL:-git@github.com:RakuenSoftware/smoothfs.git}"
SMOOTHFS_REPO_REF="${SMOOTHFS_REPO_REF:-b73f510b4f96ab0249ecb107e6e62e0d7a7c2fc4}"
SMOOTHFS_SRC_DIR="${SMOOTHFS_SRC_DIR:-}"
# Debian architecture for the produced ISO. SmoothKernel publishes per-arch
# artifacts in a single GitHub release; pick the matching set here.
DEB_ARCH="${DEB_ARCH:-amd64}"
case "$DEB_ARCH" in
    amd64|arm64) ;;
    *) echo "ERROR: unsupported DEB_ARCH '${DEB_ARCH}' (expected amd64 or arm64)" >&2; exit 1 ;;
esac
SMOOTHFS_FETCH_DIR="${CACHE_DIR}/smoothfs-src"
SMOOTHFS_SOURCE_DIR=""
SMOOTHISO_BUILD_DIR=""

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
    mkdir -p "$WORK_DIR"
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

resolve_appliance_artifacts() {
    KERNEL_IMAGE_DEB=$(pick_artifact "${SMOOTHKERNEL_DIR}/linux-image-*-smoothkernel_*_${DEB_ARCH}.deb" "-dbg_") || {
        echo "ERROR: SmoothKernel image package for ${DEB_ARCH} not found under ${SMOOTHKERNEL_DIR}."
        exit 1
    }
    KERNEL_HEADERS_DEB=$(pick_artifact "${SMOOTHKERNEL_DIR}/linux-headers-*-smoothkernel_*_${DEB_ARCH}.deb") || {
        echo "ERROR: SmoothKernel headers package for ${DEB_ARCH} not found under ${SMOOTHKERNEL_DIR}."
        exit 1
    }
    KERNEL_LIBC_DEB=$(pick_artifact "${SMOOTHKERNEL_DIR}/linux-libc-dev_*_${DEB_ARCH}.deb") || {
        echo "ERROR: SmoothKernel linux-libc-dev package for ${DEB_ARCH} not found under ${SMOOTHKERNEL_DIR}."
        exit 1
    }

    ZFS_PACKAGE_FILES=()
    local pkg pattern_suffix
    # All OpenZFS .debs are filename-tagged with the arch in the
    # SmoothKernel release, even arch-all ones (zfs-dkms, zfs-initramfs);
    # we just pick the per-arch filename for each.
    for entry in \
        "libnvpair3_*:${DEB_ARCH}" \
        "libuutil3_*:${DEB_ARCH}" \
        "libzfs7_*:${DEB_ARCH}" \
        "libzpool7_*:${DEB_ARCH}" \
        "zfs_*:${DEB_ARCH}" \
        "zfs-dkms_*:${DEB_ARCH}" \
        "zfs-initramfs_*:${DEB_ARCH}"; do
        local prefix="${entry%:*}"
        pattern_suffix="${entry##*:}"
        pkg=$(pick_artifact "${ZFS_ARTIFACT_DIR}/${prefix}_${pattern_suffix}.deb") || {
            echo "ERROR: Required OpenZFS artifact ${prefix}_${pattern_suffix}.deb not found under ${ZFS_ARTIFACT_DIR}."
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


prepare_smoothnas_payload() {
    local payload_dir="$1"
    local base_dir="${PROJECT_DIR}"

    mkdir -p "$payload_dir"

    cp "${base_dir}/bin/tierd" "$payload_dir/tierd" || {
        echo "ERROR: bin/tierd not found at ${base_dir}/bin/tierd."
        exit 1
    }
    cp "${base_dir}/bin/docker-lxc-daemon" "$payload_dir/docker-lxc-daemon" || {
        echo "ERROR: bin/docker-lxc-daemon not found at ${base_dir}/bin/docker-lxc-daemon."
        echo "       Run 'make build-runtime' before building the ISO."
        exit 1
    }
    cp "${base_dir}/runtime/smoothnas-runtime.service" "$payload_dir/smoothnas-runtime.service"
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

    local smoothkernel_tag
    smoothkernel_tag=$(tr -d '[:space:]' < "$SCRIPT_DIR/smoothkernel-version")
    local smoothkernel_base_url="https://github.com/${SMOOTHKERNEL_GITHUB_REPO}/releases/download/${smoothkernel_tag}"

    local zfs_filenames=()
    local zfs_package_names=()
    local pkg_name=""
    for pkg_name in "${ZFS_PACKAGE_FILES[@]}"; do
        zfs_filenames+=("$(basename "$pkg_name")")
        zfs_package_names+=("$(dpkg-deb -f "$pkg_name" Package)")
    done
    cat > "${payload_dir}/package-manifest" <<EOF
SMOOTHNAS_DEB_ARCH=${DEB_ARCH}
SMOOTHKERNEL_IMAGE_PACKAGE=$(dpkg-deb -f "$KERNEL_IMAGE_DEB" Package)
SMOOTHKERNEL_HEADERS_PACKAGE=$(dpkg-deb -f "$KERNEL_HEADERS_DEB" Package)
SMOOTHKERNEL_LIBC_PACKAGE=$(dpkg-deb -f "$KERNEL_LIBC_DEB" Package)
SMOOTHKERNEL_ZFS_PACKAGES="${zfs_package_names[*]}"
SMOOTHKERNEL_RELEASE_BASE_URL=${smoothkernel_base_url}
SMOOTHKERNEL_IMAGE_FILENAME=$(basename "$KERNEL_IMAGE_DEB")
SMOOTHKERNEL_HEADERS_FILENAME=$(basename "$KERNEL_HEADERS_DEB")
SMOOTHKERNEL_LIBC_FILENAME=$(basename "$KERNEL_LIBC_DEB")
SMOOTHKERNEL_ZFS_FILENAMES="${zfs_filenames[*]}"
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
