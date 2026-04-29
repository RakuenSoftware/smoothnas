# Proposal: Multi-NIC Phase 7 — Per-NIC Stats Drill-Down

**Status:** Done
**Split from:** [`smoothnas-multi-nic-independent.md`](smoothnas-multi-nic-independent.md)
**Predecessor:** [`multi-nic-06-multi-flow-status.md`](multi-nic-06-multi-flow-status.md)

---

## Context

Phases 1-6 wired the topology, the editable cards, and the Multi-
flow status summary. The proposal calls for a per-NIC drill-down
that lets an operator confirm load is actually being spread across
NICs while a backup or copy is running. This phase adds the Stats
button + polling sub-row that surfaces RX / TX rate and the
established TCP connection count, refreshed every 2 s.

## Scope

1. **`network.InterfaceStats`** — pure-Go struct mirroring the
   `/proc/net/dev` columns (RX/TX bytes/packets/errs/drop) plus
   the established-connection count.
2. **`readProcNetDev(content)`** — parses `/proc/net/dev` content
   into a name-keyed map. Pure parser so the test suite drives
   synthetic input; covered with a sample frame, a header-only
   frame, and a colonless line.
3. **`readEstablishedConnsForIPs(ips)`** — runs
   `ss -tH state established` and counts established TCP
   connections whose local-address column matches any of the
   given IPs. Returns 0 on any error so a missing `ss` doesn't
   break the stats endpoint.
4. **`network.GetInterfaceStats(name)`** — reads
   `/proc/net/dev`, finds the named interface, resolves the
   interface's IPs via `ListInterfaces`, and overlays the
   established-connection count. ≤ 10 ms on a 4-NIC box per the
   proposal's budget.
5. **`GET /api/network/interfaces/{name}/stats`** — new API route
   wrapping `GetInterfaceStats`.
6. **Frontend Stats button** on every per-NIC row (independent
   NICs view) and on every bond-member row (default-bond view).
   Toggling expands a sub-row that polls the endpoint every 2 s.
7. **Frontend rate computation** — the polling effect stores the
   last sample in a `useRef` and computes `(bytes_now -
   bytes_prev) / dt` for both RX and TX. Server is stateless on
   rate computation.
8. **Drill-down panel** renders RX / TX rate (KB/s, MB/s, GB/s
   auto-scaling), established-connection count, and the
   cumulative since-boot counters for diagnostic depth.

## Acceptance Criteria

- [x] Per-NIC drill-down shows RX / TX throughput and
      established-connection count.
- [x] Refreshed every 2 s while the panel is open; the polling
      stops when the operator collapses the panel or navigates
      away.
- [x] Stats query is cheap enough to leave open during a backup
      run: per-call cost is one `os.ReadFile` of `/proc/net/dev`
      (~ 1 ms) plus one `ss -tH` invocation (~ 2-5 ms on a busy
      box). Well under the proposal's 10 ms-per-refresh budget.
- [x] Stats button appears on both the broken-bond per-NIC rows
      and the default-bond bond-member rows so the operator can
      confirm load distribution in either topology.
- [x] `make test-backend`, `make test-frontend` (`tsc -b`), and
      `make lint` are clean.

## Out of scope (later phases)

- Add VLAN form + static-route polish (Phase 8).

## Notes

The existing IPv4 / IPv6 enumerator (`ListInterfaces`) is used to
scope the established-conn match — the same logic Phase 6's
`ListActiveIPv4` builds on. If a NIC has no IP, the connection
count is 0 (correct: nothing's terminated on it).
