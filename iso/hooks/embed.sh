#!/bin/sh
# SmoothNAS embed hook for smoothiso build-iso.sh.
# Stages the SmoothNAS payload (tierd, frontend, repo, smoothfs source,
# tests, manifest) into the installer initrd at /smoothnas/.
set -e

if [ -d "${SMOOTHNAS_PAYLOAD_DIR}" ]; then
    cp -a "${SMOOTHNAS_PAYLOAD_DIR}/." "${INITRD_TMP}/smoothnas/"
fi

# simpledrm claims the UEFI GOP framebuffer on OVMF boot, preventing bochs-drm
# from getting DRM control and causing all Xorg startup attempts to fail.
find "${INITRD_TMP}/lib/modules" -name "simpledrm.ko*" -delete 2>/dev/null || true
