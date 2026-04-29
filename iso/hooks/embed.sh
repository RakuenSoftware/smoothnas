#!/bin/sh
# SmoothNAS embed hook for smoothiso build-iso.sh.
# Stages the SmoothNAS payload (tierd, frontend, repo, smoothfs source,
# tests, manifest) into the installer initrd at /smoothnas/.
set -e

if [ -d "${SMOOTHNAS_PAYLOAD_DIR}" ]; then
    cp -a "${SMOOTHNAS_PAYLOAD_DIR}/." "${INITRD_TMP}/smoothnas/"
fi
