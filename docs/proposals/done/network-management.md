# Proposal: Network Management

**Status:** Completed
**Date:** 2026-03-29
**Depends on:** mdadm-storage-tiering (the base appliance must exist first)

---

## Problem

A bare-metal storage appliance needs network configuration to function, and misconfiguration can make the appliance unreachable. Currently the storage tiering proposal assumes a working network but provides no way to manage it through the web UI. Administrators would need to SSH in and hand-edit config files, which defeats the purpose of a managed appliance. The appliance needs a web UI for interface configuration, bonding, VLANs, and routing, with safety mechanisms to prevent lockouts.

---

## Goals

1. Configure network interfaces (static IPv4/IPv6, DHCP, MTU) through the web UI
2. Bond/LACP link aggregation for throughput and failover
3. VLAN tagging for network segmentation (storage traffic isolation)
4. DNS, hostname, and route management
5. Safe-apply pattern to prevent network lockouts

---

## Non-goals

- Bridge interfaces. This is a storage appliance, not a hypervisor.
- Wireless networking. Bare-metal appliance for datacenter/homelab use.
- Advanced firewall management beyond protocol port rules (handled by the sharing-protocols proposal).
- Dynamic routing protocols (OSPF, BGP).

---

## Architecture

### Backend: systemd-networkd

`tierd` manages network configuration through systemd-networkd, which is well-suited for a headless appliance:

- No NetworkManager overhead or D-Bus dependency
- Native support for bonding, VLANs, and static routes
- Configuration is declarative `.network`, `.netdev`, and `.link` files in `/etc/systemd/network/`
- Changes apply via `networkctl reload` (no full restart required)

`tierd` owns all files in `/etc/systemd/network/`. It generates them from its internal state (SQLite) and writes them atomically. Manual edits to these files will be overwritten.

### Configuration model

```
Physical Interface (eth0, eth1, ...)
  ├── Standalone: IP config directly on the interface
  ├── Bond member: no IP config, belongs to a bond
  └── Unused: link down, no config

Bond (bond0, bond1, ...)
  ├── Members: one or more physical interfaces
  ├── Mode: 802.3ad (LACP), balance-rr, active-backup, etc.
  └── IP config on the bond interface

VLAN (vlan.100, vlan.200, ...)
  ├── Parent: a physical interface or bond
  ├── VLAN ID: 1-4094
  └── IP config on the VLAN interface
```

Any configurable interface (standalone physical, bond, or VLAN) can have:
- Zero or more IPv4 addresses (static or DHCP)
- Zero or more IPv6 addresses (static, SLAAC, or DHCPv6)
- MTU (default 1500, up to 9000 for jumbo frames)
- Gateway and static routes

### Safe-apply pattern

Network changes can lock the administrator out of the appliance. To prevent this, all network changes follow a test-and-confirm workflow:

1. **Preview:** The UI shows the pending configuration diff before applying.
2. **Apply with timeout:** `tierd` writes the new config files, reloads networkd, and starts a 90-second countdown timer.
3. **Confirm:** The user must confirm from the web UI within 90 seconds. The confirmation request is served on both the old and new IPs (if the IP changed) so the user can reach it regardless.
4. **Revert on timeout:** If the user does not confirm within 90 seconds, `tierd` restores the previous config files and reloads networkd. The appliance returns to its last-known-good configuration.

The countdown timer and confirm/revert logic runs in `tierd` itself, not in a separate service, so it survives even if the web UI connection drops.

**Exception:** DNS and hostname changes do not trigger the safe-apply countdown since they cannot cause a network lockout. They apply immediately.

### File generation

`tierd` generates systemd-networkd files from its database. Examples:

**Standalone interface with static IPv4 and IPv6:**
```ini
# /etc/systemd/network/10-eth0.network
[Match]
Name=eth0

[Network]
Address=192.168.1.50/24
Gateway=192.168.1.1
Address=fd00::50/64
Gateway=fd00::1
DNS=192.168.1.1

[Link]
MTUBytes=1500
```

**Bond with LACP:**
```ini
# /etc/systemd/network/05-bond0.netdev
[NetDev]
Name=bond0
Kind=bond

[Bond]
Mode=802.3ad
TransmitHashPolicy=layer3+4
MIIMonitorSec=100ms
LACPTransmitRate=fast
```

```ini
# /etc/systemd/network/10-eth0.network
[Match]
Name=eth0

[Network]
Bond=bond0
```

```ini
# /etc/systemd/network/10-bond0.network
[Match]
Name=bond0

[Network]
Address=10.0.0.10/24
Gateway=10.0.0.1
```

**VLAN on a bond:**
```ini
# /etc/systemd/network/05-vlan100.netdev
[NetDev]
Name=bond0.100
Kind=vlan

[VLAN]
Id=100
```

```ini
# /etc/systemd/network/10-bond0.100.network
[Match]
Name=bond0.100

[Network]
Address=10.100.0.10/24

[Link]
MTUBytes=9000
```

---

## API

### Interfaces

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/network/interfaces` | GET | List all physical interfaces with link state, MAC, speed, MTU, current IP(s), and assignment (standalone, bond member, unused) |
| `/api/network/interfaces/{name}` | GET | Detailed interface status: link state, counters, driver, firmware |
| `/api/network/interfaces/{name}` | PUT | Configure a standalone interface (addresses, gateway, MTU, DHCP/static). Triggers safe-apply. |
| `/api/network/interfaces/{name}/identify` | POST | Blink the interface LED for physical identification |

### Bonds

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/network/bonds` | GET | List all bonds with mode, members, link state, IP(s) |
| `/api/network/bonds` | POST | Create a bond (name, mode, member interfaces, IP config, MTU). Triggers safe-apply. |
| `/api/network/bonds/{name}` | PUT | Update bond settings (add/remove members, change mode, update IP). Triggers safe-apply. |
| `/api/network/bonds/{name}` | DELETE | Destroy bond, release member interfaces. Triggers safe-apply. |

