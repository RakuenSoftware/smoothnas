#!/bin/sh
# SmoothNAS embed hook for smoothiso build-iso.sh.
# Stages the SmoothNAS payload (tierd, frontend, repo, smoothfs source,
# tests, manifest) into the installer initrd at /smoothnas/.
set -e

if [ -d "${SMOOTHNAS_PAYLOAD_DIR}" ]; then
    cp -a "${SMOOTHNAS_PAYLOAD_DIR}/." "${INITRD_TMP}/smoothnas/"
fi

debian_mirror="${DEBIAN_MIRROR:-http://deb.debian.org/debian}"
debian_suite="${DEBIAN_SUITE:-trixie}"
debian_arch="${ARCH:-${DEB_ARCH:-amd64}}"
cache_dir="${CACHE_DIR:-/tmp/smoothnas-installer-cache}"

find_cached_deb() {
    pkg="$1"
    pkg_cache="$2"
    selected=""

    set -- "${pkg_cache}/${pkg}_"*.deb "${pkg_cache}/${pkg}-"*.deb
    for deb in "$@"; do
        [ -f "$deb" ] && selected="$deb"
    done
    [ -n "$selected" ] && { printf '%s\n' "$selected"; return 0; }
    return 1
}

package_filename_from_index() {
    pkg="$1"
    component="$2"
    arch="$3"

    for index_arch in "$arch" all; do
        filename=$(curl -fsSL "${debian_mirror}/dists/${debian_suite}/${component}/binary-${index_arch}/Packages.gz" 2>/dev/null \
            | gzip -dc 2>/dev/null \
            | awk -v pkg="$pkg" '
                BEGIN { RS = ""; FS = "\n" }
                {
                    if (printed) {
                        next
                    }
                    found = 0
                    filename = ""
                    for (i = 1; i <= NF; i++) {
                        if ($i == "Package: " pkg) {
                            found = 1
                        } else if ($i ~ /^Filename: /) {
                            filename = substr($i, 11)
                        }
                    }
                    if (found && filename != "") {
                        print filename
                        printed = 1
                    }
                }
            ')
        [ -n "$filename" ] && { printf '%s\n' "$filename"; return 0; }
    done

    return 1
}

download_debian_package() {
    pkg="$1"
    component="$2"
    pkg_cache="${cache_dir}/$3"

    mkdir -p "$pkg_cache"
    if deb=$(find_cached_deb "$pkg" "$pkg_cache"); then
        printf '%s\n' "$deb"
        return 0
    fi

    filename=$(package_filename_from_index "$pkg" "$component" "$debian_arch" || true)
    if [ -z "$filename" ]; then
        echo "ERROR: unable to find ${pkg} in ${debian_suite}/${component} for ${debian_arch}" >&2
        return 1
    fi

    deb="${pkg_cache}/$(basename "$filename")"
    echo "  Fetching ${pkg} from ${debian_suite}/${component}..." >&2
    tmp_deb="${deb}.tmp"
    rm -f "$tmp_deb"
    curl -fsSL -o "$tmp_deb" "${debian_mirror}/${filename}"
    mv "$tmp_deb" "$deb"
    printf '%s\n' "$deb"
}

installer_kernel_version() {
    for modules_root in "${INITRD_TMP}/lib/modules" "${INITRD_TMP}/usr/lib/modules"; do
        [ -d "$modules_root" ] || continue
        kver=$(find "$modules_root" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null | head -1)
        [ -n "$kver" ] && { printf '%s\n' "$kver"; return 0; }
    done
    return 1
}

stage_debian_gpu_modules() {
    kver=$(installer_kernel_version || true)
    if [ -z "$kver" ]; then
        echo "ERROR: cannot determine installer kernel version for GPU module staging" >&2
        return 1
    fi
    case "$kver" in
        *+deb*) ;;
        *)
            echo "  Skipping Debian GPU module staging for non-Debian installer kernel ${kver}."
            return 0
            ;;
    esac

    kernel_pkg="linux-image-${kver}"
    kernel_deb=$(download_debian_package "$kernel_pkg" main installer-kernel-modules) || return 1
    kernel_tmp=$(mktemp -d)
    dpkg-deb -x "$kernel_deb" "$kernel_tmp"

    kernel_modules=""
    for candidate in "${kernel_tmp}/usr/lib/modules/${kver}" "${kernel_tmp}/lib/modules/${kver}"; do
        if [ -d "$candidate" ]; then
            kernel_modules="$candidate"
            break
        fi
    done
    if [ -z "$kernel_modules" ]; then
        echo "ERROR: ${kernel_pkg} does not contain modules for ${kver}" >&2
        rm -rf "$kernel_tmp"
        return 1
    fi

    initrd_modules="${INITRD_TMP}/lib/modules/${kver}"
    mkdir -p "$initrd_modules"
    for rel in kernel/drivers/gpu kernel/drivers/video kernel/drivers/iommu; do
        if [ -d "${kernel_modules}/${rel}" ]; then
            mkdir -p "${initrd_modules}/${rel}"
            cp -a --no-clobber "${kernel_modules}/${rel}/." "${initrd_modules}/${rel}/" 2>/dev/null || true
        fi
    done

    if ! find "$initrd_modules" -path '*/drivers/gpu/drm/amd/amdgpu/amdgpu.ko*' | grep -q .; then
        echo "ERROR: failed to stage amdgpu for installer kernel ${kver}" >&2
        rm -rf "$kernel_tmp"
        return 1
    fi

    echo "  Staged Debian installer GPU modules for ${kver}."
    rm -rf "$kernel_tmp"
}

stage_usrmerge_firmware() {
    [ -n "${INSTALLER_GPU_FIRMWARE_PKGS:-}" ] || return 0

    for pkg in $INSTALLER_GPU_FIRMWARE_PKGS; do
        firmware_deb=$(download_debian_package "$pkg" non-free-firmware gpu-firmware) || return 1
        firmware_tmp=$(mktemp -d)
        dpkg-deb -x "$firmware_deb" "$firmware_tmp"

        staged=0
        for firmware_dir in "${firmware_tmp}/lib/firmware" "${firmware_tmp}/usr/lib/firmware"; do
            [ -d "$firmware_dir" ] || continue
            mkdir -p "${INITRD_TMP}/lib/firmware"
            cp -a --no-clobber "${firmware_dir}/." "${INITRD_TMP}/lib/firmware/" 2>/dev/null || true
            staged=1
        done

        rm -rf "$firmware_tmp"
        if [ "$staged" != "1" ]; then
            echo "ERROR: ${pkg} did not contain lib/firmware or usr/lib/firmware" >&2
            return 1
        fi
        echo "  Staged firmware from ${pkg}."
    done
}

stage_usrmerge_firmware
stage_debian_gpu_modules

# simpledrm claims the UEFI GOP framebuffer on OVMF boot, preventing bochs-drm
# from getting DRM control and causing all Xorg startup attempts to fail.
find "${INITRD_TMP}/lib/modules" -name "simpledrm.ko*" -delete 2>/dev/null || true
