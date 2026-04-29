# Proposal: Multi-NIC Phase 1 — Default-Bond Policy + Bootstrap Marker

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)

---

## Context

Phase 1 of the multi-NIC default-bond proposal. Lays the backend
foundation for the OOB shape: a single `bond0` over every physical
Ethernet NIC in `balance-alb` mode, DHCP. Subsequent phases add the
GUI cards, IP-mode form, mode-change UI, Break Bond, MPIO protocol
enablement, per-NIC stats, and VLAN/route forms.

## Scope

1. `network.IsPhysicalEthernet(sysRoot, name)` — sysfs-based filter
   that accepts real Ethernet NICs and rejects loopback / wireless /
   virtual / bond-self / bridge / VLAN-named / tunnel interfaces.
2. `network.EnumeratePhysicalEthernet(sysRoot)` — sorted list of
   physical Ethernet NIC names.
3. `network.DefaultBondPolicy(members)` — canonical default
   `BondConfig`: `bond0`, `balance-alb`, DHCPed.
4. `network.GenerateDefaultBondMembersNetwork()` — catch-all
   member-match `.network` file with wildcard `[Match]` so a
   newly-plugged NIC auto-joins the bond.
5. `network.ApplyDefaultBondPolicy(store, networkDir, sysRoot)` —
   bootstrap reconcile entry point. Writes
   `90-default-bond0.netdev`, `90-default-bond0.network`, and
   `99-default-bond-members.network`. Sets the
   `network.bootstrap_complete` config-store marker on success.
   No-op once the marker is set so an operator's later
   Break Bond / static-IP intent is preserved across tierd
   restarts.
6. Wired into tierd startup at
   `tierd/cmd/tierd/main.go` next to `ReconcileSharingConfig`.

## Acceptance Criteria

- [x] `IsPhysicalEthernet` accepts a real Ethernet NIC and rejects
      loopback, wireless, virtual / bond / bridge / docker / veth /
      tap / tun, dotted VLAN names, and non-Ethernet ARPHRD types
      (loopback 772, GRE 778).
- [x] `EnumeratePhysicalEthernet` returns the alphabetically-sorted
      names of physical Ethernet NICs found under sysRoot.
- [x] `DefaultBondPolicy` returns a `BondConfig` with name
      `bond0`, mode `balance-alb`, DHCP4 = true; the mode passes
      `ValidateBondMode`.
- [x] `GenerateDefaultBondMembersNetwork` emits `[Match]
      Type=ether Kind=!bond Name=!lo !bond* !virbr* …` plus
      `[Network] Bond=bond0`.
- [x] `ApplyDefaultBondPolicy` writes the three files on first
      run, sets the bootstrap marker, and is a no-op once the
      marker is set.
- [x] On a `networkDir` that doesn't exist (file write fails),
      the bootstrap marker is NOT set so a retry on a
      reachable dir applies cleanly.
- [x] `tierd/cmd/tierd/main.go` calls `ApplyDefaultBondPolicy`
      at startup; failure logs a warning rather than
      preventing tierd from starting.

## Out of scope (later phases)

- GUI restructure (Phase 2).
- Edit-IP form (Phase 3).
- Bond mode change (Phase 4).
- Break Bond / Re-create Bond (Phase 5).
- SMB Multichannel + NFS multi-path + iSCSI portal fan-out
  (Phase 6).
- Per-NIC stats (Phase 7).
- VLAN form + static-route polish (Phase 8).
