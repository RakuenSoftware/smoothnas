#!/bin/sh
# Tests for usr/lib/smoothnas/network-fallback.sh against a fake
# /sys/class/net and stub networkctl, ip and sleep.
#
# Usage: packaging/tests/network-fallback-test.sh
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../smoothnas/usr/lib/smoothnas/network-fallback.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
failures=0

fail() {
    echo "FAIL: $case_name: $*" >&2
    failures=$((failures + 1))
}

# nic <name> <carrier> [master]: a physical Ethernet NIC.
nic() {
    d="$SYS/$1"
    mkdir -p "$d/device"
    echo 1 > "$d/type"
    echo "$2" > "$d/carrier"
    if [ $# -ge 3 ]; then ln -s "../$3" "$d/master"; fi
}

# soft <name>: a software link (bond, veth, bridge) with no device.
soft() {
    mkdir -p "$SYS/$1"
    echo 1 > "$SYS/$1/type"
    echo 0 > "$SYS/$1/carrier"
}

setup() {
    case_name="$1"
    T="$WORK/$1"
    SYS="$T/sys"
    BIN="$T/bin"
    mkdir -p "$SYS" "$BIN" "$T/run"
    : > "$T/calls"
    echo 0 > "$T/ticks"
    # networkctl list prints what the case put in $T/links.
    printf '%s\n' "IDX LINK TYPE OPERATIONAL SETUP" > "$T/links.header"
    : > "$T/links"
    cat > "$BIN/networkctl" <<EOF
#!/bin/sh
echo "networkctl \$*" >> "$T/calls"
[ "\$1" = list ] && cat "$T/links"
exit 0
EOF
    cat > "$BIN/ip" <<EOF
#!/bin/sh
echo "ip \$*" >> "$T/calls"
exit 0
EOF
    # Each sleep is one tick; an on-tick hook lets a case change the world.
    cat > "$BIN/sleep" <<EOF
#!/bin/sh
n=\$((\$(cat "$T/ticks") + 1))
echo "\$n" > "$T/ticks"
[ -x "$T/on-tick" ] && "$T/on-tick" "\$n"
[ "\$n" -ge 500 ] && { echo "sleep stub: too many ticks" >&2; kill -TERM \$PPID; }
exit 0
EOF
    chmod +x "$BIN/networkctl" "$BIN/ip" "$BIN/sleep"
}

run() {
    set +e
    PATH="$BIN:$PATH" \
    SMOOTHNAS_SYS_CLASS_NET="$SYS" \
    SMOOTHNAS_RUN_NETWORK_DIR="$T/run" \
    SMOOTHNAS_NETWORK_FALLBACK_STATE="$T/state" \
    SMOOTHNAS_NETWORK_FALLBACK_TIMEOUT="${TIMEOUT:-10}" \
    SMOOTHNAS_NETWORK_FALLBACK_INTERVAL=1 \
        sh "$SCRIPT" 2> "$T/stderr"
    status=$?
    set -e
}

fallback_files() { ls "$T/run" 2>/dev/null | sort | tr '\n' ' '; }
ticks() { cat "$T/ticks"; }

# 1. The configured network is up: exit at once and touch nothing.
setup routable-at-boot
nic net0 1 bond0
soft bond0
echo "9 bond0 bond routable configured" > "$T/links"
run
[ "$status" = 0 ] || fail "exit $status"
[ -z "$(fallback_files)" ] || fail "wrote fallback files: $(fallback_files)"
[ ! -e "$T/state" ] || fail "wrote state file"
grep -q '^ip ' "$T/calls" && fail "changed links: $(cat "$T/calls")"

# 2. The WTR Max outage: udev renamed bond0 to net0, the real NIC kept
# eth1, nothing managed is routable, and lxcbr0 (unmanaged) has an address.
setup wtr-max-renamed-bond
soft net0                 # the renamed bond: no device
nic eth1 0 bond0          # the NIC that should have been net0
nic net1 1 bond0          # X710 port with link
nic net2 0 net0           # enslaved to the renamed bond
nic net3 0 net0
soft bond0                # recreated by hand, no address
soft lxcbr0
soft veth0
cat > "$T/links" <<EOF
2 net0 bond no-carrier configured
4 eth1 ether no-carrier configured
6 net1 ether enslaved configured
7 lxcbr0 bridge routable unmanaged
9 bond0 bond no-carrier configuring
EOF
TIMEOUT=10 run
[ "$status" = 0 ] || fail "exit $status: $(cat "$T/stderr")"
want="00-smoothnas-fallback-eth1.network 00-smoothnas-fallback-net1.network 00-smoothnas-fallback-net2.network 00-smoothnas-fallback-net3.network "
[ "$(fallback_files)" = "$want" ] || fail "fallback files: got '$(fallback_files)', want '$want'"
for n in eth1 net1 net2 net3; do
    f="$T/run/00-smoothnas-fallback-$n.network"
    grep -qx "Name=$n" "$f" || fail "$n file does not match $n"
    grep -qx "DHCP=ipv4" "$f" || fail "$n file does not DHCP"
    grep -qx "ClientIdentifier=mac" "$f" || fail "$n file does not identify by MAC"
    grep -qx "ip link set dev $n nomaster" "$T/calls" || fail "$n not released from its bond"
    grep -qx "networkctl reconfigure $n" "$T/calls" || fail "$n not reconfigured"
done
grep -qx "networkctl reload" "$T/calls" || fail "networkd not reloaded"
for s in net0 bond0 lxcbr0 veth0; do
    [ ! -e "$T/run/00-smoothnas-fallback-$s.network" ] || fail "fell back on software link $s"
done
grep -q "^nics=eth1 net1 net2 net3$" "$T/state" || fail "state file: $(cat "$T/state" 2>/dev/null)"
[ "$(ticks)" -ge 10 ] || fail "fell back after $(ticks)s, before the 10s timeout"
[ "$(ticks)" -le 11 ] || fail "fell back after $(ticks)s, well past the 10s timeout"

# 3. No carrier: the switch is still booting. Never fall back, however long.
setup no-carrier
nic net0 0 bond0
soft bond0
echo "9 bond0 bond no-carrier configuring" > "$T/links"
cat > "$T/on-tick" <<EOF
#!/bin/sh
# After 300s the switch is still down; stop the test.
[ "\$1" -ge 300 ] && echo "9 bond0 bond routable configured" > "$T/links"
exit 0
EOF
chmod +x "$T/on-tick"
TIMEOUT=10 run
[ "$status" = 0 ] || fail "exit $status"
[ -z "$(fallback_files)" ] || fail "fell back without carrier: $(fallback_files)"

# 4. The clock starts at carrier: the switch comes up at 200s, the bond's
# static address applies 5s later. No fallback.
setup late-switch
nic net0 0 bond0
soft bond0
echo "9 bond0 bond no-carrier configuring" > "$T/links"
cat > "$T/on-tick" <<EOF
#!/bin/sh
[ "\$1" -eq 200 ] && echo 1 > "$SYS/net0/carrier"
[ "\$1" -eq 205 ] && echo "9 bond0 bond routable configured" > "$T/links"
exit 0
EOF
chmod +x "$T/on-tick"
TIMEOUT=10 run
[ "$status" = 0 ] || fail "exit $status"
[ -z "$(fallback_files)" ] || fail "fell back although the network came up within the timeout of carrier: $(fallback_files)"

# 5. Carrier appears late and the network never comes up: fall back TIMEOUT
# seconds after carrier, not after boot.
setup late-carrier-broken
nic net0 0 bond0
soft bond0
echo "9 bond0 bond no-carrier configuring" > "$T/links"
cat > "$T/on-tick" <<EOF
#!/bin/sh
[ "\$1" -eq 100 ] && echo 1 > "$SYS/net0/carrier"
exit 0
EOF
chmod +x "$T/on-tick"
TIMEOUT=10 run
[ "$status" = 0 ] || fail "exit $status"
[ "$(fallback_files)" = "00-smoothnas-fallback-net0.network " ] || fail "fallback files: '$(fallback_files)'"
[ "$(ticks)" -ge 110 ] && [ "$(ticks)" -le 111 ] || fail "fell back at tick $(ticks), want 10s after carrier at tick 100"

# 6. A routable static address with an unreachable gateway still counts as up.
setup static-no-gateway
nic net0 1
echo "2 net0 ether routable configured" > "$T/links"
run
[ "$status" = 0 ] || fail "exit $status"
[ -z "$(fallback_files)" ] || fail "fell back on a routable static address"

# 7. Only unmanaged links are routable (container bridges): fall back.
setup only-unmanaged-routable
nic net0 1
soft lxcbr0
cat > "$T/links" <<EOF
2 net0 ether degraded configuring
7 lxcbr0 bridge routable unmanaged
EOF
TIMEOUT=3 run
[ "$status" = 0 ] || fail "exit $status"
[ "$(fallback_files)" = "00-smoothnas-fallback-net0.network " ] || fail "fallback files: '$(fallback_files)'"
grep -q "nomaster" "$T/calls" && fail "released a NIC that had no master"

# 8. Wireless and non-Ethernet devices are not fallback targets.
setup skips-wireless
nic net0 1
mkdir -p "$SYS/wlan0/device" "$SYS/wlan0/wireless"; echo 1 > "$SYS/wlan0/type"; echo 1 > "$SYS/wlan0/carrier"
mkdir -p "$SYS/ib0/device"; echo 32 > "$SYS/ib0/type"; echo 1 > "$SYS/ib0/carrier"
echo "2 net0 ether degraded configuring" > "$T/links"
TIMEOUT=2 run
[ "$(fallback_files)" = "00-smoothnas-fallback-net0.network " ] || fail "fallback files: '$(fallback_files)'"

if [ "$failures" -ne 0 ]; then
    echo "$failures failure(s)" >&2
    exit 1
fi
echo "network-fallback: all cases passed"
