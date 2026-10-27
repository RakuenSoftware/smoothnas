#!/bin/sh
set -eu

# SmoothNAS GPU bring-up for plugin runtimes.
#
# AMD dGPUs used for headless inference can come up in D3cold on server boards.
# If amdgpu probes while the device is inaccessible, /dev/dri never appears and
# plugin manifests correctly select gpu-amd but have no render node to pass
# through. Disable D3cold before udev triggers driver probing; leave
# NVIDIA/Intel to their normal driver paths.
for dev in /sys/bus/pci/devices/*; do
    [ -r "$dev/vendor" ] || continue
    [ "$(cat "$dev/vendor" 2>/dev/null)" = "0x1002" ] || continue
    class="$(cat "$dev/class" 2>/dev/null || true)"
    case "$class" in
        0x03*) ;;
        *) continue ;;
    esac
    [ -w "$dev/d3cold_allowed" ] && echo 0 > "$dev/d3cold_allowed" 2>/dev/null || true
    [ -w "$dev/power/control" ] && echo on > "$dev/power/control" 2>/dev/null || true
done
