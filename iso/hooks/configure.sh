# SmoothNAS configure hook — sourced by smoothiso/installer.sh configure_system.
# Adds NAS-specific tuning, nginx, sysctl, udev and firewall on top of the
# generic system configuration. tierd, its UI, the plugin runtime and their
# systemd units come from the smoothnas package (packages.sh).

# The generic firstboot script reads these settings to use the same marker
# path as the product's systemd unit rather than its /var/lib/linux fallback.
mkdir -p "$TARGET/usr/local/lib/smoothiso"
# The installer is /bin/sh (BusyBox ash), so Bash printf %q is unavailable.
quote_config_value() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}
{
    printf 'PRODUCT_ID='
    quote_config_value "$PRODUCT_ID"
    printf '\nDATA_DIR='
    quote_config_value "$DATA_DIR"
    printf '\n'
} > "$TARGET/usr/local/lib/smoothiso/config.sh"
chmod 644 "$TARGET/usr/local/lib/smoothiso/config.sh"

# Persist the language selected during installation. firstboot.sh reads
# this to write /etc/smoothnas/locale, which the web UI reads before login.
_install_lang="${INSTALLER_LANG:-en}"
case "$_install_lang" in
    en|nl|en-*|nl-*) ;;
    *) _install_lang="en" ;;
esac
mkdir -p "$TARGET/etc/smoothnas"
printf '%s\n' "$_install_lang" > "$TARGET/etc/smoothnas/installer-lang"
chmod 644 "$TARGET/etc/smoothnas/installer-lang"

# Add admin to the tierd group so the daemon can run as the login user.
chroot "$TARGET" groupadd --system tierd 2>/dev/null || true
chroot "$TARGET" usermod -aG tierd admin 2>/dev/null || true

# /etc/issue is written further down once the boot-quietness settings
# are in place, so a single canonical banner ends up on tty1.

# Empty targetcli config so iSCSI starts cleanly.
mkdir -p "$TARGET/etc/target"
echo '{"fabric_modules": [], "storage_objects": [], "targets": []}' \
    > "$TARGET/etc/target/saveconfig.json"

# NAS sysctl tuning. SmoothKernel ships with BBR + FQ built in; this file
# applies the network buffer ceilings, dirty-page thresholds, and VFS cache
# pressure used for NAS workloads on every boot.
cat > "$TARGET/etc/sysctl.d/99-smoothnas.conf" << 'SYSCTL'
# SmoothNAS: NAS performance tuning — managed by installer.

net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.ipv4.tcp_rmem = 4096 87380 134217728
net.ipv4.tcp_wmem = 4096 65536 134217728
net.core.netdev_max_backlog = 5000

net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr

vm.dirty_background_ratio = 5
vm.dirty_ratio = 20
vm.vfs_cache_pressure = 50
SYSCTL

if [ -f /smoothnas/90-smoothnas-net.conf ]; then
    cp /smoothnas/90-smoothnas-net.conf \
        "$TARGET/etc/sysctl.d/90-smoothnas-net.conf"
    chmod 644 "$TARGET/etc/sysctl.d/90-smoothnas-net.conf"
fi

# I/O scheduler udev rules: none for NVMe, BFQ for spinners and md arrays.
cat > "$TARGET/etc/udev/rules.d/60-smoothnas-iosched.rules" << 'UDEV'
# SmoothNAS: I/O scheduler selection — managed by installer.
ACTION=="add|change", KERNEL=="nvme*", ATTR{queue/scheduler}="none"
ACTION=="add|change", KERNEL=="sd[a-z]*", ATTR{queue/rotational}=="0", ATTR{queue/scheduler}="none"
ACTION=="add|change", KERNEL=="sd[a-z]*", ATTR{queue/rotational}=="1", ATTR{queue/scheduler}="bfq"
ACTION=="add|change", KERNEL=="md*", ATTR{queue/scheduler}="bfq"
UDEV

# Extend GRUB cmdline (last assignment wins when /etc/default/grub is sourced).
# Keep the login VT clean by relying on quiet + loglevel=3 +
# systemd.show_status=false. Do NOT pin the kernel console to ttyS0:
# on hardware where the serial port has no reader (USB-serial adapter
# with flow control, qemu socket without a connected client, or just
# a box with no serial hardware at all) the kernel blocks during
# initramfs init waiting for serial output to drain — boot wedges at
# "Loading Linux ..." for many minutes. Operators who actually want
# serial console boot can opt in via smoothiso's
# SMOOTHISO_SERIAL_CONSOLE=1 install env var.
cat >> "$TARGET/etc/default/grub" << 'GRUBCFG'

