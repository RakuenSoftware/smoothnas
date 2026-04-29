import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

// Phase 2-3 of the multi-NIC default-bond proposal: four-card layout
// plus the Edit-IP form scoped equally to the bond row and per-NIC
// rows. Edit-IP submits via the existing safe-apply / pending-confirm
// flow already wired into PUT /api/network/interfaces/{name} (and
// PUT /api/network/bonds/{name} for the bond case).
//
// Cards:
//   1. System            — hostname, DNS, default route
//   2. Active topology   — the default bond + members (or, after
//                          Break Bond, the independent NICs)
//   3. VLANs             — operator-defined VLAN tags
//   4. Multi-flow status — placeholder for Phase 6 probes

type EditIPTarget = {
  kind: 'bond' | 'iface';
  name: string;
};

type EditIPForm = {
  dhcp: boolean;
  ipv4: string;     // CIDR, e.g. "192.168.1.10/24"
  gateway4: string;
  ipv6: string;     // CIDR
  gateway6: string;
  mtu: string;      // string in form, parsed at submit
  dns: string;      // comma-separated
};

function emptyEditIPForm(): EditIPForm {
  return { dhcp: true, ipv4: '', gateway4: '', ipv6: '', gateway6: '', mtu: '', dns: '' };
}

// Bond modes accepted by tierd's network.ValidateBondMode. Kept in
// sync manually with the validBondModes set in
// tierd/internal/network/bond.go — drift would surface as a 400 on
// submit, which the form pre-empts client-side anyway. balance-alb
// is the appliance default.
const BOND_MODES = [
  'balance-alb',    // default; per-peer ARP load balancing, no switch config
  'active-backup',  // simple failover, no MAC churn
  '802.3ad',        // LACP; needs switch support; gives single-flow aggregation
  'balance-rr',     // packet round-robin; risks TCP reordering
  'balance-xor',    // hash-based pinning per peer
  'balance-tlb',    // transmit load balancing
];

