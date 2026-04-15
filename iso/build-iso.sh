#!/bin/bash
# Build a SmoothNAS installer ISO based on Debian 13 (Trixie) netinst.
#
# Usage: ./iso/build-iso.sh <version>
#
# LICENSING NOTE:
#   SmoothNAS does NOT bundle, distribute, or modify ZFS, Samba, or any
#   other third-party software. All packages are downloaded by the user's
#   machine directly from Debian's mirrors during installation.
#
# Prerequisites:
#   - xorriso, isolinux, syslinux-utils, cpio, gzip, file, curl
#
# The script:
#   1. Downloads the Debian 13 netinst ISO if not cached
#   2. Extracts the full ISO (preserving all boot structures)
#   3. Extracts network modules from the ISO's udeb pool
#   4. Appends our preseed, installer, and network modules to the initrd
#   5. Rewrites the boot menu
#   6. Repacks using the same xorriso flags Debian uses
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
VERSION="${1:?Usage: ./iso/build-iso.sh <version>}"

DEBIAN_ISO_URL="https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/"
CACHE_DIR="${PROJECT_DIR}/iso/cache"
WORK_DIR="${PROJECT_DIR}/iso/work"
OUTPUT_DIR="${PROJECT_DIR}/iso/output"
ISO_FILE="${OUTPUT_DIR}/smoothnas-${VERSION}.iso"

# --- Preflight ---

check_prereqs() {
    local missing=()
    for cmd in xorriso cpio gzip file curl; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo "ERROR: Missing tools: ${missing[*]}"
        exit 1
    fi
    if [ ! -f /usr/lib/ISOLINUX/isohdpfx.bin ]; then
        echo "ERROR: /usr/lib/ISOLINUX/isohdpfx.bin not found. Install isolinux."
        exit 1
    fi
}

check_artifacts() {
    if [ ! -f "${SCRIPT_DIR}/smoothnas-install" ]; then
        echo "ERROR: iso/smoothnas-install not found."
        exit 1
    fi
    if [ ! -f "${SCRIPT_DIR}/preseed.cfg" ]; then
        echo "ERROR: iso/preseed.cfg not found."
        exit 1
    fi

    local base_dir
    base_dir=$(dirname "$SCRIPT_DIR")

    if [ ! -f "${base_dir}/bin/tierd" ]; then
        echo "  bin/tierd not found, building backend..."
        (cd "${base_dir}/tierd" && CGO_ENABLED=1 go build -o ../bin/tierd ./cmd/tierd/) || \
            { echo "ERROR: backend build failed."; exit 1; }
    fi

    if [ ! -d "${base_dir}/tierd-ui/dist/smoothnas-ui" ]; then
        echo "  tierd-ui/dist/smoothnas-ui not found, building frontend..."
        (cd "${base_dir}/tierd-ui" && npm ci && npm run build) || \
            { echo "ERROR: frontend build failed."; exit 1; }
    fi
}

# --- Download ---

download_iso() {
    mkdir -p "$CACHE_DIR"
    local cached="${CACHE_DIR}/debian-netinst.iso"
    if [ -f "$cached" ]; then
        echo "Using cached: $cached" >&2
        echo "$cached"; return
    fi
    echo "Finding latest Debian 13 netinst..." >&2
    local name
    name=$(curl -sL "$DEBIAN_ISO_URL" | grep -oP 'href="debian-[0-9.]+-amd64-netinst\.iso"' | head -1 | tr -d '"' | sed 's/href=//')
    [ -z "$name" ] && { echo "ERROR: Cannot find netinst ISO" >&2; exit 1; }
    echo "Downloading $name..." >&2
    curl -fSL -o "$cached" "${DEBIAN_ISO_URL}${name}"
    echo "$cached"
}

# --- Extract only what we need from the ISO ---