Supported bond modes:

| Mode | Name | Description |
|------|------|-------------|
| `802.3ad` | LACP | Link Aggregation Control Protocol. Requires switch support. |
| `balance-rr` | Round-robin | Packets distributed sequentially across members. |
| `active-backup` | Active-backup | One active member, others on standby. No switch config needed. |
| `balance-xor` | XOR | Hash-based distribution. |
| `balance-tlb` | Adaptive transmit | Outgoing traffic distributed by load, incoming on one member. |
| `balance-alb` | Adaptive load | Both transmit and receive load balancing. |

### VLANs

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/network/vlans` | GET | List all VLANs with ID, parent interface, IP(s) |
| `/api/network/vlans` | POST | Create a VLAN (parent interface or bond, VLAN ID, IP config, MTU). Triggers safe-apply. |
| `/api/network/vlans/{name}` | PUT | Update VLAN settings (IP, MTU). Triggers safe-apply. |
| `/api/network/vlans/{name}` | DELETE | Destroy VLAN. Triggers safe-apply. |

### DNS and hostname

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/network/dns` | GET | Current DNS servers and search domains |
| `/api/network/dns` | PUT | Update DNS servers and search domains. Applies immediately (no safe-apply). |
| `/api/network/hostname` | GET | Current hostname |
| `/api/network/hostname` | PUT | Update hostname. Applies immediately (no safe-apply). |

### Routes

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/network/routes` | GET | List all static routes (excluding auto-generated interface routes) |
| `/api/network/routes` | POST | Add a static route (destination CIDR, gateway, interface, metric). Triggers safe-apply. |
| `/api/network/routes/{id}` | DELETE | Remove a static route. Triggers safe-apply. |

### Safe-apply control

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/network/pending` | GET | Current pending change: diff, countdown remaining, or null if none |
| `/api/network/pending/confirm` | POST | Confirm the pending change, making it permanent |
| `/api/network/pending/revert` | POST | Immediately revert the pending change (don't wait for timeout) |

---

## UI

### New pages

| Page | Description |
|------|-------------|
| **Network** | Overview: all interfaces (physical, bonds, VLANs) with link state, IPs, and throughput sparklines. DNS and hostname at the top. |
| **Interface detail** | Configure a standalone interface: addresses, gateway, MTU, DHCP toggle. Link stats and counters. |
| **Bonds** | Create and manage bonds: select members, choose mode, configure IP. |
| **VLANs** | Create and manage VLANs: select parent, set ID, configure IP and MTU. |
| **Routes** | View and manage static routes. |

### Safe-apply UX

When a network change triggers safe-apply:

1. A full-screen overlay appears with a 90-second countdown
2. The overlay shows the configuration diff (old vs. new)
3. Two buttons: **Confirm** (keep changes) and **Revert Now** (roll back immediately)
4. If the page becomes unreachable (IP changed), the user navigates to the new IP; the overlay appears there too
5. If the countdown expires without confirmation, the appliance reverts and the overlay disappears on reconnect

### Dashboard integration

The existing Dashboard page gains a network health summary: link states, bond status, and an alert if any interface is down or a bond is degraded (fewer members than configured).

---

## IPv6 Support

All IP configuration supports both IPv4 and IPv6. Per interface (standalone, bond, or VLAN):

| Setting | IPv4 | IPv6 |
|---------|------|------|
| Static address | Manual CIDR | Manual CIDR |
| Automatic | DHCP | SLAAC, DHCPv6, or both |
| Gateway | Single default gateway | Single default gateway |
| Multiple addresses | Yes | Yes |

The UI presents IPv4 and IPv6 as parallel sections within each interface's configuration, not as separate pages. Both can be configured simultaneously.

systemd-networkd handles dual-stack natively. Example with both:

```ini
[Network]
Address=192.168.1.50/24
Gateway=192.168.1.1
Address=2001:db8::50/64
Gateway=2001:db8::1
IPv6AcceptRA=false
```

When SLAAC is enabled, `IPv6AcceptRA=true` is set and no static IPv6 address is required.

---

## Trade-offs

**systemd-networkd vs. NetworkManager.** networkd is lighter, has no GUI dependency, and its file-based config is easy to generate and atomic-swap. NetworkManager is more common on desktops but adds D-Bus complexity and is heavier than needed for a headless appliance. networkd is the right tool for this use case.

**90-second safe-apply timeout.** Long enough to re-navigate if the IP changed, short enough to not leave the appliance in a broken state for long. This is the same approach enterprise switches use for configuration changes. The timeout is not configurable to keep the safety guarantee simple.

**No bridge interfaces.** Bridges are for VM networking. This appliance serves storage, not compute. Adding bridge support would complicate the model for no benefit. If bridge support is needed later, it fits cleanly into the same systemd-networkd file generation pattern.

**No wireless.** A storage appliance on WiFi is not a serious configuration. The latency, throughput, and reliability characteristics are incompatible with iSCSI, NFS, and SMB workloads.

**Jumbo frames opt-in, not default.** MTU 9000 improves throughput for large transfers but requires every device on the L2 segment to support it. Defaulting to 1500 avoids silent packet drops. The UI surfaces MTU prominently so users can enable jumbo frames where appropriate (especially on dedicated storage VLANs).
