#!/bin/bash
# SmoothNAS late-command: runs inside the installer chroot after package install.
# Installs tierd binary, frontend, nginx config, systemd services, and firstboot.
set -euo pipefail

echo "=== SmoothNAS late-command ==="

# --- Install tierd binary ---
if [ -f /cdrom/smoothnas/tierd ]; then
    install -m 755 /cdrom/smoothnas/tierd /usr/local/bin/tierd
    echo "Installed tierd binary"
else
    echo "WARNING: tierd binary not found on install media"
fi

# --- Install frontend ---
if [ -d /cdrom/smoothnas/tierd-ui ]; then
    mkdir -p /usr/share/tierd-ui
    cp -r /cdrom/smoothnas/tierd-ui/* /usr/share/tierd-ui/
    echo "Installed tierd-ui frontend"
else
    echo "WARNING: tierd-ui not found on install media"
fi

# --- tierd systemd service ---
cat > /etc/systemd/system/tierd.service << 'UNIT'
[Unit]
Description=SmoothNAS Storage Management Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=+/bin/sh -c '[ -f /etc/pam.d/tierd ] || printf "auth [success=1 default=ignore] pam_unix.so nullok\nauth requisite pam_deny.so\nauth required pam_permit.so\naccount [success=1 new_authtok_reqd=done default=ignore] pam_unix.so\naccount requisite pam_deny.so\naccount required pam_permit.so\n" > /etc/pam.d/tierd'
ExecStart=/usr/local/bin/tierd
Environment=TIERD_ADDR=127.0.0.1:8420
Environment=TIERD_DB=/var/lib/tierd/tierd.db
Environment=HOME=/root
Environment=USER=root
Environment=LOGNAME=root
Restart=on-failure
RestartSec=5
PrivateTmp=true
NoNewPrivileges=false

[Install]
WantedBy=multi-user.target
UNIT

mkdir -p /var/lib/tierd
systemctl enable tierd.service

# --- nginx config ---
cat > /etc/nginx/sites-available/tierd << 'NGINX'
server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name _;

    client_max_body_size 512m;

    ssl_certificate     /etc/tierd/tls/cert.pem;
    ssl_certificate_key /etc/tierd/tls/key.pem;

    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers HIGH:!aNULL:!MD5;
    ssl_prefer_server_ciphers on;

    root /usr/share/tierd-ui;
    index index.html;

    location /api/ {
        proxy_pass http://127.0.0.1:8420;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }

    location / {
        try_files $uri $uri/ /index.html;
    }
}

server {
    listen 80;
    listen [::]:80;
    server_name _;
    return 301 https://$host$request_uri;
}
NGINX

ln -sf /etc/nginx/sites-available/tierd /etc/nginx/sites-enabled/tierd
rm -f /etc/nginx/sites-enabled/default

# --- firstboot service ---
if [ -f /cdrom/smoothnas/firstboot.sh ]; then
    install -m 755 /cdrom/smoothnas/firstboot.sh /usr/local/bin/smoothnas-firstboot
fi

cat > /etc/systemd/system/smoothnas-firstboot.service << 'UNIT'
[Unit]
Description=SmoothNAS First Boot Setup
After=network-online.target
ConditionPathExists=!/var/lib/tierd/.firstboot-done

[Service]
Type=oneshot
ExecStart=/usr/local/bin/smoothnas-firstboot
RemainAfterExit=true

[Install]
WantedBy=multi-user.target
UNIT

systemctl enable smoothnas-firstboot.service

# --- Disable ifupdown (conflicts with systemd-networkd) ---
systemctl disable networking.service 2>/dev/null || true
systemctl mask networking.service 2>/dev/null || true

# --- Enable systemd-networkd ---
systemctl enable systemd-networkd.service
systemctl enable systemd-resolved.service

# --- Point resolv.conf at systemd-resolved ---
ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf

# --- Base nftables rules ---
cat > /etc/nftables.conf << 'NFT'
#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;
        ct state established,related accept
        iif lo accept
        meta l4proto icmp accept
        meta l4proto icmpv6 accept
        tcp dport 22 accept comment "SSH"
        tcp dport 443 accept comment "HTTPS"
    }
    chain forward {
        type filter hook forward priority 0; policy drop;
    }
    chain output {
        type filter hook output priority 0; policy accept;
    }
}
NFT

systemctl enable nftables.service

# --- Disable sharing services by default (tierd enables them on demand) ---
systemctl disable smbd.service nmbd.service 2>/dev/null || true
systemctl disable nfs-kernel-server.service 2>/dev/null || true
systemctl disable rpcbind.service 2>/dev/null || true

# --- Clean up ---
rm -f /tmp/late-command.sh

echo "=== SmoothNAS late-command complete ==="