extract_iso() {
    local src="$1"
    echo "Extracting boot files from ISO (skipping pool, gtk, xen)..."
    rm -rf "$WORK_DIR"
    mkdir -p "$WORK_DIR"

    # Extract only the directories we actually need:
    #   install.amd/  (vmlinuz + initrd.gz, kernel + base initrd)
    #   isolinux/     (BIOS boot)
    #   boot/grub/    (UEFI boot + efi.img)
    #   pool/         (temp, for nic-modules udeb extraction only)
    # Note: extract install.amd as a directory, not individual files.
    # Extracting files one at a time fails because xorriso creates the parent
    # directory with the ISO's read-only permissions, blocking the second file.
    local tmp_pool
    tmp_pool=$(mktemp -d)

    xorriso -osirrox on -indev "$src" \
        -extract /install.amd          "$WORK_DIR/install.amd" \
        -extract /isolinux              "$WORK_DIR/isolinux" \
        -extract /boot                  "$WORK_DIR/boot" \
        -extract /EFI                   "$WORK_DIR/EFI" \
        -extract /pool                  "$tmp_pool/pool" \
        2>&1

    chmod -R u+w "$WORK_DIR"
    chmod -R u+w "$tmp_pool"

    # Move pool to a temp location; setup_initrd will use it, then we discard.
    POOL_DIR="$tmp_pool/pool"
}

# --- Append installer files and network modules to the stock initrd ---

