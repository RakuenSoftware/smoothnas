#!/bin/sh
set -u

# SmoothNAS boot-time network fallback.
#
# A NAS that boots without a usable address can only be reached from its
# console. If no systemd-networkd managed link becomes routable within
# TIMEOUT seconds of a physical NIC seeing carrier, release every physical
# NIC from its bond and DHCP each one on its own. Fallback config lives in
# /run, so it lasts until the next boot and never replaces the operator's
# configuration in /etc/systemd/network.
#
# The clock starts at carrier, not at boot. After a power cut the switch
# often comes up after the NAS, and a correctly configured box mustn't give
# up its static address because the switch was slow.

TIMEOUT="${SMOOTHNAS_NETWORK_FALLBACK_TIMEOUT:-90}"
INTERVAL="${SMOOTHNAS_NETWORK_FALLBACK_INTERVAL:-2}"
SYS_CLASS_NET="${SMOOTHNAS_SYS_CLASS_NET:-/sys/class/net}"
RUN_NETWORK_DIR="${SMOOTHNAS_RUN_NETWORK_DIR:-/run/systemd/network}"
STATE_FILE="${SMOOTHNAS_NETWORK_FALLBACK_STATE:-/run/smoothnas/network-fallback}"

log() { echo "smoothnas-network-fallback: $*" >&2; }

# Physical Ethernet: backed by a device, ARPHRD_ETHER, not wireless. Bonds,
# VLANs, bridges and veths have no device link.
physical_nics() {
    for dev in "$SYS_CLASS_NET"/*; do
        [ -e "$dev/device" ] || continue
        [ "$(cat "$dev/type" 2>/dev/null)" = 1 ] || continue
        [ -e "$dev/wireless" ] && continue
        [ -e "$dev/phy80211" ] && continue
        basename "$dev"
    done
}

any_carrier() {
    for nic in $(physical_nics); do
        [ "$(cat "$SYS_CLASS_NET/$nic/carrier" 2>/dev/null)" = 1 ] && return 0
    done
    return 1
}

# A networkd-managed link (not loopback, not an unmanaged container bridge)
# with a routable address means the configured network came up.
managed_routable() {
    networkctl list --no-legend --no-pager 2>/dev/null |
        awk '$3 != "loopback" && $4 == "routable" && $5 != "unmanaged" { found = 1 } END { exit !found }'
}

fall_back() {
    nics="$(physical_nics)"
    if [ -z "$nics" ]; then
        log "no physical Ethernet NICs to fall back to"
        return 1
    fi
    mkdir -p "$RUN_NETWORK_DIR" "$(dirname "$STATE_FILE")"
    for nic in $nics; do
        # 00- sorts ahead of every tierd and installer file, so networkd
        # matches this file first.
        cat > "$RUN_NETWORK_DIR/00-smoothnas-fallback-$nic.network" <<EOF
# Written by smoothnas-network-fallback: the configured network did not come
# up, so this NIC DHCPs on its own until the next boot.
[Match]
Name=$nic

[Network]
DHCP=ipv4
IPv6AcceptRA=yes

[DHCPv4]
ClientIdentifier=mac
EOF
        # The bond's own .network still names it a member; detach so the NIC
        # gets its own MAC back and can DHCP.
        if [ -e "$SYS_CLASS_NET/$nic/master" ]; then
            ip link set dev "$nic" nomaster || log "could not release $nic from its bond"
        fi
        ip link set dev "$nic" up || true
    done
    networkctl reload || log "networkctl reload failed"
    for nic in $nics; do
        networkctl reconfigure "$nic" || log "networkctl reconfigure $nic failed"
    done
    printf 'activated=%s\nnics=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(echo $nics)" > "$STATE_FILE"
    log "configured network did not come up within ${TIMEOUT}s of carrier; DHCP on each NIC: $(echo $nics)"
    return 0
}

carrier_seen=""
elapsed=0
while :; do
    if managed_routable; then
        exit 0
    fi
    if [ -z "$carrier_seen" ] && any_carrier; then
        carrier_seen=1
        log "carrier is up; waiting ${TIMEOUT}s for the configured network"
    fi
    if [ -n "$carrier_seen" ]; then
        if [ "$elapsed" -ge "$TIMEOUT" ]; then
            fall_back
            exit $?
        fi
        elapsed=$((elapsed + INTERVAL))
    fi
    sleep "$INTERVAL"
done