export default function Network() {
  const { t } = useI18n();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [interfaces, setInterfaces] = useState<any[]>([]);
  const [bonds, setBonds] = useState<any[]>([]);
  const [vlans, setVlans] = useState<any[]>([]);
  const [routes, setRoutes] = useState<any[]>([]);
  const [hostname, setHostname] = useState('');
  const [dns, setDns] = useState<any>({});
  const [pending, setPending] = useState<any>(null);
  const [multiFlow, setMultiFlow] = useState<any>(null);
  // Phase 7: per-NIC stats drill-down. statsOpen is the NIC name
  // currently expanded ('' when nothing is open). statsHistory
  // holds the last sample so we can compute throughput rates by
  // subtracting consecutive readings — keeps the server stateless
  // on the rate-computation path. statsLatest is what we render.
  const [statsOpen, setStatsOpen] = useState<string>('');
  const statsHistoryRef = useRef<{ t: number; sample: any } | null>(null);
  const [statsLatest, setStatsLatest] = useState<any>(null);
  const [statsRates, setStatsRates] = useState<{ rxBps: number; txBps: number } | null>(null);
  // Phase 8: VLAN form + static routes.
  const [showVlanForm, setShowVlanForm] = useState(false);
  const [vlanForm, setVlanForm] = useState({
    parent: '', id: '', dhcp: true, ipv4: '', gateway4: '',
  });
  const [showRouteForm, setShowRouteForm] = useState(false);
  const [routeForm, setRouteForm] = useState({
    destination: '', gateway: '', iface: '', metric: '',
  });
  const [editTarget, setEditTarget] = useState<EditIPTarget | null>(null);
  const [editForm, setEditForm] = useState<EditIPForm>(emptyEditIPForm());
  const [submitting, setSubmitting] = useState(false);
  // Phase 4: per-bond Change Mode modal. modeChangeBond holds the
  // bond name being edited (null when the modal is closed); modeChoice
  // holds the selected new mode while the modal is open.
  const [modeChangeBond, setModeChangeBond] = useState<string | null>(null);
  const [modeChoice, setModeChoice] = useState<string>(BOND_MODES[0]);

  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoading(true);
    let count = 7;
    const done = () => { if (--count <= 0) setLoading(false); };
    api.getInterfaces().then(i => { setInterfaces(i || []); done(); }).catch(done);
    api.getBonds().then(b => { setBonds(b || []); done(); }).catch(done);
    api.getVlans().then(v => { setVlans(v || []); done(); }).catch(done);
    api.getRoutes().then((r: any) => { setRoutes(r || []); done(); }).catch(done);
    api.getHostname().then((h: any) => { setHostname(h?.hostname || ''); done(); }).catch(done);
    api.getDns().then((d: any) => { setDns(d || {}); done(); }).catch(done);
    api.getMultiFlow().then((m: any) => { setMultiFlow(m); done(); }).catch(done);
    api.getPendingChange().then(setPending).catch(() => {});
  }

  function confirm() {
    api.confirmChange().then(() => { setPending(null); loadData(); })
      .catch(e => setError(extractError(e, t('network.error.confirm'))));
  }

  function revert() {
    api.revertChange().then(() => { setPending(null); loadData(); })
      .catch(e => setError(extractError(e, t('network.error.revert'))));
  }

  // ---- Edit-IP form -----------------------------------------------------

  function openEditIP(target: EditIPTarget) {
    const seed = target.kind === 'bond'
      ? bonds.find(b => b.name === target.name)
      : findInterface(target.name);
    setEditForm({
      dhcp: !!(seed?.dhcp4 || seed?.dhcp6),
      ipv4: (seed?.ipv4_addrs || [])[0] || '',
      gateway4: seed?.gateway4 || '',
      ipv6: (seed?.ipv6_addrs || [])[0] || '',
      gateway6: seed?.gateway6 || '',
      mtu: seed?.mtu ? String(seed.mtu) : '',
      dns: (seed?.dns || []).join(','),
    });
    setEditTarget(target);
    setError('');
  }

  function cancelEditIP() {
    setEditTarget(null);
    setEditForm(emptyEditIPForm());
  }

  // editFormError returns a short user-facing string when the form
  // values are invalid, or '' when submission can proceed. Mirrors the
  // backend validateIPConfig but keeps the UI error close to the input.
  function editFormError(): string {
    const f = editForm;
    if (f.dhcp) {
      // DHCP path: ignore static fields entirely.
      if (f.mtu) {
        const n = Number(f.mtu);
        if (Number.isNaN(n) || n < 576 || n > 9000) return t('network.validate.mtu');
      }
      return '';
    }
    if (!f.ipv4 && !f.ipv6) return t('network.validate.ipv4OrIpv6');
    if (f.ipv4 && !/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\/\d{1,2}$/.test(f.ipv4)) {
      return t('network.validate.ipv4Cidr');
    }
    if (f.gateway4 && !/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(f.gateway4)) {
      return t('network.validate.gateway4');
    }
    if (f.ipv6 && !(f.ipv6.includes(':') && f.ipv6.includes('/'))) {
      return t('network.validate.ipv6Cidr');
    }
    if (f.gateway6 && !f.gateway6.includes(':')) {
      return t('network.validate.gateway6');
    }
    if (f.mtu) {
      const n = Number(f.mtu);
      if (Number.isNaN(n) || n < 576 || n > 9000) return t('network.validate.mtu');
    }
    return '';
  }

  // ---- VLAN form (Phase 8) ----------------------------------------------

  function vlanParents(): string[] {
    const phys = physicalInterfaces().map(i => i.name);
    const bondNames = bonds.map(b => b.name);
    return [...bondNames, ...phys];
  }

  function vlanFormError(): string {
    const f = vlanForm;
    if (!f.parent) return t('network.validate.vlanParent');
    const id = Number(f.id);
    if (!Number.isInteger(id) || id < 1 || id > 4094) return t('network.validate.vlanId');
    if (!f.dhcp) {
      if (!f.ipv4) return t('network.validate.ipv4Required');
      if (!/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\/\d{1,2}$/.test(f.ipv4)) {
        return t('network.validate.vlanIpv4Cidr');
      }
      if (f.gateway4 && !/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(f.gateway4)) {
        return t('network.validate.gateway4Plain');
      }
    }
    return '';
  }

  function submitVlanForm() {
    const err = vlanFormError();
    if (err) { setError(err); return; }
    const f = vlanForm;
    const id = Number(f.id);
    const body: any = {
      parent: f.parent,
      id,
      dhcp4: f.dhcp,
      dhcp6: f.dhcp,
    };
    if (!f.dhcp) {
      if (f.ipv4) body.ipv4_addrs = [f.ipv4];
      if (f.gateway4) body.gateway4 = f.gateway4;
    }
    setSubmitting(true);
    setError('');
    api.createVlan(body)
      .then(() => {
        setShowVlanForm(false);
        setVlanForm({ parent: '', id: '', dhcp: true, ipv4: '', gateway4: '' });
        loadData();
      })
      .catch(e => setError(extractError(e, t('network.error.vlanCreate'))))
      .finally(() => setSubmitting(false));
  }

  function deleteVlan(name: string) {
    if (!window.confirm(t('network.confirm.deleteVlan', { name }))) return;
    setSubmitting(true);
    setError('');
    api.deleteVlan(name)
      .then(() => loadData())
      .catch(e => setError(extractError(e, t('network.error.vlanDelete'))))
      .finally(() => setSubmitting(false));
  }

  // ---- Static routes (Phase 8) ------------------------------------------

  function routeFormError(): string {
    const f = routeForm;
    if (!f.destination) return t('network.validate.destRequired');
    // Allow "default" as a special destination, plus the CIDR forms
    // that the backend's ValidateRouteCIDR accepts.
    if (f.destination !== 'default' &&
        !/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\/\d{1,2}$/.test(f.destination) &&
        !(f.destination.includes(':') && f.destination.includes('/'))) {
      return t('network.validate.destCidr');
    }
    if (f.gateway && !/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(f.gateway) &&
        !f.gateway.includes(':')) {
      return t('network.validate.gatewayAny');
    }
    if (f.metric) {
      const m = Number(f.metric);
      if (!Number.isInteger(m) || m < 0) return t('network.validate.metric');
    }
    return '';
  }

  function submitRouteForm() {
    const err = routeFormError();
    if (err) { setError(err); return; }
    const f = routeForm;
    const body: any = {
      destination: f.destination,
    };
    if (f.gateway) body.gateway = f.gateway;
    if (f.iface) body.interface = f.iface;
    if (f.metric) body.metric = Number(f.metric);
    setSubmitting(true);
    setError('');
    api.addRoute(body)
      .then(() => {
        setShowRouteForm(false);
        setRouteForm({ destination: '', gateway: '', iface: '', metric: '' });
        loadData();
      })
      .catch(e => setError(extractError(e, t('network.error.routeAdd'))))
      .finally(() => setSubmitting(false));
  }

  function deleteStaticRoute(id: string) {
    if (!window.confirm(t('network.confirm.deleteRoute', { id }))) return;
    setSubmitting(true);
    setError('');
    api.deleteRoute(id)
      .then(() => loadData())
      .catch(e => setError(extractError(e, t('network.error.routeDelete'))))
      .finally(() => setSubmitting(false));
  }

  // ---- Per-NIC stats drill-down ----------------------------------------

  function toggleStats(name: string) {
    if (statsOpen === name) {
      setStatsOpen('');
      statsHistoryRef.current = null;
      setStatsLatest(null);
      setStatsRates(null);
      return;
    }
    setStatsOpen(name);
    statsHistoryRef.current = null;
    setStatsLatest(null);
    setStatsRates(null);
  }

  useEffect(() => {
    if (!statsOpen) return;
    let cancelled = false;
    const tick = () => {
      api.getInterfaceStats(statsOpen)
        .then((sample: any) => {
          if (cancelled) return;
          const now = Date.now();
          const prev = statsHistoryRef.current;
          if (prev && now > prev.t) {
            const dt = (now - prev.t) / 1000;
            setStatsRates({
              rxBps: Math.max(0, (Number(sample.rx_bytes) - Number(prev.sample.rx_bytes)) / dt),
              txBps: Math.max(0, (Number(sample.tx_bytes) - Number(prev.sample.tx_bytes)) / dt),
            });
          }
          statsHistoryRef.current = { t: now, sample };
          setStatsLatest(sample);
        })
        .catch(() => { /* swallow; show last known sample */ });
    };
    tick();
    const id = window.setInterval(tick, 2000);
    return () => { cancelled = true; window.clearInterval(id); };
  }, [statsOpen]);

  function formatBytesPerSec(bps: number): string {
    if (!isFinite(bps) || bps < 1) return '0 B/s';
    const units = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
    let i = 0;
    while (bps >= 1024 && i < units.length - 1) { bps /= 1024; i++; }
    return `${bps.toFixed(bps < 10 ? 1 : 0)} ${units[i]}`;
  }

  function renderStatsPanel(_name: string) {
    if (!statsLatest) {
      return <div style={{ padding: 8, color: '#888' }}>{t('network.stats.sampling')}</div>;
    }
    const rxRate = statsRates ? formatBytesPerSec(statsRates.rxBps) : t('network.stats.samplingShort');
    const txRate = statsRates ? formatBytesPerSec(statsRates.txBps) : t('network.stats.samplingShort');
    return (
      <div style={{ padding: 8 }}>
        <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', marginBottom: 8 }}>
          <div>
            <div style={{ fontSize: 11, color: '#888' }}>{t('network.stats.rxRate')}</div>
            <div style={{ fontSize: 18, fontWeight: 600 }}>{rxRate}</div>
          </div>
          <div>
            <div style={{ fontSize: 11, color: '#888' }}>{t('network.stats.txRate')}</div>
            <div style={{ fontSize: 18, fontWeight: 600 }}>{txRate}</div>
          </div>
          <div>
            <div style={{ fontSize: 11, color: '#888' }}>{t('network.stats.estTcp')}</div>
            <div style={{ fontSize: 18, fontWeight: 600 }}>{Number(statsLatest.established_connections || 0)}</div>
          </div>
        </div>
        <div style={{ fontSize: 12, color: '#666' }}>
          {t('network.stats.counters', {
            rxBytes: Number(statsLatest.rx_bytes || 0).toLocaleString(),
            rxPkts: Number(statsLatest.rx_packets || 0).toLocaleString(),
            rxDrop: Number(statsLatest.rx_drop || 0).toLocaleString(),
            txBytes: Number(statsLatest.tx_bytes || 0).toLocaleString(),
            txPkts: Number(statsLatest.tx_packets || 0).toLocaleString(),
          })}
        </div>
      </div>
    );
  }

  // ---- Break Bond / Re-create Bond -------------------------------------

  function breakBond(bondName: string) {
    const ok = window.confirm(t('network.confirm.breakBond', { name: bondName }));
    if (!ok) return;
    setSubmitting(true);
    setError('');
    api.breakBond(bondName)
      .then(() => loadData())
      .catch(e => setError(extractError(e, t('network.error.breakBond'))))
      .finally(() => setSubmitting(false));
  }

  function recreateDefaultBond() {
    const ok = window.confirm(t('network.confirm.recreateBond'));
    if (!ok) return;
    setSubmitting(true);
    setError('');
    api.recreateDefaultBond()
      .then(() => loadData())
      .catch(e => setError(extractError(e, t('network.error.recreateBond'))))
      .finally(() => setSubmitting(false));
  }

  // ---- Change Mode (bond) -----------------------------------------------

  function openChangeMode(bondName: string) {
    const b = bonds.find(x => x.name === bondName);
    setModeChoice(b?.mode || BOND_MODES[0]);
    setModeChangeBond(bondName);
    setError('');
  }

  function cancelChangeMode() {
    setModeChangeBond(null);
  }

  function submitChangeMode() {
    if (!modeChangeBond) return;
    const current = bonds.find(b => b.name === modeChangeBond);
    if (!current) {
      setError(t('network.error.bondNotFound', { name: modeChangeBond }));
      return;
    }
    if (modeChoice === current.mode) {
      cancelChangeMode();
      return;
    }
    if (!BOND_MODES.includes(modeChoice)) {
      setError(t('network.error.unknownMode', { mode: modeChoice }));
      return;
    }

    // Pass through everything else verbatim so the mode swap doesn't
    // accidentally clobber a static IP, the member list, or DNS/MTU
    // overrides set on the bond. Acceptance criterion 7.4: a static
    // IP set on the bond survives a mode change.
    const bondIface = findInterface(modeChangeBond);
    const body: any = {
      ...current,
      mode: modeChoice,
      ipv4_addrs: current.ipv4_addrs || bondIface?.ipv4_addrs || [],
      ipv6_addrs: current.ipv6_addrs || bondIface?.ipv6_addrs || [],
      gateway4: current.gateway4 || bondIface?.gateway4 || '',
      gateway6: current.gateway6 || bondIface?.gateway6 || '',
      dhcp4: current.dhcp4 ?? bondIface?.dhcp4 ?? false,
      dhcp6: current.dhcp6 ?? bondIface?.dhcp6 ?? false,
      mtu: current.mtu || bondIface?.mtu || 0,
      dns: current.dns || bondIface?.dns || [],
    };

    setSubmitting(true);
    setError('');
    api.updateBond(modeChangeBond, body)
      .then(() => {
        cancelChangeMode();
        loadData();
      })
      .catch(e => setError(extractError(e, t('network.error.changeMode'))))
      .finally(() => setSubmitting(false));
  }

  function submitEditIP() {
    if (!editTarget) return;
    const err = editFormError();
    if (err) {
      setError(err);
      return;
    }
    const f = editForm;
    const body: any = {
      dhcp4: f.dhcp,
      dhcp6: f.dhcp,
    };
    if (!f.dhcp) {
      if (f.ipv4) body.ipv4_addrs = [f.ipv4];
      if (f.gateway4) body.gateway4 = f.gateway4;
      if (f.ipv6) body.ipv6_addrs = [f.ipv6];
      if (f.gateway6) body.gateway6 = f.gateway6;
    }
    if (f.mtu) body.mtu = Number(f.mtu);
    if (f.dns.trim()) {
      body.dns = f.dns.split(',').map(s => s.trim()).filter(Boolean);
    }

    setSubmitting(true);
    setError('');
    const promise = editTarget.kind === 'bond'
      ? api.updateBond(editTarget.name, { ...body, name: editTarget.name, mode: bonds.find(b => b.name === editTarget.name)?.mode || 'balance-alb', members: bonds.find(b => b.name === editTarget.name)?.members || [] })
      : api.configureInterface(editTarget.name, body);
    promise
      .then(() => {
        cancelEditIP();
        loadData();
      })
      .catch(e => setError(extractError(e, t('network.error.applyIp'))))
      .finally(() => setSubmitting(false));
  }

  // ---- helpers ----------------------------------------------------------

  function findInterface(name: string): any | undefined {
    return interfaces.find(i => i.name === name);
  }

  function physicalInterfaces(): any[] {
    const bondNames = new Set(bonds.map(b => b.name));
    const vlanNames = new Set(vlans.map(v => v.name));
    return interfaces.filter(i => !bondNames.has(i.name) && !vlanNames.has(i.name));
  }

  function defaultGateway(): string {
    const def = routes.find(r => r.destination === 'default' || r.destination === '0.0.0.0/0');
    if (def?.gateway) return def.gateway;
    for (const i of interfaces) {
      if (i.gateway4) return i.gateway4;
    }
    return '';
  }

  function ifaceAddresses(i: any): string {
    const addrs: string[] = [];
    for (const a of (i?.addresses || [])) addrs.push(a);
    for (const a of (i?.ipv4_addrs || [])) addrs.push(a);
    for (const a of (i?.ipv6_addrs || [])) addrs.push(a);
    return addrs.length ? addrs.join(', ') : '—';
  }

  function ifaceIPMode(i: any): string {
    if (i?.dhcp4 || i?.dhcp6) return t('network.ipMode.dhcp');
    if (((i?.ipv4_addrs || []).length + (i?.ipv6_addrs || []).length) > 0) return t('network.ipMode.static');
    if ((i?.addresses || []).length > 0) return t('network.ipMode.dhcp');
    return '—';
  }

  function speedLabel(i: any): string {
    if (i?.speed) return i.speed;
    if (i?.speed_mbps) return `${i.speed_mbps} Mb/s`;
    return '—';
  }

  function linkBadgeClass(i: any): string {
    const s = i?.state || i?.link;
    return s === 'up' ? 'active' : 'inactive';
  }

  function linkLabel(i: any): string {
    return i?.state || i?.link || t('common.unknown');
  }

  // ---- card renderers ---------------------------------------------------

  function systemCard() {
    return (
      <div className="section">
        <h2>{t('network.section.system')}</h2>
        <table className="data-table">
          <tbody>
            <tr><th>{t('network.system.hostname')}</th><td>{hostname ? <code>{hostname}</code> : '—'}</td></tr>
            <tr><th>{t('network.system.dns')}</th><td>{(dns?.servers || dns?.dns_servers || []).join(', ') || '—'}</td></tr>
            <tr><th>{t('network.system.defaultRoute')}</th><td>{defaultGateway() || '—'}</td></tr>
          </tbody>
        </table>
      </div>
    );
  }

  function activeTopologyCard() {
    if (bonds.length > 0) {
      return (
        <div className="section">
          <h2>{t('network.section.activeTopology')}</h2>
          {bonds.map(b => {
            const bondIface = findInterface(b.name);
            return (
              <div key={b.name} style={{ marginBottom: 16 }}>
                <div style={{ display: 'flex', gap: 12, alignItems: 'baseline', marginBottom: 8 }}>
                  <strong>{b.name}</strong>
                  <span className="badge info">{b.mode}</span>
                  <span style={{ color: '#666' }}>
                    {t('network.bond.ipSummary', { addrs: ifaceAddresses(bondIface), mode: ifaceIPMode(bondIface || b) })}
                  </span>
                  <button className="btn secondary" onClick={() => openEditIP({ kind: 'bond', name: b.name })}>
                    {t('network.button.editIp')}
                  </button>
                  <button className="btn secondary" onClick={() => openChangeMode(b.name)}>
                    {t('network.button.changeMode')}
                  </button>
                  <button className="btn danger" onClick={() => breakBond(b.name)} disabled={submitting}>
                    {t('network.button.breakBond')}
                  </button>
                </div>
                <table className="data-table">
                  <thead>
                    <tr><th>{t('network.col.member')}</th><th>{t('network.col.link')}</th><th>{t('network.col.speed')}</th><th>{t('network.col.mac')}</th><th>{t('arrays.col.actions')}</th></tr>
                  </thead>
                  <tbody>
                    {(b.members || []).flatMap((m: string) => {
                      const iface = findInterface(m);
                      const rows = [
                        <tr key={m}>
                          <td><code>{m}</code></td>
                          <td>
                            <span className={`badge ${linkBadgeClass(iface)}`}>
                              {linkLabel(iface)}
                            </span>
                          </td>
                          <td>{speedLabel(iface)}</td>
                          <td><code>{iface?.mac || '—'}</code></td>
                          <td className="action-cell">
                            <button className="btn secondary" onClick={() => toggleStats(m)}>
                              {statsOpen === m ? t('network.button.hideStats') : t('network.button.stats')}
                            </button>
                          </td>
                        </tr>,
                        statsOpen === m ? (
                          <tr key={m + ':stats'}>
                            <td colSpan={5}>{renderStatsPanel(m)}</td>
                          </tr>
                        ) : null,
                      ];
                      return rows.filter(Boolean);
                    })}
                  </tbody>
                </table>
              </div>
            );
          })}
          <p className="form-hint" style={{ fontSize: 12, color: '#888', marginTop: 8 }}>
            {t('network.bond.hint')}
          </p>
        </div>
      );
    }

    const phys = physicalInterfaces();
    return (
      <div className="section">
        <h2>{t('network.section.activeTopologyIndependent')}</h2>
        {phys.length === 0 ? <p>{t('network.empty.physicalInterfaces')}</p> : (
          <table className="data-table">
            <thead>
              <tr>
                <th>{t('network.col.nic')}</th><th>{t('network.col.link')}</th><th>{t('network.col.speed')}</th>
                <th>{t('network.col.ip')}</th><th>{t('network.col.mode')}</th><th>{t('network.col.mac')}</th><th>{t('arrays.col.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {phys.flatMap(i => [
                <tr key={i.name}>
                  <td><strong>{i.name}</strong></td>
                  <td><span className={`badge ${linkBadgeClass(i)}`}>{linkLabel(i)}</span></td>
                  <td>{speedLabel(i)}</td>
                  <td>{ifaceAddresses(i)}</td>
                  <td>{ifaceIPMode(i)}</td>
                  <td><code>{i.mac || '—'}</code></td>
                  <td className="action-cell">
                    <button className="btn secondary" onClick={() => openEditIP({ kind: 'iface', name: i.name })}>
                      {t('network.button.editIp')}
                    </button>
                    <button className="btn secondary" onClick={() => toggleStats(i.name)}>
                      {statsOpen === i.name ? t('network.button.hideStats') : t('network.button.stats')}
                    </button>
                  </td>
                </tr>,
                statsOpen === i.name ? (
                  <tr key={i.name + ':stats'}>
                    <td colSpan={7}>{renderStatsPanel(i.name)}</td>
                  </tr>
                ) : null,
              ].filter(Boolean))}
            </tbody>
          </table>
        )}
        <div style={{ marginTop: 12 }}>
          <button className="btn secondary" onClick={recreateDefaultBond} disabled={submitting}>
            {t('network.button.recreateBond')}
          </button>
          <span className="form-hint" style={{ fontSize: 12, color: '#888', marginLeft: 8 }}>
            {t('network.bond.recreateHint')}
          </span>
        </div>
      </div>
    );
  }

  function vlansCard() {
    return (
      <div className="section">
        <h2>{t('network.section.vlans')}</h2>
        {vlans.length === 0 ? <p>{t('network.empty.vlans')}</p> : (
          <table className="data-table">
            <thead>
              <tr><th>{t('datasets.col.name')}</th><th>{t('network.col.parent')}</th><th>{t('network.col.vlanId')}</th><th>{t('network.col.ip')}</th><th>{t('arrays.col.actions')}</th></tr>
            </thead>
            <tbody>
              {vlans.map(v => {
                const iface = findInterface(v.name);
                return (
                  <tr key={v.name}>
                    <td><code>{v.name}</code></td>
                    <td>{v.parent}</td>
                    <td>{v.vlan_id}</td>
                    <td>{ifaceAddresses(iface)}</td>
                    <td className="action-cell">
                      <button className="btn danger" onClick={() => deleteVlan(v.name)} disabled={submitting}>
                        {t('common.delete')}
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        <div style={{ marginTop: 8 }}>
          <button className="btn secondary" onClick={() => setShowVlanForm(s => !s)}>
            {showVlanForm ? t('common.cancel') : t('network.button.addVlan')}
          </button>
        </div>
        {showVlanForm && (
          <div className="create-form" style={{ marginTop: 8 }}>
            <h3>{t('network.button.addVlan')}</h3>
            <div className="form-row">
              <label>
                {t('network.col.parent')}
                <select
                  value={vlanForm.parent}
                  onChange={e => setVlanForm(f => ({ ...f, parent: e.target.value }))}
                >
                  <option value="">{t('network.field.pickParent')}</option>
                  {vlanParents().map(p => (
                    <option key={p} value={p}>{p}</option>
                  ))}
                </select>
              </label>
              <label>
                {t('network.field.vlanId')}
                <input
                  value={vlanForm.id}
                  onChange={e => setVlanForm(f => ({ ...f, id: e.target.value }))}
                  placeholder="100"
                />
              </label>
            </div>
            <div className="form-row">
              <label>
                <input
                  type="checkbox"
                  checked={vlanForm.dhcp}
                  onChange={e => setVlanForm(f => ({ ...f, dhcp: e.target.checked }))}
                />
                {' '}{t('network.ipMode.dhcp')}
              </label>
            </div>
            {!vlanForm.dhcp && (
              <div className="form-row">
                <label>
                  {t('network.field.ipv4Cidr')}
                  <input
                    value={vlanForm.ipv4}
                    onChange={e => setVlanForm(f => ({ ...f, ipv4: e.target.value }))}
                    placeholder="10.0.100.5/24"
                  />
                </label>
                <label>
                  {t('network.field.ipv4Gateway')}
                  <input
                    value={vlanForm.gateway4}
                    onChange={e => setVlanForm(f => ({ ...f, gateway4: e.target.value }))}
                    placeholder="10.0.100.1"
                  />
                </label>
              </div>
            )}
            <div style={{ display: 'flex', gap: 8 }}>
              <button className="btn secondary" onClick={() => setShowVlanForm(false)} disabled={submitting}>
                {t('common.cancel')}
              </button>
              <button
                className="btn primary"
                onClick={submitVlanForm}
                disabled={submitting || !!vlanFormError()}
                title={vlanFormError() || ''}
              >
                {submitting ? t('network.button.adding') : t('network.button.addVlan')}
              </button>
            </div>
          </div>
        )}
      </div>
    );
  }

  function routesCard() {
    return (
      <div className="section">
        <h2>{t('network.section.routes')}</h2>
        {routes.length === 0 ? <p>{t('network.empty.routes')}</p> : (
          <table className="data-table">
            <thead>
              <tr><th>{t('network.col.destination')}</th><th>{t('network.col.gateway')}</th><th>{t('network.col.interface')}</th><th>{t('network.col.metric')}</th><th>{t('arrays.col.actions')}</th></tr>
            </thead>
            <tbody>
              {routes.map((r: any) => (
                <tr key={r.id || r.destination}>
                  <td><code>{r.destination}</code></td>
                  <td>{r.gateway || '—'}</td>
                  <td>{r.interface || '—'}</td>
                  <td>{r.metric || '—'}</td>
                  <td className="action-cell">
                    <button
                      className="btn danger"
                      onClick={() => deleteStaticRoute(r.id || r.destination)}
                      disabled={submitting}
                    >
                      {t('common.delete')}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <div style={{ marginTop: 8 }}>
          <button className="btn secondary" onClick={() => setShowRouteForm(s => !s)}>
            {showRouteForm ? t('common.cancel') : t('network.button.addRoute')}
          </button>
        </div>
        {showRouteForm && (
          <div className="create-form" style={{ marginTop: 8 }}>
            <h3>{t('network.route.addTitle')}</h3>
            <div className="form-row">
              <label>
                {t('network.col.destination')}
                <input
                  value={routeForm.destination}
                  onChange={e => setRouteForm(f => ({ ...f, destination: e.target.value }))}
                  placeholder="10.0.0.0/8 or default"
                />
              </label>
              <label>
                {t('network.field.gatewayOpt')}
                <input
                  value={routeForm.gateway}
                  onChange={e => setRouteForm(f => ({ ...f, gateway: e.target.value }))}
                  placeholder="192.168.1.1"
                />
              </label>
            </div>
            <div className="form-row">
              <label>
                {t('network.field.interfaceOpt')}
                <input
                  value={routeForm.iface}
                  onChange={e => setRouteForm(f => ({ ...f, iface: e.target.value }))}
                  placeholder="bond0 / enp1s0"
                />
              </label>
              <label>
                {t('network.field.metricOpt')}
                <input
                  value={routeForm.metric}
                  onChange={e => setRouteForm(f => ({ ...f, metric: e.target.value }))}
                  placeholder="100"
                />
              </label>
            </div>
            <div style={{ display: 'flex', gap: 8 }}>
              <button className="btn secondary" onClick={() => setShowRouteForm(false)} disabled={submitting}>
                {t('common.cancel')}
              </button>
              <button
                className="btn primary"
                onClick={submitRouteForm}
                disabled={submitting || !!routeFormError()}
                title={routeFormError() || ''}
              >
                {submitting ? t('network.button.adding') : t('network.button.addRoute')}
              </button>
            </div>
          </div>
        )}
      </div>
    );
  }

  function multiFlowStatusCard() {
    const ips = (multiFlow?.active_ips || []) as string[];
    const pathCount = ips.length;
    const exposingMultiplePaths = pathCount > 1;
    const smbOn = !!multiFlow?.smb_multichannel_enabled;
    const smbAdvertised = (multiFlow?.smb_advertised_ips || []) as string[];
    const nfsListening = (multiFlow?.nfs_listening_ips || []) as string[];
    const iscsiTargets = Number(multiFlow?.iscsi_targets || 0);
    const iscsiPortals = Number(multiFlow?.iscsi_portals_per_target || 0);

    return (
      <div className="section">
        <h2>{t('network.section.multiflow')}</h2>
        <table className="data-table">
          <tbody>
            <tr>
              <th>{t('network.multiflow.topologyPaths')}</th>
              <td>
                {pathCount === 0 ? (
                  <span className="badge inactive">{t('network.multiflow.noIps')}</span>
                ) : (
                  <span className={`badge ${exposingMultiplePaths ? 'active' : 'info'}`}>
                    {t(pathCount === 1 ? 'network.multiflow.pathOne' : 'network.multiflow.pathMany', { count: pathCount })}
                  </span>
                )}
                {pathCount > 0 && (
                  <span style={{ fontSize: 12, color: '#666', marginLeft: 8 }}>
                    {ips.join(', ')}
                  </span>
                )}
              </td>
            </tr>
            <tr>
              <th>{t('network.multiflow.smbMultichannel')}</th>
              <td>
                <span className={`badge ${smbOn ? 'active' : 'inactive'}`}>
                  {smbOn ? t('network.multiflow.enabled') : t('network.multiflow.disabled')}
                </span>
                <span style={{ fontSize: 12, color: '#666', marginLeft: 8 }}>
                  {t(smbAdvertised.length === 1 ? 'network.multiflow.advertisingOne' : 'network.multiflow.advertisingMany', { count: smbAdvertised.length })}
                </span>
              </td>
            </tr>
            <tr>
              <th>{t('network.multiflow.nfsMultipath')}</th>
              <td>
                <span className={`badge ${nfsListening.length > 1 ? 'active' : 'info'}`}>
                  {t(nfsListening.length === 1 ? 'network.multiflow.listeningOne' : 'network.multiflow.listeningMany', { count: nfsListening.length })}
                </span>
              </td>
            </tr>
            <tr>
              <th>{t('network.multiflow.iscsiPortals')}</th>
              <td>
                <span className="badge info">
                  {t('network.multiflow.iscsiSummary', {
                    targets: iscsiTargets,
                    targetsLabel: iscsiTargets === 1 ? t('network.multiflow.targetOne') : t('network.multiflow.targetMany'),
                    portals: iscsiPortals,
                    portalsLabel: iscsiPortals === 1 ? t('network.multiflow.portalOne') : t('network.multiflow.portalMany'),
                  })}
                </span>
              </td>
            </tr>
          </tbody>
        </table>
        {!exposingMultiplePaths && (
          <p className="form-hint" style={{ fontSize: 12, color: '#888', marginTop: 8 }}>
            {t('network.multiflow.singlePathHint')}
          </p>
        )}
      </div>
    );
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('network.title')}</h1>
        <p className="subtitle">{t('network.subtitle')}</p>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {error && <div className="error-msg">{error}</div>}

      {pending && (
        <div className="safe-apply-banner">
          <span>{t('network.pending.message', { seconds: pending.revert_in || '?' })}</span>
          <button className="btn primary" onClick={confirm}>{t('network.pending.confirm')}</button>
          <button className="btn danger" onClick={revert}>{t('network.pending.revert')}</button>
        </div>
      )}

      {modeChangeBond && (
        <div className="create-form">
          <h3>{t('network.changeMode.title', { name: modeChangeBond })}</h3>
          <div className="form-row">
            <label>
              {t('network.field.bondMode')}
              <select
                value={modeChoice}
                onChange={e => setModeChoice(e.target.value)}
              >
                {BOND_MODES.map(m => (
                  <option key={m} value={m}>{m}</option>
                ))}
              </select>
            </label>
          </div>
          <div className="form-hint" style={{ fontSize: 12, color: '#888', marginBottom: 8 }}>
            {t('network.changeMode.hint')}
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={cancelChangeMode} disabled={submitting}>
              {t('common.cancel')}
            </button>
            <button
              className="btn primary"
              onClick={submitChangeMode}
              disabled={submitting}
            >
              {submitting ? t('network.button.applying') : t('common.apply')}
            </button>
          </div>
        </div>
      )}

      {editTarget && (
        <div className="create-form">
          <h3>{t(editTarget.kind === 'bond' ? 'network.editIp.titleBond' : 'network.editIp.titleIface', { name: editTarget.name })}</h3>
          <div className="form-row">
            <label>
              <input
                type="checkbox"
                checked={editForm.dhcp}
                onChange={e => setEditForm(f => ({ ...f, dhcp: e.target.checked }))}
              />
              {' '}{t('network.field.dhcpDual')}
            </label>
          </div>
          {!editForm.dhcp && (
            <>
              <div className="form-row">
                <label>
                  {t('network.field.ipv4Cidr')}
                  <input
                    value={editForm.ipv4}
                    onChange={e => setEditForm(f => ({ ...f, ipv4: e.target.value }))}
                    placeholder="192.168.1.10/24"
                  />
                </label>
                <label>
                  {t('network.field.ipv4Gateway')}
                  <input
                    value={editForm.gateway4}
                    onChange={e => setEditForm(f => ({ ...f, gateway4: e.target.value }))}
                    placeholder="192.168.1.1"
                  />
                </label>
              </div>
              <div className="form-row">
                <label>
                  {t('network.field.ipv6Cidr')}
                  <input
                    value={editForm.ipv6}
                    onChange={e => setEditForm(f => ({ ...f, ipv6: e.target.value }))}
                    placeholder="2001:db8::10/64"
                  />
                </label>
                <label>
                  {t('network.field.ipv6Gateway')}
                  <input
                    value={editForm.gateway6}
                    onChange={e => setEditForm(f => ({ ...f, gateway6: e.target.value }))}
                    placeholder="fe80::1"
                  />
                </label>
              </div>
            </>
          )}
          <div className="form-row">
            <label>
              {t('network.field.mtu')}
              <input
                value={editForm.mtu}
                onChange={e => setEditForm(f => ({ ...f, mtu: e.target.value }))}
                placeholder="1500"
              />
            </label>
            <label>
              {t('network.field.dnsOverrides')}
              <input
                value={editForm.dns}
                onChange={e => setEditForm(f => ({ ...f, dns: e.target.value }))}
                placeholder="1.1.1.1, 1.0.0.1"
              />
            </label>
          </div>
          <div className="form-hint" style={{ fontSize: 12, color: '#888', marginBottom: 8 }}>
            {t('network.editIp.hint')}
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={cancelEditIP} disabled={submitting}>
              {t('common.cancel')}
            </button>
            <button
              className="btn primary"
              onClick={submitEditIP}
              disabled={submitting || !!editFormError()}
              title={editFormError() || ''}
            >
              {submitting ? t('network.button.applying') : t('common.apply')}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text={t('network.loading')} />

      {!loading && (
        <>
          {systemCard()}
          {activeTopologyCard()}
          {vlansCard()}
          {routesCard()}
          {multiFlowStatusCard()}
        </>
      )}
    </div>
  );
}
