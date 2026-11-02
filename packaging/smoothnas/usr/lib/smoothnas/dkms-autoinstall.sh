#!/bin/sh
set -u

# Build every DKMS module (OpenZFS, smoothfs, ...) for every installed kernel
# that has its headers.
#
# A kernel's own postinst runs the DKMS hook, which skips the kernel when its
# headers aren't configured yet. Debian's headers packages rerun the hook when
# they're configured; SmoothKernel's bindeb-pkg headers have no postinst. So
# when apt configures linux-image before linux-headers, nothing ever builds
# the modules for the new kernel. That boots a NAS without ZFS: its pools
# don't import, and every share on them fails.
#
# Runs from the smoothnas postinst and after every dpkg run (apt.conf.d
# 53smoothnas-dkms), so modules get built whatever order packages configure
# in. `dkms autoinstall` skips modules already installed, so a run with
# nothing to build costs a status check per kernel.

MODULES_DIR="${SMOOTHNAS_MODULES_DIR:-/lib/modules}"

log() { echo "smoothnas-dkms: $*" >&2; }

command -v dkms >/dev/null 2>&1 || exit 0

status=0
for dir in "$MODULES_DIR"/*; do
    [ -d "$dir" ] || continue
    kver="$(basename "$dir")"
    # build links to the kernel's headers; it dangles until they're
    # installed, and then DKMS can't build for this kernel anyway.
    [ -f "$dir/build/Makefile" ] || continue
    if ! dkms autoinstall -k "$kver"; then
        log "could not build DKMS modules for $kver; see /var/lib/dkms/*/*/build/make.log"
        status=1
    fi
done
exit "$status"
