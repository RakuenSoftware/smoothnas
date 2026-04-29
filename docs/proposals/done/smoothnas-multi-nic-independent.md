# Proposal: Default-Bond Network Model + MPIO + Network GUI Polish

**Status:** Done — all eight phases delivered.
**Sub-phases:**
- [`multi-nic-01-default-bond-policy.md`](multi-nic-01-default-bond-policy.md) (#329) — backend default-bond policy at tierd startup
- [`multi-nic-02-network-page-cards.md`](multi-nic-02-network-page-cards.md) (#330) — Network page card layout (read-only)
- [`multi-nic-03-edit-ip-form.md`](multi-nic-03-edit-ip-form.md) (#331) — Edit-IP form (DHCP-or-static)
- [`multi-nic-04-bond-mode-change.md`](multi-nic-04-bond-mode-change.md) (#332) — Bond mode change
- [`multi-nic-05-break-bond.md`](multi-nic-05-break-bond.md) (#333) — Break Bond / Re-create Bond
- [`multi-nic-06-multi-flow-status.md`](multi-nic-06-multi-flow-status.md) (#334) — Multi-flow status + SMB Multichannel
- [`multi-nic-07-per-nic-stats.md`](multi-nic-07-per-nic-stats.md) (#335) — per-NIC stats drill-down
- [`multi-nic-08-vlan-route-forms.md`](multi-nic-08-vlan-route-forms.md) — VLAN form + static route polish

---

## 1. Problem

Today the SmoothNAS appliance treats its physical NICs as a flat
list of `InterfaceConfig` records. Each NIC can be configured via
the existing `/api/network/interfaces` REST surface, and a
separate `/api/network/bonds` surface lets an operator combine
NICs with `BondConfig`. There's no opinionated default, the
Network page is a thin form over those records, and the protocol
layer (SMB / NFS / iSCSI) is silent about multi-NIC topologies.

This proposal pins the appliance default to:

- **One IP.** A box with N NICs presents one IP on the LAN. DNS
  resolves to one address; cabling and link state are an
  implementation detail to clients.
- **Per-stream per-NIC service.** That single IP sits on a bond
  in `balance-alb` mode across every physical Ethernet. Each
  inbound TCP stream is pinned to one NIC by the kernel's per-
  peer ARP load balancing. A box with 4× 2.5 GbE NICs can serve
  up to 4 streams at full 2.5 GbE each (one per NIC); the
  appliance does NOT try to aggregate a single stream across
  NICs.
- **MPIO supported by default at the protocol layer.** SMB
  Multichannel, NFSv4.1 session-trunking, and iSCSI multipath
  are enabled out of the box, so a client that wants
  multi-path I/O can use it the moment the network topology
  exposes multiple paths.
- **Bond DHCP or static; per-NIC DHCP or static after Break.**
  The bond defaults to DHCP. Operators can switch the bond to
  a static IP via the GUI, OR break the bond entirely; in the
  broken-bond shape each NIC is independently configurable as
  DHCP or static. The IP-mode choice is orthogonal to the
  topology choice.
- **A real Network GUI.** The page surfaces the bond + members,
  link state per NIC, per-NIC throughput / connection counts,
  one-click DHCP-or-static on either the bond or per-NIC,
  bond-mode swap, break-bond with reversible per-member
  restore, VLAN create, static-route create.

## 2. Non-goals

- **Aggregating a single TCP stream across multiple NICs without
  switch help.** balance-alb does not do this; LACP requires
  switch config; balance-rr causes TCP reordering. Single-stream
  aggregation stays an explicit opt-in path the operator chooses
  when their switch supports LACP.
- **Software-defined-network / overlay tunnelling.** No VXLAN /
  WireGuard mesh / multi-tenant routing.
- **Replacing the existing `tierd/internal/network` Go package.**
  `BondConfig`, `InterfaceConfig`, `VLANConfig`, `safeapply`,
  the validators, and the systemd-networkd file generators all
  stay; this proposal layers on top.
- **NIC firmware updates.** Surfaced as a link-capability hint,
  not as an action.
- **Wi-Fi.** SmoothNAS is a wired appliance.

## 3. Default network policy

When tierd starts on a box where no network config has been
recorded (fresh install, factory reset), the default is:

1. Enumerate every physical Ethernet NIC by udev (`ID_NET_NAME_PATH`,
   filtering out tunnels, virtuals, wireless).
2. Auto-create `bond0` containing every enumerated NIC as a
   member, mode `balance-alb`, miimon 100 ms.
3. The bond is brought up and DHCPed (`DHCP=ipv4`); the leased
   address is the appliance's single LAN IP.
4. Every NIC, connected or not, is a member of `bond0` from the
   moment it appears in udev. A subsequent cable plug-in is
   picked up by the bond's miimon link-state probe and starts
   carrying traffic without any operator action.
5. The recorded config in tierd's SQLite reflects "default-bond
   policy" so a config-aware reconcile (e.g., after an upgrade)
   re-creates the bond if `bond0` is missing.

The behaviour this default delivers:

- **Parallel-client throughput**: balance-alb's per-peer ARP
  pinning routes each peer's traffic to a single NIC and
  distributes peers across NICs. 4 parallel clients on a 4-NIC
  box land on different NICs and each see full 2.5 GbE.
- **Single-stream rate**: a single TCP connection is pinned to
  one NIC and capped at that NIC's line rate. The appliance
  does not try to spread one stream across NICs (the user-
  visible knob to do that is "switch to mode `802.3ad` and let
  the switch's LACP hash spread by 5-tuple").
- **Failover**: if a NIC's link drops, balance-alb migrates the
  pinned peers to a still-up NIC; the bond IP doesn't move.
- **Cabling-order independence**: the management IP is on the
  bond, so plugging cables in different order across reboots
  doesn't shuffle which NIC carries the management web UI.

If the operator records explicit config (break the bond, change
mode, set static IP), that config wins. The default policy only
applies to fresh state.

## 4. IP-mode matrix

Topology and IP-mode are orthogonal. Both axes are first-class:

| Topology         | IP mode  | Result                                       |
|------------------|----------|----------------------------------------------|
| `bond0` (default)| DHCP     | One DHCP-leased IP on the bond. (OOB.)       |
| `bond0`          | Static   | One operator-set static IP on the bond.      |
| broken (per-NIC) | DHCP     | Each NIC gets its own DHCP lease.            |
| broken (per-NIC) | Static   | Each NIC gets its own operator-set static.   |
| broken (mixed)   | per NIC  | Each NIC independently DHCP or static.       |

Operator paths through the matrix:

- **Bond → static**. "Edit IP…" on the bond row opens a form
  (DHCP toggle, CIDR, gateway, DNS, MTU); submit hits the
  safe-apply flow and the bond's `Network=` file flips from
  `DHCP=ipv4` to static.
- **Bond → DHCP**. Same form, "DHCP" toggle.
- **Break Bond**. Drops the bond, writes per-NIC
  `InterfaceConfig` records. Each member defaults to DHCP
  unless a previously-saved per-member config exists, in which
  case the saved config is restored.
- **Per-NIC → static / DHCP**. After break, the per-NIC row's
  "Edit IP…" opens the same DHCP-or-static form, scoped to
  that NIC.
- **Re-create Bond**. Restores the default-bond policy across
  all physical NICs and drops the per-NIC IPs. The bond's IP
  mode (DHCP or static) is whatever the operator picks at
  re-create time, defaulting to DHCP for symmetry with the
  fresh-install case.

The same form / safe-apply / pending-confirm wiring services
both the bond row and per-NIC rows. There's no separate
"static-IP page" for the bond vs. per-NIC case — it's the same
component scoped by which row was edited.

## 5. MPIO at the protocol layer

The bond default exposes one IP, so a client out of the box
sees a single path. To support multi-path I/O without forcing
the operator to break the bond first, the protocol layer is
opted in by default:

### 5.1 SMB Multichannel

`smb.conf` is generated with:

```
[global]
    server multi channel support = yes
    interfaces = <bond0_ip>;capability=RSS,speed=<bond_speed>
    bind interfaces only = yes
```

When the operator breaks the bond into independent NICs, the
`interfaces=` line is regenerated to advertise every per-NIC IP
with that NIC's link speed; clients learn the additional IPs
from the SMB Negotiate response and open a TCP channel to each.

When the operator switches to `802.3ad` LACP, the bond stays
single-IP; multichannel is still on but adds no extra channels
because there's still one server IP.

### 5.2 NFSv4.1 session-trunking

`nfsd` listens on every recorded IP. The default-bond shape
puts that on the single bond IP; broken-bond shape gives one
listener per NIC IP. NFSv4.1 clients with `mount -o
trunkdiscovery,nconnect=N` discover the additional IPs (when
present) and open multiple TCP connections per session.

### 5.3 iSCSI multipath

Each iSCSI target gets a portal on every recorded IP. Default-
bond → one portal; broken-bond → N portals; LACP-bond → still
one. The client open-iscsi initiator's MPIO discovers the
portals and opens sessions per portal.

### 5.4 Operator path to "MPIO actually multi-paths"

The default bond does not give MPIO multi-path because there's
one server IP. To get genuine multi-path:

- **Break the bond** via the GUI. tierd writes per-NIC
  `InterfaceConfig` records (DHCP each NIC unless a saved
  static config is restored), regenerates `smb.conf`, restarts
  smbd, re-publishes iSCSI portals on the new IPs. The
  client's MPIO/multichannel/trunkdiscovery now sees N paths
  and uses them.
- Or **switch to LACP** (`802.3ad`) if the upstream switch
  supports it; single-stream aggregation comes from the
  switch's hash policy rather than from MPIO at the protocol
  layer.

Both are explicit operator choices. The default is the
single-IP / per-stream-per-NIC shape.

## 6. Network GUI rewrite

Card-based layout that surfaces the topology end-to-end.
Default-bond shape:

```
┌─ System ─────────────────────────────────────────────┐
│ Hostname:  smoothnas-01           [Edit]              │
│ DNS:       1.1.1.1, 1.0.0.1       [Edit]              │
│ Default route: 192.168.1.1                            │
└──────────────────────────────────────────────────────┘

┌─ Active topology: bond0 (balance-alb) ───────────────┐
│ IP: 192.168.1.10/24 (DHCP)            [Edit IP…]      │
│ Mode: balance-alb                     [Change Mode…]  │
│                                       [Break Bond]    │
│                                                        │
│ Members:                                               │
│  enp1s0  up    2.5 GbE   active   12 active conns     │
│  enp2s0  up    2.5 GbE   active    8 active conns     │
│  enp3s0  down    —       inactive  —                  │
│  enp4s0  up    2.5 GbE   active   15 active conns     │
└──────────────────────────────────────────────────────┘

┌─ VLANs ──────────────────────────────────────────────┐
│ Name        Parent     VID    IP                       │
│ vlan100     bond0      100    10.0.100.5/24            │
│                                            [Add VLAN] │
└──────────────────────────────────────────────────────┘

┌─ Multi-flow status ──────────────────────────────────┐
│ SMB Multichannel:   enabled  (advertising 1 path)     │
│ NFS multi-path:     listening on 1 IP                 │
│ iSCSI portals:      1 per target                      │
│                                                        │
│ ⓘ With the default bond, clients see one path. Break  │
│   the bond or switch to per-NIC IPs to expose         │
│   multiple paths to MPIO-aware clients.               │
└──────────────────────────────────────────────────────┘
```

After **Break Bond** the layout flips:

```
┌─ Active topology: independent NICs ──────────────────────────┐
│  enp1s0  up    2.5 GbE   192.168.1.10/24 DHCP    [Edit IP…]   │
│  enp2s0  up    2.5 GbE   192.168.1.11/24 Static  [Edit IP…]   │
│  enp3s0  down    —       —                        [Edit IP…]   │
│  enp4s0  up    2.5 GbE   192.168.1.13/24 DHCP    [Edit IP…]   │
│                                                                 │
│ [Re-create Bond] (restores balance-alb default)                │
└───────────────────────────────────────────────────────────────┘
```

The "Edit IP…" form is the same component on the bond row and
the per-NIC rows: DHCP toggle, IPv4 CIDR + gateway, IPv6 CIDR +
gateway, MTU, DNS overrides; safe-apply / pending-confirm wired
through `tierd/internal/network/safeapply.go`.

Per-row drill-down (a "Stats" affordance on each NIC):

- Real-time RX / TX throughput (`/proc/net/dev` deltas, sampled
  every 2 s).
- Established TCP connection count (`ss -tH src <ip>`).
- Link speed, duplex, MTU, driver from `ethtool`.

## 7. Acceptance criteria

### 7.1 Default bond policy

- [ ] On a fresh install with N ≥ 1 physical Ethernet NICs,
      tierd auto-creates `bond0` with mode `balance-alb` and
      every enumerated NIC as a member; the bond is DHCP'd.
- [ ] On a 4-NIC box with the default policy, four parallel
      `iperf3` streams from four distinct clients show roughly
      4× line-rate aggregate, with each stream pinned to one
      NIC (verified via `/proc/net/dev` deltas).
- [ ] On the same box, a single `iperf3` stream caps at one
      NIC's line rate.
- [ ] Plugging a previously-unconnected NIC after first boot
      results in it joining `bond0` automatically and starting
      to carry traffic within one miimon cycle.
- [ ] If a NIC's link drops, balance-alb migrates pinned peers
      to a still-up member; the bond IP does not move; pre-
      existing TCP connections to a peer that was on the
      dropped NIC may reset (acceptable; documented).

### 7.2 IP-mode matrix

- [ ] The bond can be edited from DHCP to static and back via
      the GUI; safe-apply / pending-confirm wraps the change.
- [ ] After Break Bond, each per-NIC row's Edit IP supports
      DHCP and static independently.
- [ ] A box can run with mixed per-NIC IP modes (some DHCP,
      some static) under broken-bond.
- [ ] Re-create Bond drops the per-NIC IPs and offers a fresh
      DHCP-or-static choice for the bond, defaulting to DHCP.
- [ ] Validation rejects invalid CIDR / gateway-not-in-subnet
      / MTU out of range via the existing `Validate*` helpers.

### 7.3 MPIO protocol enablement

- [ ] `smb.conf` defaults `server multi channel support = yes`
      and emits a live `interfaces = ...` line generated from
      the active IP set (default-bond → one IP; broken-bond →
      N IPs).
- [ ] When the active IP set changes, smbd is reloaded so the
      `interfaces=` line takes effect without an operator
      action.
- [ ] `nfsd` listens on every IP in the active IP set.
- [ ] iSCSI targets publish a portal on every IP in the active
      IP set; the iSCSI page on the GUI shows the portal count.
- [ ] The Network page's "Multi-flow status" card reflects the
      live state of all three: enabled / disabled, count of
      paths advertised, and a hint when the bond is masking
      multiple NICs behind one IP.

### 7.4 Bond-mode change

- [ ] "Change Mode…" exposes every `BondConfig.Mode` value the
      validator accepts (`balance-rr`, `active-backup`,
      `balance-xor`, `802.3ad`, `balance-tlb`, `balance-alb`).
- [ ] Switching mode is a safe-apply: tierd writes the new
      mode and a pending-confirm window opens; the change is
      rolled back if the operator's session doesn't confirm
      inside the window.
- [ ] A static IP set on the bond survives a mode change (the
      mode swap rewrites the netdev file but leaves the
      network file intact).

### 7.5 Break Bond / Re-create Bond

- [ ] "Break Bond" deletes the bond's netdev + network files,
      drops the bond IP, and writes per-member `InterfaceConfig`
      records (DHCP each unless a previous static config was
      saved).
- [ ] "Re-create Bond" rebuilds the default-bond policy across
      all physical NICs, dropping the per-NIC IPs.
- [ ] Pre-bond per-member configs are persisted in tierd's
      SQLite (`network.bond.<name>.previous_member_config`) so
      Break Bond restores the operator's prior intent.

### 7.6 Per-NIC stats

- [ ] Per-NIC drill-down shows RX / TX throughput and
      established-connection count, refreshed every 2 s, with
      a query path that costs ≤ 10 ms per refresh on a
      4-NIC box.

### 7.7 VLAN / route forms

- [ ] "Add VLAN" form takes parent (bond or NIC), VID, IP
      config; writes via `VLANConfig`.
- [ ] Static-route create / delete reaches `RouteConfig`.

## 8. Rollout

Slice into reviewable PRs.

### Phase 1: backend default-bond policy + tests

- Add `network.DefaultBondPolicy()` that returns the
  `BondConfig` (mode `balance-alb`, all enumerated NICs).
- Wire it into the network reconcile path for fresh state.
- Persist a "default policy applied" marker so subsequent
  reconciles don't re-create the bond after an operator's
  Break Bond.
- Tests: cover "no records → default bond", "operator broke
  the bond → reconcile leaves it broken", "new NIC appears →
  joins existing bond".

### Phase 2: GUI restructure (read-only)

- Reshape the Network page to the four-card layout (System,
  Active topology, VLANs, Multi-flow status).
- Wire the existing endpoints; no edit affordances yet.

### Phase 3: Edit IP form (DHCP-or-static), bond and per-NIC

- One form component used by the bond row and the per-NIC
  rows.
- DHCP toggle, IPv4/IPv6 CIDR + gateway, MTU, DNS.
- Safe-apply integration.

### Phase 4: Bond mode change

- "Change Mode…" with the full mode dropdown + safe-apply.

### Phase 5: Break Bond / Re-create Bond

- Pre-member-config persistence.
- Break Bond action: per-member `InterfaceConfig` write,
  smb.conf regeneration, smbd reload, iSCSI portal
  re-publish.
- Re-create Bond action.

### Phase 6: SMB Multichannel + NFS multi-path + iSCSI portal fan-out

- `smb.conf` interfaces= generator.
- nfsd listen audit.
- iSCSI per-portal-IP fan-out.
- Multi-flow status card data.
- Reload-on-change wiring.

### Phase 7: per-NIC stats

- Throughput sampling.
- Connection-count via `ss -tH src <ip>`.
- Drill-down panel.

### Phase 8: VLAN form + static-route polish

- "Add VLAN" form.
- Static-route UI.

## 9. Risks

### 9.1 First-boot bond create vs DHCP race

If tierd creates `bond0` after systemd-networkd has already
started DHCP on individual NICs, both can hold leases briefly.
Mitigation: tierd's first-boot reconcile runs before
systemd-networkd's `network.target` is reached; the auto-bond
is written to `/etc/systemd/network/` before the per-NIC
configs and supersedes them.

### 9.2 Bond-member orphaning across upgrades

A tierd upgrade that re-runs reconcile must not re-create
`bond0` if the operator broke the bond. Mitigation: persist
the operator's Break Bond decision; reconcile honours it.

### 9.3 balance-alb on switches that don't expect MAC churn

balance-alb uses ARP rewriting; some old / strict managed
switches log noise or rate-limit. Mitigation: the GUI's
"Change Mode…" makes switching to `active-backup` (no MAC
churn) one click; operator runbook documents the symptom.

### 9.4 Single-stream rate ceiling

A user expecting one client to saturate 4× 2.5 GbE will not
get it from the default. Mitigation: the Multi-flow status
card explicitly says "single stream caps at one NIC; switch
to LACP or break the bond for multi-path", with deep links
to the relevant runbook section.

### 9.5 nfsd / smbd reload races

A NIC plug/unplug while smbd is mid-reload could race.
Mitigation: debounce reloads (≥ 5 s window); the Multi-flow
status card shows pending reloads.

## 10. Test plan

- **Unit:** `DefaultBondPolicy()` truth table; bond
  create/break round-trips through `safeapply`; IP-mode
  matrix (bond-DHCP, bond-static, broken-DHCP, broken-static,
  broken-mixed) round-trips; SMB `interfaces=` generator with
  default-bond, broken-bond, and LACP-bond topologies; VLAN-
  on-bond vs VLAN-on-NIC.
- **Integration:** Network page Playwright flow — confirm
  default-bond shape on first load, edit bond IP (DHCP →
  static → DHCP), change mode, break bond, edit per-NIC IP
  (DHCP → static), re-create bond, add VLAN.
- **Live (on the test rig with 4 NICs):**
  - Start with default bond. Run 4 parallel `iperf3` streams
    from 4 clients; each pins to a distinct NIC; aggregate
    ≈ 4× line rate.
  - Single-client `iperf3` ≈ 1× line rate.
  - Pull a member NIC's cable; existing peers on other NICs
    keep flowing; peers that were on the pulled NIC migrate
    within one miimon cycle.
  - Set the bond to a static IP; reboot; confirm the IP
    survives.
  - Break the bond. Confirm 4 NIC IPs appear, smb.conf
    advertises all 4, iSCSI publishes 4 portals per target.
  - Set one of the per-NIC IPs to static; reboot; confirm
    the per-NIC static survives and the others stay DHCP.
  - Mount NFSv4.1 with `nconnect=4 -o trunkdiscovery` from
    one client; assert 4 TCP connections (`ss`) land on 4
    distinct NIC IPs.

## 11. Out-of-scope follow-ups

- Per-flow QoS, traffic shaping, bandwidth caps.
- Wi-Fi support.
- Multi-tenant network isolation (separate routing tables
  per tenant).
- WireGuard / VPN endpoints.
- VRRP / keepalived virtual-IP failover across boxes (cluster
  feature, separate proposal).