setup_initrd() {
    # Keep the stock netinst initrd (it has storage modules but not network).
    # Append a cpio layer with: network modules from the ISO's udeb pool,
    # our preseed, and the installer script.
    echo "Injecting network modules + preseed + installer into initrd..."
    local tmp
    tmp=$(mktemp -d)

    # Extract hardware modules from udebs in the pool.
    # The stock d-i initrd is minimal -- it includes neither storage drivers
    # (SATA/SCSI/NVMe) nor NIC drivers (virtio_net, e1000, etc.). Those are
    # loaded via udebs during the normal installer flow, but our custom
    # installer bypasses that. We must inject them here.
    local udeb_patterns=(
        'scsi-core-modules-*-amd64-di_*.udeb'
        'scsi-modules-*-amd64-di_*.udeb'
        'sata-modules-*-amd64-di_*.udeb'
        'nic-modules-*-amd64-di_*.udeb'
        'nic-shared-modules-*-amd64-di_*.udeb'
        'pata-modules-*-amd64-di_*.udeb'
        'md-modules-*-amd64-di_*.udeb'
        'multipath-modules-*-amd64-di_*.udeb'
        'ext4-modules-*-amd64-di_*.udeb'
        'dm-modules-*-amd64-di_*.udeb'
        'usb-storage-modules-*-amd64-di_*.udeb'
    )

    for pattern in "${udeb_patterns[@]}"; do
        local udeb
        udeb=$(find "${POOL_DIR}" -name "$pattern" | head -1)
        if [ -n "$udeb" ]; then
            echo "  Extracting modules from $(basename "$udeb")..."
            local udeb_tmp
            udeb_tmp=$(mktemp -d)
            dpkg-deb -x "$udeb" "$udeb_tmp"

            # Udebs use lib/modules/ but the initrd uses usr/lib/modules/.
            # Merge the module tree so all drivers are available.
            local kver
            kver=$(find "$udeb_tmp/lib/modules" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null | head -1)
            if [ -n "$kver" ]; then
                # Use cp --archive --no-clobber to merge without overwriting.
                local dest="${tmp}/usr/lib/modules/${kver}"
                mkdir -p "$dest"
                cp -a --no-clobber -r "$udeb_tmp/lib/modules/${kver}/." "$dest/" 2>/dev/null || true
            fi

            rm -rf "$udeb_tmp"
        fi
    done

    # The stock d-i initrd has libnewt and libslang but not whiptail or libpopt.
    # Extract both from the pool so the installer can show interactive dialogs.
    local whiptail_deb
    whiptail_deb=$(find "${POOL_DIR}" -name 'whiptail_*.deb' | head -1)
    if [ -n "$whiptail_deb" ]; then
        echo "  Extracting whiptail from $(basename "$whiptail_deb")..."
        local wt_tmp
        wt_tmp=$(mktemp -d)
        dpkg-deb -x "$whiptail_deb" "$wt_tmp"
        mkdir -p "${tmp}/usr/bin"
        cp "$wt_tmp/usr/bin/whiptail" "${tmp}/usr/bin/"
        rm -rf "$wt_tmp"
    else
        echo "  WARNING: whiptail deb not found, falling back to text prompts"
    fi

    local popt_deb
    popt_deb=$(find "${POOL_DIR}" -name 'libpopt0_*.deb' -o -name 'libpopt0t64_*.deb' | head -1)
    if [ -z "$popt_deb" ]; then
        # Not in the netinst pool; download it.
        echo "  Downloading libpopt0t64..."
        local popt_url="${DEBIAN_MIRROR}/pool/main/p/popt/"
        local popt_name
        popt_name=$(curl -sL "$popt_url" | grep -oP 'href="libpopt0t64_[^"]*_amd64\.deb"' | tail -1 | tr -d '"' | sed 's/href=//')
        if [ -n "$popt_name" ]; then
            popt_deb="/tmp/libpopt.deb"
            curl -fsSL -o "$popt_deb" "${popt_url}${popt_name}"
        fi
    fi
    if [ -n "$popt_deb" ] && [ -f "$popt_deb" ]; then
        echo "  Extracting libpopt from $(basename "$popt_deb")..."
        local popt_tmp
        popt_tmp=$(mktemp -d)
        dpkg-deb -x "$popt_deb" "$popt_tmp"
        # Copy the .so files into the initrd's lib path.
        local libdir="${tmp}/usr/lib/x86_64-linux-gnu"
        mkdir -p "$libdir"
        find "$popt_tmp" -name 'libpopt.so*' -exec cp {} "$libdir/" \;
        rm -rf "$popt_tmp"
    else
        echo "  WARNING: libpopt not found, whiptail may not work"
    fi

    # Extract partitioning and filesystem tools from the ISO pool.
    # The d-i initrd has none of these; the installer needs them all.
    echo "  Extracting partitioning tools from pool..."
    local pkg_tmp
    pkg_tmp=$(mktemp -d)

    # Helper: extract a deb/udeb into pkg_tmp, then copy binaries/libs to $tmp.
    extract_bins() {
        local deb="$1"; shift
        dpkg-deb -x "$deb" "$pkg_tmp/pkg"
        for bin in "$@"; do
            local src=$(find "$pkg_tmp/pkg" -name "$bin" -type f | head -1)
            if [ -n "$src" ]; then
                local dest="${tmp}/usr/sbin/${bin}"
                mkdir -p "${tmp}/usr/sbin"
                cp "$src" "$dest"
                chmod +x "$dest"
            fi
        done
        # Copy any shared libraries.
        find "$pkg_tmp/pkg" -name '*.so*' -type f | while read -r lib; do
            local name=$(basename "$lib")
            mkdir -p "${tmp}/usr/lib/x86_64-linux-gnu"
            cp "$lib" "${tmp}/usr/lib/x86_64-linux-gnu/"
        done
        # Copy symlinks for shared libraries and tool aliases.
        if [ -d "$pkg_tmp/pkg/usr/sbin" ]; then
            find "$pkg_tmp/pkg/usr/sbin" -type l | while read -r link; do
                local name=$(basename "$link")
                local target=$(readlink "$link")
                mkdir -p "${tmp}/usr/sbin"
                ln -sf "$target" "${tmp}/usr/sbin/${name}"
            done || true
        fi
        find "$pkg_tmp/pkg" -path '*/lib/*' -type l 2>/dev/null | while read -r link; do
            local name=$(basename "$link")
            local target=$(readlink "$link")
            mkdir -p "${tmp}/usr/lib/x86_64-linux-gnu"
            ln -sf "$target" "${tmp}/usr/lib/x86_64-linux-gnu/${name}"
        done || true
        rm -rf "$pkg_tmp/pkg"
    }

    # LVM2 (pvcreate, vgcreate, lvcreate, etc. are symlinks to lvm).
    local lvm_udeb=$(find "${POOL_DIR}" -name 'lvm2-udeb_*.udeb' | head -1)
    [ -n "$lvm_udeb" ] && extract_bins "$lvm_udeb" lvm

    # LVM library dependency: libaio.
    local libaio_udeb=$(find "${POOL_DIR}" -name 'libaio1-udeb_*.udeb' | head -1)
    [ -n "$libaio_udeb" ] && extract_bins "$libaio_udeb"

    # LVM library dependency: libdevmapper.
    local dm_udeb=$(find "${POOL_DIR}" -name 'libdevmapper*-udeb_*.udeb' | head -1)
    [ -n "$dm_udeb" ] && extract_bins "$dm_udeb"

    # mdadm (RAID).
    local mdadm_udeb=$(find "${POOL_DIR}" -name 'mdadm-udeb_*.udeb' | head -1)
    [ -n "$mdadm_udeb" ] && extract_bins "$mdadm_udeb" mdadm

    # e2fsprogs (mke2fs, mkfs.ext4 symlink).
    local e2fs_udeb=$(find "${POOL_DIR}" -name 'e2fsprogs-udeb_*.udeb' | head -1)
    [ -n "$e2fs_udeb" ] && extract_bins "$e2fs_udeb" mke2fs

    # e2fsprogs library dependencies: libext2fs (also ships libe2p), libcom_err.
    local libext2_deb=$(find "${POOL_DIR}" -name 'libext2fs2t64_*.deb' | head -1)
    [ -n "$libext2_deb" ] && extract_bins "$libext2_deb"
    local libcomerr_deb=$(find "${POOL_DIR}" -name 'libcom-err2_*.deb' | head -1)
    [ -n "$libcomerr_deb" ] && extract_bins "$libcomerr_deb"

    # dosfstools (mkfs.fat; add mkfs.vfat symlink).
    local fat_udeb=$(find "${POOL_DIR}" -name 'dosfstools-udeb_*.udeb' | head -1)
    [ -n "$fat_udeb" ] && extract_bins "$fat_udeb" mkfs.fat
    mkdir -p "${tmp}/usr/sbin"
    ln -sf mkfs.fat "${tmp}/usr/sbin/mkfs.vfat"

    # wipefs (from full util-linux deb, not in the udeb).
    local utillinux_deb=$(find "${POOL_DIR}" -name 'util-linux_*.deb' | head -1)
    if [ -n "$utillinux_deb" ]; then
        dpkg-deb -x "$utillinux_deb" "$pkg_tmp/pkg"
        [ -f "$pkg_tmp/pkg/usr/sbin/wipefs" ] && cp "$pkg_tmp/pkg/usr/sbin/wipefs" "${tmp}/usr/sbin/" && chmod +x "${tmp}/usr/sbin/wipefs"
        rm -rf "$pkg_tmp/pkg"
    fi

    # gdisk library dependency: libstdc++.
    local libstdcpp_deb=$(find "${POOL_DIR}" -name 'libstdc++6_*.deb' | head -1)
    [ -n "$libstdcpp_deb" ] && extract_bins "$libstdcpp_deb"

    # gdisk (provides sgdisk). Not on the netinst ISO; download from mirror.
    local gdisk_deb="${CACHE_DIR}/gdisk.deb"
    if [ ! -f "$gdisk_deb" ]; then
        echo "  Downloading gdisk..."
        local gdisk_url="http://deb.debian.org/debian/pool/main/g/gdisk/"
        local gdisk_name
        gdisk_name=$(curl -sL "$gdisk_url" | grep -oP 'href="gdisk_[^"]*_amd64\.deb"' | tail -1 | tr -d '"' | sed 's/href=//')
        if [ -n "$gdisk_name" ]; then
            curl -fsSL -o "$gdisk_deb" "${gdisk_url}${gdisk_name}"
        fi
    fi
    if [ -f "$gdisk_deb" ]; then
        extract_bins "$gdisk_deb" sgdisk
    else
        echo "  WARNING: gdisk not found, sgdisk will not be available"
    fi

    # whiptail (interactive dialogs). The d-i initrd has libnewt and libslang
    # but not whiptail itself or libpopt.
    local whiptail_deb=$(find "${POOL_DIR}" -name 'whiptail_*.deb' | head -1)
    if [ -n "$whiptail_deb" ]; then
        dpkg-deb -x "$whiptail_deb" "$pkg_tmp/pkg"
        mkdir -p "${tmp}/usr/bin"
        cp "$pkg_tmp/pkg/usr/bin/whiptail" "${tmp}/usr/bin/"
        chmod +x "${tmp}/usr/bin/whiptail"
        rm -rf "$pkg_tmp/pkg"
    else
        echo "  WARNING: whiptail not found, falling back to text prompts"
    fi
    local popt_udeb=$(find "${POOL_DIR}" -name 'libpopt0-udeb_*.udeb' -o -name 'libpopt0_*.deb' | head -1)
    [ -n "$popt_udeb" ] && extract_bins "$popt_udeb"

    rm -rf "$pkg_tmp"

    # Bundle debootstrap from the ISO's udeb (avoids downloading at install time).
    local dbs_udeb
    dbs_udeb=$(find "${POOL_DIR}" -name 'debootstrap-udeb_*_all.udeb' | head -1)
    if [ -n "$dbs_udeb" ]; then
        echo "  Bundling debootstrap from $(basename "$dbs_udeb")..."
        dpkg-deb -x "$dbs_udeb" "$tmp"
    fi

    # Compile pkgdetails (required by debootstrap, normally provided by perl
    # or base-installer). Download source and compile statically.
    echo "  Building pkgdetails..."
    local pkgdetails_src="/tmp/pkgdetails.c"
    curl -fsSL "https://salsa.debian.org/installer-team/base-installer/-/raw/master/pkgdetails.c" -o "$pkgdetails_src"
    mkdir -p "${tmp}/usr/lib/debootstrap"
    if gcc -static -o "${tmp}/usr/lib/debootstrap/pkgdetails" "$pkgdetails_src"; then
        echo "  pkgdetails compiled successfully"
    else
        echo "  WARNING: Failed to compile pkgdetails -- debootstrap will need perl"
    fi
    rm -f "$pkgdetails_src"

    cp "${SCRIPT_DIR}/preseed.cfg" "${tmp}/preseed.cfg"
    cp "${SCRIPT_DIR}/smoothnas-install" "${tmp}/smoothnas-install"
    chmod +x "${tmp}/smoothnas-install"
    cp "${SCRIPT_DIR}/firstboot.sh" "${tmp}/smoothnas-firstboot"
    chmod +x "${tmp}/smoothnas-firstboot"

    # Embed pre-built tierd binary and frontend if available.
    # CI builds these before running build-iso.sh; local builds
    # should run 'make build' first.
    local base_dir
    base_dir=$(dirname "$SCRIPT_DIR")
    if [ -f "${base_dir}/bin/tierd" ]; then
        echo "  Embedding tierd binary..."
        mkdir -p "${tmp}/smoothnas"
        cp "${base_dir}/bin/tierd" "${tmp}/smoothnas/tierd"
        chmod +x "${tmp}/smoothnas/tierd"
    else
        echo "  WARNING: bin/tierd not found, run 'make build-backend' first"
    fi
    if [ -d "${base_dir}/tierd-ui/dist/smoothnas-ui" ]; then
        echo "  Embedding tierd-ui frontend..."
        mkdir -p "${tmp}/smoothnas/tierd-ui"
        cp -r "${base_dir}/tierd-ui/dist/smoothnas-ui/"* "${tmp}/smoothnas/tierd-ui/"
    else
        echo "  WARNING: tierd-ui/dist/smoothnas-ui not found, run 'make build-frontend' first"
    fi

    # Merge our files into the stock initrd as a single cpio archive.
    # Appending a second cpio layer (even within the same gzip stream)
    # is unreliable: the d-i's initramfs only extracts the first archive.
    # Instead, extract the stock initrd, overlay our files, and repack.
    local initrd="${WORK_DIR}/install.amd/initrd.gz"
    local initrd_root
    initrd_root=$(mktemp -d)
    (cd "$initrd_root" && zcat "$initrd" | cpio -id --quiet 2>/dev/null || true)
    cp -a "$tmp"/. "$initrd_root"/
    (cd "$initrd_root" && find . | cpio -o -H newc --quiet 2>/dev/null | gzip) > "$initrd"
    rm -rf "$initrd_root"
    rm -rf "$tmp"

    # Pool was only needed for nic-modules extraction; discard it.
    rm -rf "$(dirname "$POOL_DIR")"
}

