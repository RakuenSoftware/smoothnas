#!/bin/sh
# SmoothNAS disk selection script.
# Runs as preseed/early_command inside the Debian installer environment.
# Presents a disk selection dialog, then configures partman for either
# single-disk LVM or RAID-1 + LVM based on how many disks are selected.
#
# Available in the installer: sh, grep, awk, sed, cat, lsblk (maybe),
# /proc/partitions, whiptail/dialog, debconf-set.

set -e

# --- Discover disks ---
# The installer environment may not have lsblk. Parse /proc/partitions
# for whole disks (no partition number suffix).
discover_disks() {
    local unsorted=""
    for dev in /sys/block/*; do
        local name=$(basename "$dev")

        # Skip non-physical devices.
        case "$name" in
            loop*|ram*|sr*|dm-*|md*|nbd*|zram*) continue ;;
        esac

        # Skip if no size or size is 0.
        local size_sectors=$(cat "$dev/size" 2>/dev/null || echo 0)
        [ "$size_sectors" -le 0 ] 2>/dev/null && continue

        # Get size in GB.
        local size_bytes=$((size_sectors * 512))
        local size_gb=$((size_bytes / 1073741824))

        # Skip tiny disks (<1GB).
        [ "$size_gb" -lt 1 ] && continue

        # Skip USB drives.
        local removable=$(cat "$dev/removable" 2>/dev/null || echo 0)
        if [ "$removable" = "1" ]; then
            continue
        fi
        local devpath=$(readlink -f "$dev" 2>/dev/null || echo "")
        case "$devpath" in
            */usb*) continue ;;
        esac

        # Detect if this is the boot/installer device and mark it.
        local is_installer=0
        # Check if any partition on this disk is mounted (installer media).
        for mnt in $(grep "^/dev/${name}" /proc/mounts 2>/dev/null | awk '{print $1}'); do
            is_installer=1
        done

        # Skip the installer device entirely.
        [ "$is_installer" = "1" ] && continue

        local model=$(cat "$dev/device/model" 2>/dev/null | sed 's/[[:space:]]*$//' || echo "Unknown")

        unsorted="$unsorted
$size_sectors /dev/$name \"${size_gb}GB ${model}\" off"
    done

    # Sort by size (ascending) and strip the sort key.
    local disks=""
    local sorted=$(echo "$unsorted" | sort -n)
    local IFS_OLD="$IFS"
    IFS='
'
    for line in $sorted; do
        [ -z "$line" ] && continue
        local entry=$(echo "$line" | sed 's/^[0-9]* //')
        disks="$disks $entry"
    done
    IFS="$IFS_OLD"
    echo "$disks"
}

# --- Present disk selection ---
DISKS=$(discover_disks)

if [ -z "$DISKS" ]; then
    # No disks found, let the installer handle it.
    exit 0
fi

# Count available disks.
DISK_COUNT=$(echo "$DISKS" | tr ' ' '\n' | grep '^/dev/' | wc -l)

if [ "$DISK_COUNT" -lt 1 ]; then
    exit 0
fi

# Use whiptail for the dialog. Fall back to first disk if whiptail is unavailable.
if ! command -v whiptail >/dev/null 2>&1; then
    # No whiptail, use the first disk as single-disk.
    FIRST_DISK=$(echo "$DISKS" | tr ' ' '\n' | grep '^/dev/' | head -1)
    debconf-set partman-auto/disk "$FIRST_DISK"
    debconf-set partman-auto/method lvm
    debconf-set grub-installer/bootdev "$FIRST_DISK"
    exit 0
fi

# Show checklist. The eval handles the quoted arguments properly.
SELECTED=$(eval whiptail --title \"SmoothNAS Disk Selection\" \
    --checklist \"Select disk\(s\) for the OS.\\n\\nOne disk: single-disk LVM.\\nTwo or more disks: RAID-1 mirror + LVM.\\n\\nRemaining disks are managed by SmoothNAS.\" \
    20 70 "$DISK_COUNT" \
    $DISKS \
    3>&1 1>&2 2>&3) || true

# Strip quotes from whiptail output.
SELECTED=$(echo "$SELECTED" | tr -d '"')

if [ -z "$SELECTED" ]; then
    # User cancelled or selected nothing. Let installer prompt normally.
    exit 0
fi

# Count selected disks.
SEL_COUNT=$(echo "$SELECTED" | wc -w)

if [ "$SEL_COUNT" -eq 1 ]; then
    # --- Single disk: LVM ---
    debconf-set partman-auto/disk "$SELECTED"
    debconf-set partman-auto/method lvm
    debconf-set partman-auto/choose_recipe atomic
    debconf-set partman-auto-lvm/guided_size max
    debconf-set partman-auto-lvm/new_vg_name smoothnas-vg
    # Install GRUB to the selected disk, not the USB.
    debconf-set grub-installer/bootdev "$SELECTED"
else
    # --- Multiple disks: RAID-1 + LVM ---
    # Join selected disks with spaces for partman-auto/disk.
    DISK_LIST=$(echo "$SELECTED" | tr '\n' ' ')
    debconf-set partman-auto/disk "$DISK_LIST"
    # Install GRUB to the first selected disk.
    FIRST_DISK=$(echo "$SELECTED" | head -1)
    debconf-set grub-installer/bootdev "$FIRST_DISK"
    debconf-set partman-auto/method raid

    # RAID recipe: level, device-count, spare-count, fs, mountpoint, devices
    # Two entries: /boot on RAID-1, and everything else on RAID-1 for LVM.
    debconf-set partman-auto-raid/recipe "1 $SEL_COUNT 0 ext4 /boot . 1 $SEL_COUNT 0 lvm - ."

    # Partition recipe for each member disk.
    debconf-set partman-auto/expert_recipe "smoothnas-raid :: \
        512 512 512 ext4 \
            \$primary{ } \$bootable{ } \
            method{ raid } . \
        4096 4096 4096 linux-swap \
            method{ raid } . \
        8192 20480 -1 ext4 \
            method{ raid } ."

    debconf-set partman-auto/choose_recipe smoothnas-raid
    debconf-set partman-auto-lvm/guided_size max
    debconf-set partman-auto-lvm/new_vg_name smoothnas-vg
    debconf-set partman-md/confirm true
    debconf-set partman-md/confirm_nooverwrite true
fi
