#!/bin/bash
# Generates a self-signed TLS certificate for first boot.
# Run once during installation or first boot.
set -euo pipefail

TLS_DIR="/etc/tierd/tls"
CERT="$TLS_DIR/cert.pem"
KEY="$TLS_DIR/key.pem"

if [ -f "$CERT" ] && [ -f "$KEY" ]; then
    echo "TLS certificate already exists, skipping generation."
    exit 0
fi

mkdir -p "$TLS_DIR"

openssl req -x509 -nodes \
    -days 3650 \
    -newkey rsa:2048 \
    -keyout "$KEY" \
    -out "$CERT" \
    -subj "/CN=smoothnas/O=SmoothNAS" \
    -addext "subjectAltName=DNS:smoothnas,DNS:localhost,IP:127.0.0.1"

chmod 600 "$KEY"
chmod 644 "$CERT"

echo "Self-signed TLS certificate generated at $TLS_DIR"