# SmoothNAS: NAS-tuning kernel cmdline. Login VT stays clean.
#
# amdgpu.runpm=0 keeps discrete AMD accelerators out of runtime D3cold.
# On several headless NAS boards the GPU is visible on PCI but probe fails
# with "Unable to change power state from D3cold to D0" before /dev/dri is
# created; plugin GPU detection and Vulkan containers need that render node.
GRUB_CMDLINE_LINUX="quiet loglevel=3 systemd.show_status=false transparent_hugepage=madvise numa_balancing=disable amdgpu.runpm=0"
GRUBCFG


# journald: don't forward messages to /dev/console so the login VT is
# not polluted by service log output once the system is up. Also CAP the
# journal so it can't grow unbounded on the small root LV — the OS disk is
# ~22 GB and persistent tierd/plugin logs otherwise fill it (observed at ~1 GB
# and climbing). 200 MB is plenty for boot/diagnostics; the rest lives on the
# storage tiers, not root.
mkdir -p "$TARGET/etc/systemd/journald.conf.d"
cat > "$TARGET/etc/systemd/journald.conf.d/00-smoothnas-quiet.conf" << 'JOURNALD'
[Journal]
ForwardToConsole=no
ForwardToWall=no
MaxLevelConsole=emerg
MaxLevelWall=emerg
SystemMaxUse=200M
SystemKeepFree=500M
JOURNALD

# /etc/issue: just the SmoothNAS banner with the IP and a hint that the
# web UI is the normal entry point — no system info above it.
cat > "$TARGET/etc/issue" << 'ISSUE'

  SmoothNAS \n  -  \4

  Web UI:  https://\4
  Log in as `admin` with the password you set during install.

ISSUE

# Replace smoothiso's SSH-only firewall with SmoothNAS's HTTPS-aware ruleset.
cat > "$TARGET/etc/nftables.conf" << 'NFT'
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
        tcp dport 80 accept comment "HTTP redirect"
        tcp dport 443 accept comment "HTTPS"
    }
    chain forward {
        type filter hook forward priority 0; policy drop;
        iifname "veth0" accept
        ct state established,related oifname "veth0" accept
    }
    chain output {
        type filter hook output priority 0; policy accept;
    }
}
NFT

mkdir -p "$TARGET/var/lib/tierd"

ui_status "Configuring system" "Configuring nginx reverse proxy for the SmoothNAS web UI." 4 6

# The web UI's self-signed certificate is generated before nginx starts by
# smoothnas-tls-init.service (smoothnas package), so the web UI is reachable
# from the first seconds of the first boot.
chroot "$TARGET" systemctl enable nginx.service 2>/dev/null || true

# nginx reverse proxy in front of tierd.
mkdir -p "$TARGET/etc/nginx/conf.d/plugins.d"
cat > "$TARGET/etc/nginx/sites-available/tierd" << 'NGINX'
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
        # tierd is down during first-boot setup (storage-stack build takes
        # minutes) and briefly across updates. Answer in the API's own
        # error shape instead of nginx's HTML 502.
        error_page 502 504 = @tierd_down;
    }

    location @tierd_down {
        default_type application/json;
        add_header Retry-After 5 always;
        return 503 '{"error":"SmoothNAS is starting up. The management service is not ready yet; this can take several minutes on the first boot.","code":"system.starting"}';
    }

    include /etc/nginx/conf.d/plugins.d/*.conf;

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
chroot "$TARGET" ln -sf /etc/nginx/sites-available/tierd /etc/nginx/sites-enabled/tierd
chroot "$TARGET" rm -f /etc/nginx/sites-enabled/default

# Sharing services start on demand (tierd manages them).
chroot "$TARGET" systemctl disable smbd.service nmbd.service 2>/dev/null || true
chroot "$TARGET" systemctl disable nfs-kernel-server.service 2>/dev/null || true
chroot "$TARGET" systemctl disable rpcbind.service 2>/dev/null || true
chroot "$TARGET" systemctl disable rtslib-fb-targetctl.service 2>/dev/null || true
