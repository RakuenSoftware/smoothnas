#!/bin/bash
# SmoothNAS first-boot setup. Runs once on the first boot after install.
# Completes deferred package configuration and generates TLS certificate.
#
# NOTE: The admin account is created by the custom installer
# (smoothnas-install), NOT here. Do not add user creation logic.
set -euo pipefail

MARKER="/var/lib/tierd/.firstboot-done"

if [ -f "$MARKER" ]; then
    echo "First boot already completed."
    exit 0
fi

echo "============================================="
echo "  SmoothNAS First Boot Setup"
echo "============================================="

# --- Reconfigure packages deferred from install ---
# The installer stubs debconf-using postinsts to avoid hangs. Run them now
# in a proper environment where debconf works.
echo "Completing package configuration..."
DEBIAN_FRONTEND=noninteractive dpkg --configure --pending 2>/dev/null || true
# The installer stubbed the ca-certificates postinst (it sources debconf's
# confmodule), so update-ca-certificates never ran.  Generate the CA bundle
# now so that TLS connections (e.g. GitHub API for OS updates) work.
update-ca-certificates --fresh 2>/dev/null || true
echo "Package configuration complete."

# --- Generate self-signed TLS certificate ---
TLS_DIR="/etc/tierd/tls"
mkdir -p "$TLS_DIR"

if [ ! -f "$TLS_DIR/cert.pem" ]; then
    HOSTNAME=$(hostname)
    openssl req -x509 -nodes \
        -days 3650 \
        -newkey rsa:2048 \
        -keyout "$TLS_DIR/key.pem" \
        -out "$TLS_DIR/cert.pem" \
        -subj "/CN=${HOSTNAME}/O=SmoothNAS" \
        -addext "subjectAltName=DNS:${HOSTNAME},DNS:localhost,IP:127.0.0.1" \
        2>/dev/null

    chmod 600 "$TLS_DIR/key.pem"
    chmod 644 "$TLS_DIR/cert.pem"
    echo "Generated self-signed TLS certificate."
fi

# --- Ensure tierd system group exists ---
if ! getent group tierd >/dev/null 2>&1; then
    groupadd --system tierd
    echo "Created tierd system group."
fi

# --- Ensure tierd PAM service exists ---
# The installer creates this file, but upgrades from older installations
# may not have it. Without it, PAM authentication for the web UI fails
# because the Go binary references the "tierd" service.
if [ ! -f /etc/pam.d/tierd ]; then
    cat > /etc/pam.d/tierd << 'PAM'
# PAM service for tierd web UI authentication.
auth	[success=1 default=ignore]	pam_unix.so nullok
auth	requisite			pam_deny.so
auth	required			pam_permit.so
account	[success=1 new_authtok_reqd=done default=ignore]	pam_unix.so
account	requisite			pam_deny.so
account	required			pam_permit.so
PAM
    echo "Created tierd PAM service."
fi

# --- Randomise root password ---
# The installer temporarily sets root's password to match admin. Replace it
# with a random password so the web UI (admin via PAM) is the normal entry
# point. The random password is saved to /var/lib/tierd/initial-credentials
# for emergency/rescue access.
ROOT_PASS=$(openssl rand -base64 12 | tr -dc 'a-zA-Z0-9' | head -c 16)
echo "root:${ROOT_PASS}" | chpasswd

mkdir -p /var/lib/tierd
cat > /var/lib/tierd/initial-credentials << CREDS
SmoothNAS Initial Credentials
Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)

Root password: ${ROOT_PASS}

This is for emergency/rescue access only.
Log in to the web UI with the admin password you set during install.
Delete this file after recording the root password.
CREDS
chmod 600 /var/lib/tierd/initial-credentials

# --- Initialize tierd database ---
mkdir -p /var/lib/tierd

# --- Configure and enable SSH ---
# openssh-server is installed but its postinst may have been stubbed during
# install, leaving the config and host keys absent. Generate them now.
if [ ! -f /etc/ssh/sshd_config ]; then
    cat > /etc/ssh/sshd_config << 'SSHD'
Include /etc/ssh/sshd_config.d/*.conf
PermitRootLogin yes
PasswordAuthentication yes
SSHD
    echo "Created sshd_config."
fi
ssh-keygen -A 2>/dev/null || true
mkdir -p /run/sshd
systemctl enable ssh.service 2>/dev/null || true
systemctl start ssh.service 2>/dev/null || true
echo "SSH configured and started."

# --- Enable and restart nginx (TLS cert was just generated) ---
# Enable nginx so it survives subsequent reboots (the installer may not have
# done this if apt's postinst ran before systemd was fully initialised).
systemctl enable nginx.service 2>/dev/null || true
systemctl restart nginx 2>/dev/null || true
# NOTE: Do NOT restart tierd here. tierd.service has
# After=smoothnas-firstboot.service, so systemd will start it
# automatically once this script exits. Calling systemctl restart
# tierd from inside firstboot creates a deadlock: the restart blocks
# waiting for tierd to become active, but tierd can't start until
# firstboot finishes.

# --- Mark first boot as done ---
touch "$MARKER"
echo "First boot setup complete."
