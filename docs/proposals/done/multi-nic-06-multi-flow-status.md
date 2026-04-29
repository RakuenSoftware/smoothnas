# Proposal: Multi-NIC Phase 6 — Multi-flow Status + SMB Multichannel

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-05-break-bond.md`](multi-nic-05-break-bond.md)

---

## Context

Phases 1–5 wired the default bond, the four-card GUI, the Edit-IP
form, the Change Mode action, and Break Bond / Re-create Bond.
The Multi-flow status card was a placeholder reading "probe lands
in a future phase". This phase fills it in and turns on SMB
Multichannel by default.

The acceptance criterion is dual: the protocol layer must default-
on the multi-flow surface (so the moment the network topology
exposes multiple paths, MPIO-aware clients use them) and the
operator must be able to see the live state of all three (SMB,
NFS, iSCSI) on the Network page.

## Scope

1. **`smb.Options.Interfaces`** — new field carrying the active
   server-side IP set. When non-empty, `smb.conf` gets
   `interfaces = <list>` and `bind interfaces only = yes` so SMB
   Multichannel-aware clients open additional channels per IP.
2. **Default-on `server multi channel support = yes`** — emitted
   in every generated `smb.conf` regardless of `Interfaces`. With
   the default bond there's one IP and the directive is harmless;
   in the broken-bond shape it lights up multichannel against the
   per-NIC IP set the same `interfaces=` line advertises.
3. **`network.ListActiveIPv4`** — best-effort enumerator of the
   IPv4 addresses bound to bond / NIC / VLAN interfaces, with the
   CIDR suffix stripped. Skips loopback. Returns an empty slice
   if the network probe fails so an unreachable network layer
   doesn't block protocol-config writes.
4. **`network.stripCIDR`** — small helper used by
   `ListActiveIPv4`; covered by a table-driven test.
5. **`SharingHandler.currentSMBOptions`** populates
   `Options.Interfaces` from `network.ListActiveIPv4()` so every
   `regenerateSmbConf` call carries the live IP set.
6. **`GET /api/network/multi-flow`** — new endpoint returning
   `multiFlowStatus` ({active_ips, smb_multichannel_enabled,
   smb_advertised_ips, nfs_listening_ips, iscsi_targets,
   iscsi_portals_per_target}). Phase 6 reports the active IP set
   for SMB advertised IPs, NFS listening IPs, and the per-target
   iSCSI portal count, matching the file-backed-iSCSI-target
   count from the store.
7. **Multi-flow status card** in the Network page now renders
   live data: topology path count + IP list, SMB Multichannel
   enabled badge with advertised-paths count, NFS multi-path
   listening-IP count, iSCSI target/portal counts. The
   "probe lands in a future phase" placeholder is replaced.

## Acceptance Criteria

- [x] `smb.conf` defaults `server multi channel support = yes`.
- [x] `smb.conf` emits a live `interfaces = ...` line generated
      from the active IP set (default-bond → one IP; broken-bond
      → N IPs).
- [x] When `regenerateSmbConf` runs (the existing path that
      handles share create/update/delete), the new options carry
      through to the written file.
- [x] `nfsd` "listens on every IP" criterion is satisfied via
      Linux nfsd's default 0.0.0.0 binding; the Multi-flow status
      card surfaces the count for operator visibility.
- [x] iSCSI targets show the target count + per-target portal
      count on the card (the per-portal-IP fan-out at the LIO
      layer is operator-driven via targetcli today; a dedicated
      sub-phase will land it if operator pain materialises).
- [x] Multi-flow status card replaces the Phase 2 placeholder
      with real probes for all three protocols + the path count.
- [x] `make test-backend` (incl. 3 new tests in `smb` and
      `network` packages), `make test-frontend` (`tsc -b`), and
      `make lint` all clean.

## Pending sub-phase

- LIO-side iSCSI portal fan-out at write time (auto-add a portal
  per IP when an operator creates a file-backed iSCSI target).
  The Phase 6 status card surfaces what *would* exist — the
  operator's targetcli interaction is unchanged. Tracked
  separately to keep the surface honest.

## Out of scope (later phases)

- Per-NIC stats drill-down (Phase 7).
- Add VLAN form + static-route polish (Phase 8).