# --- Rewrite boot menu ---

setup_boot() {
    echo "Configuring boot menu..."

    cat > "${WORK_DIR}/isolinux/isolinux.cfg" << 'EOF'
DEFAULT smoothnas
TIMEOUT 50
PROMPT 1
MENU TITLE SmoothNAS Installer

LABEL smoothnas
    MENU LABEL SmoothNAS Install
    MENU DEFAULT
    kernel /install.amd/vmlinuz
    append auto=true priority=critical file=/preseed.cfg DEBCONF_DEBUG=5 console=ttyS0,115200n8 console=tty0 initrd=/install.amd/initrd.gz ---

LABEL bootlocal
    MENU LABEL Boot from first hard disk
    localboot 0x80
EOF

    cat > "${WORK_DIR}/boot/grub/grub.cfg" << 'EOF'
set default=0
set timeout=5

menuentry "SmoothNAS Install" {
    linux /install.amd/vmlinuz auto=true priority=critical file=/preseed.cfg DEBCONF_DEBUG=5 console=ttyS0,115200n8 console=tty0 ---
    initrd /install.amd/initrd.gz
}

menuentry "Boot from first hard disk" {
    set root=(hd0)
    chainloader +1
}
EOF
}

# --- Repack ISO (same flags Debian uses) ---

