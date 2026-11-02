#!/bin/bash
# Real-udev regression test for the bond rename that took the Aoostar WTR Max
# off the network: a NIC naming .link file that matches MACAddress= also
# matches a bond carrying that NIC's MAC, and udev renames the bond.
#
# Runs as root on a host with systemd-udevd and the bonding module (the CI
# runner, or a SmoothNAS box). It only touches a test bond and one
# uniquely named .link file, and removes both on exit.
#
#   1. Negative control: with MACAddress=, udev renames the bond. If this
#      does not reproduce, the host can't show the bug and the test fails
#      rather than passing vacuously.
#   2. The fix: with PermanentMACAddress=, the bond keeps its name.
#   3. PermanentMACAddress= still matches a real NIC (dry run through
#      `udevadm test-builtin`, with Name= set to the NIC's current name, so
#      nothing is renamed even if it were applied).
#   4. The link file tierd's migration produces and the one the installer
#      writes are what step 2 tested.
set -euo pipefail

[ "$(id -u)" = 0 ] || { echo "must run as root" >&2; exit 2; }

LINK_DIR=/etc/systemd/network
LINK="$LINK_DIR/00-smoothnas-ci-bond-rename.link"
BOND=smcibond0
RENAMED=smcinic0
# Locally administered, so it can't collide with real hardware.
MAC=02:5a:4e:c1:00:01

cleanup() {
    ip link del "$BOND" 2>/dev/null || true
    ip link del "$RENAMED" 2>/dev/null || true
    rm -f "$LINK"
    udevadm control --reload 2>/dev/null || true
}
trap cleanup EXIT
cleanup
modprobe bonding
mkdir -p "$LINK_DIR"

write_link() { # <selector> <mac> <name>
    printf '[Match]\n%s=%s\n\n[Link]\nName=%s\n' "$1" "$2" "$3" > "$LINK"
    udevadm control --reload
}

# Create the bond the way networkd does: the MAC is set in the same netlink
# request, so it's already the member's MAC when udev handles the add event.
bond_name_after_add() {
    ip link del "$BOND" 2>/dev/null || true
    ip link del "$RENAMED" 2>/dev/null || true
    ip link add "$BOND" address "$MAC" type bond mode active-backup
    udevadm settle --timeout=30
    if [ -e "/sys/class/net/$RENAMED" ]; then
        echo "$RENAMED"
    elif [ -e "/sys/class/net/$BOND" ]; then
        echo "$BOND"
    else
        echo "missing"
    fi
}

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

echo "== 1. negative control: MACAddress= renames a bond carrying that MAC"
write_link MACAddress "$MAC" "$RENAMED"
got="$(bond_name_after_add)"
if [ "$got" != "$RENAMED" ]; then
    fail "with MACAddress= the bond is '$got', want '$RENAMED': this host does not reproduce the bug, so step 2 would prove nothing"
fi

echo "== 2. PermanentMACAddress= does not match the bond"
write_link PermanentMACAddress "$MAC" "$RENAMED"
got="$(bond_name_after_add)"
[ "$got" = "$BOND" ] || fail "with PermanentMACAddress= the bond is '$got', want '$BOND'"

echo "== 3. PermanentMACAddress= matches a real NIC"
nic=""
perm=""
for dev in /sys/class/net/*; do
    [ -e "$dev/device" ] || continue
    [ "$(cat "$dev/type")" = 1 ] || continue
    n="$(basename "$dev")"
    p="$(ip -d link show dev "$n" | sed -n 's/.*permaddr \([0-9a-f:]*\).*/\1/p')"
    # Unenslaved NICs don't print permaddr; their address is the permanent
    # one when the kernel says it was assigned by hardware (0 = PERM).
    if [ -z "$p" ] && [ "$(cat "$dev/addr_assign_type" 2>/dev/null)" = 0 ]; then
        p="$(cat "$dev/address")"
    fi
    if [ -n "$p" ] && [ "$p" != 00:00:00:00:00:00 ]; then
        nic="$n"; perm="$p"; break
    fi
done
if [ -z "$nic" ]; then
    fail "no physical NIC with a permanent MAC on this host"
else
    write_link PermanentMACAddress "$perm" "$nic"
    out="$(udevadm test-builtin net_setup_link "/sys/class/net/$nic" 2>&1 || true)"
    if ! grep -q "$LINK" <<<"$out"; then
        echo "$out" >&2
        fail "PermanentMACAddress=$perm did not match $nic"
    fi
fi

echo "== 4. the shipped link files use the selector step 2 tested"
here="$(cd "$(dirname "$0")" && pwd)"
if ! grep -q 'PermanentMACAddress=%s' "$here/../../tierd/internal/network/linkfile.go"; then
    fail "tierd's GenerateLinkFile no longer writes PermanentMACAddress="
fi
if ! grep -qx '+PermanentMACAddress=$mac' "$here/../../iso/smoothiso-patches/0004-match-nic-links-on-permanent-mac.patch"; then
    fail "the installer patch no longer writes PermanentMACAddress="
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures failure(s)" >&2
    exit 1
fi
echo "link-bond-rename: all checks passed"