repack_iso() {
    echo "Repacking ISO..."
    mkdir -p "$OUTPUT_DIR"

    # Regenerate md5sum.
    (cd "$WORK_DIR" && find . -type f ! -name md5sum.txt ! -path './isolinux/*' -exec md5sum {} \; 2>/dev/null) > "${WORK_DIR}/md5sum.txt"

    xorriso -as mkisofs \
        -o "$ISO_FILE" \
        -isohybrid-mbr /usr/lib/ISOLINUX/isohdpfx.bin \
        -c isolinux/boot.cat \
        -b isolinux/isolinux.bin \
        -no-emul-boot \
        -boot-load-size 4 \
        -boot-info-table \
        -eltorito-alt-boot \
        -e boot/grub/efi.img \
        -no-emul-boot \
        -isohybrid-gpt-basdat \
        -V "SmoothNAS ${VERSION}" \
        "$WORK_DIR"

    echo ""
    echo "  ISO: ${ISO_FILE}"
    echo "  Size: $(du -h "$ISO_FILE" | cut -f1)"
}

# --- Main ---

main() {
    echo "=== SmoothNAS ISO Builder v${VERSION} ==="
    check_prereqs
    check_artifacts

    local src
    src=$(download_iso)

    extract_iso "$src"
    setup_initrd
    setup_boot
    repack_iso

    rm -rf "$WORK_DIR"
    echo "Done."
}

main
