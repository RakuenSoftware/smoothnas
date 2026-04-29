import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

function formatBytes(n: number | undefined): string {
  if (n == null || n <= 0) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0; let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatProgress(moved: number, total: number): string {
  if (!total) return '—';
  const pct = Math.round((moved / total) * 100);
  return `${formatBytes(moved)} / ${formatBytes(total)} (${pct}%)`;
}

function healthDot(severity: string) {
  if (severity === 'critical') return <span style={{ color: '#d32f2f', fontWeight: 700 }}>●</span>;
  if (severity === 'warning') return <span style={{ color: '#f9a825', fontWeight: 700 }}>●</span>;
  return null;
}

function CapabilityBadge({ label, value }: { label: string; value: string | boolean | undefined }) {
  if (!value || value === 'n/a' || value === 'none' || value === '') return null;
  const text = typeof value === 'boolean' ? label : `${label}: ${value}`;
  return (
    <span style={{
      display: 'inline-block',
      fontSize: 10,
      background: '#e8f5e9',
      color: '#2e7d32',
      border: '1px solid #a5d6a7',
      borderRadius: 8,
      padding: '1px 7px',
      marginRight: 4,
      marginBottom: 2,
      fontWeight: 500,
    }}>{text}</span>
  );
}

function bandColor(band: string) {
  switch (band) {
    case 'hot': return '#c62828';
    case 'warm': return '#f57c00';
    case 'cold': return '#1565c0';
    case 'idle': return '#757575';
    default: return '#999';
  }
}

const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;
const COORDINATED_SNAPSHOT_MODE = 'coordinated-namespace';

export default function TieringInventory() {
  const { t } = useI18n();
  const toast = useToast();
  const navigate = useNavigate();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [domains, setDomains] = useState<any[]>([]);
  const [targets, setTargets] = useState<any[]>([]);
  const [movements, setMovements] = useState<any[]>([]);
  const [degraded, setDegraded] = useState<any[]>([]);
  const [namespaces, setNamespaces] = useState<any[]>([]);
  const [snapshotsByNamespace, setSnapshotsByNamespace] = useState<Record<string, any[]>>({});
  const [snapshotErrors, setSnapshotErrors] = useState<Record<string, string>>({});
  const [snapshotBusy, setSnapshotBusy] = useState<Record<string, boolean>>({});

  // UI state
  const [collapsedDomains, setCollapsedDomains] = useState<Set<string>>(new Set());
  const [expandedTargets, setExpandedTargets] = useState<Set<string>>(new Set());
  const [domainFilter, setDomainFilter] = useState('');
  const [showHistory, setShowHistory] = useState(false);
  const [showDegraded, setShowDegraded] = useState(true);

  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const [confirmText, setConfirmText] = useState(t('arrays.confirm.confirm'));
  const [confirmClass, setConfirmClass] = useState('btn danger');
  const confirmAction = useRef<(() => void) | null>(null);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load(); }, []);

  function load() {
    setLoading(true);
    setError('');
    Promise.all([
      api.getTieringDomains(),
      api.getTieringTargets(),
      api.getTieringMovements(),
      api.getTieringDegraded(),
      api.getTieringNamespaces(),
    ]).then(([d, t, m, deg, ns]) => {
      setDomains(d || []);
      setTargets(t || []);
      setMovements(m || []);
      setDegraded(deg || []);
      setNamespaces(ns || []);
      setLoading(false);
      refreshSnapshotLists(ns || []);
    }).catch(e => {
      const msg = extractError(e, t('tieringInventory.error.load'));
      setError(msg);
      toast.error(msg);
      setLoading(false);
    });
  }

  function toggleDomain(id: string) {
    setCollapsedDomains(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  }

  function toggleTarget(id: string) {
    setExpandedTargets(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  }

  function cancelMovement(id: string) {
    setConfirmTitle(t('tieringInventory.confirm.cancelTitle'));
    setConfirmMessage(t('tieringInventory.confirm.cancelMessage'));
    setConfirmText(t('tieringInventory.confirm.cancelTitle'));
    setConfirmClass('btn danger');
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.cancelTieringMovement(id).then(() => {
        toast.success(t('tieringInventory.toast.cancelled'));
        setMovements(prev => prev.map(j => j.id === id ? { ...j, state: 'cancelled' } : j));
      }).catch(e => toast.error(extractError(e, t('tieringInventory.error.cancel'))));
    };
    setConfirmVisible(true);
  }

  function refreshSnapshotLists(nsList = namespaces) {
    nsList
      .filter((ns: any) => ns.snapshot_mode === COORDINATED_SNAPSHOT_MODE)
      .forEach((ns: any) => {
        setSnapshotBusy(prev => ({ ...prev, [ns.id]: true }));
        api.listTieringNamespaceSnapshots(ns.id)
          .then(snaps => {
            setSnapshotsByNamespace(prev => ({ ...prev, [ns.id]: snaps || [] }));
            setSnapshotErrors(prev => {
              const next = { ...prev };
              delete next[ns.id];
              return next;
            });
          })
          .catch(e => {
            const msg = extractError(e, t('tiering.error.loadSnapshots'));
            setSnapshotErrors(prev => ({ ...prev, [ns.id]: msg }));
          })
          .finally(() => {
            setSnapshotBusy(prev => ({ ...prev, [ns.id]: false }));
          });
      });
  }

  function createSnapshot(ns: any) {
    setSnapshotBusy(prev => ({ ...prev, [ns.id]: true }));
    setSnapshotErrors(prev => {
      const next = { ...prev };
      delete next[ns.id];
      return next;
    });
    api.createTieringNamespaceSnapshot(ns.id)
      .then(() => {
        toast.success(t('tieringInventory.toast.snapshotCreatedFor', { name: ns.name }));
        return api.listTieringNamespaceSnapshots(ns.id);
      })
      .then(snaps => {
        setSnapshotsByNamespace(prev => ({ ...prev, [ns.id]: snaps || [] }));
      })
      .catch(e => {
        const msg = extractError(e, t('tiering.error.createSnapshot'));
        setSnapshotErrors(prev => ({ ...prev, [ns.id]: msg }));
        toast.error(msg);
      })
      .finally(() => {
        setSnapshotBusy(prev => ({ ...prev, [ns.id]: false }));
        load();
      });
  }

  function deleteSnapshot(ns: any, snapshotID: string) {
    setConfirmTitle(t('tiering.confirm.deleteTitle'));
    setConfirmMessage(t('tieringInventory.confirm.deleteSnapshotMessage', { id: snapshotID, name: ns.name }));
    setConfirmText(t('tiering.confirm.deleteTitle'));
    setConfirmClass('btn danger');
    confirmAction.current = () => {
      setConfirmVisible(false);
      setSnapshotBusy(prev => ({ ...prev, [ns.id]: true }));
      setSnapshotErrors(prev => {
        const next = { ...prev };
        delete next[ns.id];
        return next;
      });
      api.deleteTieringNamespaceSnapshot(ns.id, snapshotID)
        .then(() => {
          setSnapshotsByNamespace(prev => ({
            ...prev,
            [ns.id]: (prev[ns.id] || []).filter((s: any) => s.snapshot_id !== snapshotID),
          }));
          toast.success(t('tiering.toast.snapshotDeleted'));
        })
        .catch(e => {
          const msg = extractError(e, t('tiering.error.deleteSnapshot'));
          setSnapshotErrors(prev => ({ ...prev, [ns.id]: msg }));
          toast.error(msg);
        })
        .finally(() => {
          setSnapshotBusy(prev => ({ ...prev, [ns.id]: false }));
          load();
        });
    };
    setConfirmVisible(true);
  }

  function nsName(id: string): string {
    const ns = namespaces.find((n: any) => n.id === id);
    return ns ? ns.name : id.slice(0, 8);
  }

  function targetName(id: string): string {
    const t = targets.find((t: any) => t.id === id);
    return t ? t.name : id.slice(0, 8);
  }

  function manageInLink(target: any): { label: string; path: string } {
    if (target.backend_kind === 'zfsmgd' || target.backend_kind === 'zfs-managed') {
      return { label: t('tieringInventory.button.manageZfs'), path: '/pools' };
    }
    return { label: t('tieringInventory.button.manageMdadm'), path: '/tiers' };
  }

  // Aggregate capacity and health per domain
  function domainStats(domainID: string) {
    const ts = targets.filter((t: any) => t.placement_domain === domainID);
    const criticalCount = degraded.filter((d: any) =>
      ts.some((t: any) => t.id === d.scope_id) && d.severity === 'critical'
    ).length;
    const warningCount = degraded.filter((d: any) =>
      ts.some((t: any) => t.id === d.scope_id) && d.severity === 'warning'
    ).length;
    return { criticalCount, warningCount };
  }

  function targetDegradedStates(targetID: string) {
    return degraded.filter((d: any) => d.scope_id === targetID);
  }

  function namespacesForDomain(domainID: string) {
    return namespaces
      .filter((ns: any) => ns.placement_domain === domainID)
      .sort((a: any, b: any) => a.name.localeCompare(b.name));
  }

  // Partition movements into active vs history
  const activeMovements = movements.filter((j: any) =>
    j.state === 'pending' || j.state === 'running'
  );
  const historyMovements = movements.filter((j: any) => {
    if (j.state === 'pending' || j.state === 'running') return false;
    if (!j.updated_at) return true;
    const age = Date.now() - new Date(j.updated_at).getTime();
    return age <= THIRTY_DAYS_MS;
  });

  const filteredDomains = domainFilter
    ? domains.filter((d: any) => d.id === domainFilter)
    : domains;

  if (loading) return <div className="page"><Spinner loading /></div>;

  return (
    <div className="page">
      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText={confirmText}
        confirmClass={confirmClass}
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />

      <div className="page-header">
        <h1>{t('tieringInventory.title')}</h1>
        <p className="subtitle">
          {t('tieringInventory.subtitle')}
        </p>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <button className="refresh-btn" onClick={load}>{t('common.refresh')}</button>
          {domains.length > 0 && (
            <select
              value={domainFilter}
              onChange={e => setDomainFilter(e.target.value)}
              style={{ fontSize: 13, padding: '4px 8px', borderRadius: 4, border: '1px solid #ccc' }}
            >
              <option value="">{t('tieringInventory.filter.allDomains')}</option>
              {domains.map((d: any) => (
                <option key={d.id} value={d.id}>{d.id}</option>
              ))}
            </select>
          )}
        </div>
      </div>

      {error && <div className="error-msg">{error}</div>}

      {/* Domain groups */}
      {filteredDomains.length === 0 && !loading && (
        <div className="empty-state">{t('tieringInventory.empty.domains')}</div>
      )}

      {filteredDomains.map((domain: any) => {
        const domainTargets = targets
          .filter((t: any) => t.placement_domain === domain.id)
          .sort((a: any, b: any) => a.rank - b.rank);
        const collapsed = collapsedDomains.has(domain.id);
        const { criticalCount, warningCount } = domainStats(domain.id);

        return (
          <div key={domain.id} style={{ marginBottom: 20, border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
            {/* Domain header */}
            <div
              style={{
                display: 'flex', alignItems: 'center', gap: 10, padding: '10px 14px',
                background: '#f5f5f5', cursor: 'pointer', userSelect: 'none',
              }}
              onClick={() => toggleDomain(domain.id)}
            >
              <span style={{ fontSize: 12, color: '#777', fontWeight: 600, textTransform: 'uppercase', letterSpacing: '0.5px' }}>
                {t('tieringInventory.label.domain')}
              </span>
              <span style={{ fontWeight: 600, fontSize: 15 }}>{domain.id}</span>
              <span style={{
                fontSize: 11, background: '#e3f2fd', color: '#1565c0',
                borderRadius: 8, padding: '1px 7px', border: '1px solid #90caf9'
              }}>{domain.backend_kind}</span>
              {criticalCount > 0 && (
                <span style={{ fontSize: 12, color: '#d32f2f', fontWeight: 700 }}>
                  {t('tieringInventory.severity.critical', { count: criticalCount })}
                </span>
              )}
              {criticalCount === 0 && warningCount > 0 && (
                <span style={{ fontSize: 12, color: '#f9a825', fontWeight: 700 }}>
                  {t('tieringInventory.severity.warning', { count: warningCount })}
                </span>
              )}
              <span style={{ marginLeft: 'auto', fontSize: 12, color: '#888' }}>
                {t(domainTargets.length === 1 ? 'tieringInventory.summary.targetOne' : 'tieringInventory.summary.targetMany', { count: domainTargets.length })}
              </span>
              <span style={{ fontSize: 13, color: '#999', marginLeft: 4 }}>{collapsed ? '▶' : '▼'}</span>
            </div>

            {/* Targets */}
            {!collapsed && (
              <div>
                {domainTargets.length === 0 ? (
                  <div style={{ padding: '10px 14px', fontSize: 13, color: '#999' }}>
                    {t('tieringInventory.empty.targetsInDomain')}
                  </div>
                ) : (
                  <>
                    {/* Column headers */}
                    <div style={{
                      display: 'grid',
                      gridTemplateColumns: '1fr 80px 50px 80px 80px 80px 80px 80px 100px',
                      gap: 6, padding: '6px 14px',
                      fontSize: 11, color: '#888', borderBottom: '1px solid #eee',
                      background: '#fafafa',
                    }}>
                      <span>{t('datasets.col.name')}</span>
                      <span>{t('volumes.col.backend')}</span>
                      <span>{t('tiers.col.rank')}</span>
                      <span>{t('tiers.col.fillPct')}</span>
                      <span>{t('tiers.col.fullPct')}</span>
                      <span>{t('volumes.col.health')}</span>
                      <span>{t('tieringInventory.col.activity')}</span>
                      <span>{t('tieringInventory.col.queue')}</span>
                      <span></span>
                    </div>
                    {domainTargets.map((target: any) => {
                      const targetDeg = targetDegradedStates(target.id);
                      const isExpanded = expandedTargets.has(target.id);
                      const link = manageInLink(target);
                      const caps = target.capabilities || {};

                      return (
                        <div key={target.id} style={{ borderBottom: '1px solid #f0f0f0' }}>
                          {/* Main row */}
                          <div style={{
                            display: 'grid',
                            gridTemplateColumns: '1fr 80px 50px 80px 80px 80px 80px 80px 100px',
                            gap: 6, padding: '8px 14px', alignItems: 'center',
                            fontSize: 13,
                          }}>
                            <span style={{ fontWeight: 500, display: 'flex', alignItems: 'center', gap: 6 }}>
                              {target.name}
                              {targetDeg.length > 0 && (
                                <span
                                  title={targetDeg.map((d: any) => `${d.code}: ${d.message}`).join('\n')}
                                  style={{ cursor: 'help' }}
                                >
                                  {healthDot(targetDeg.some((d: any) => d.severity === 'critical') ? 'critical' : 'warning')}
                                </span>
                              )}
                            </span>
                            <span style={{ fontSize: 12, color: '#555' }}>{target.backend_kind}</span>
                            <span style={{
                              fontSize: 11, background: '#eeeeee', borderRadius: 8,
                              padding: '1px 6px', textAlign: 'center',
                            }}>#{target.rank}</span>
                            <span>{target.target_fill_pct ?? '—'}%</span>
                            <span>{target.full_threshold_pct ?? '—'}%</span>
                            <span style={{
                              fontSize: 11, fontWeight: 600,
                              color: target.health === 'healthy' ? '#2e7d32' :
                                target.health === 'degraded' ? '#e65100' : '#555',
                            }}>{target.health || '—'}</span>
                            <span style={{ color: bandColor(target.activity_band) }}>
                              {target.activity_band || '—'}
                            </span>
                            <span style={{ color: '#555' }}>
                              {target.queue_depth != null ? target.queue_depth : '—'}
                            </span>
                            <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
                              <button
                                style={{ fontSize: 11, padding: '2px 8px' }}
                                onClick={() => toggleTarget(target.id)}
                              >
                                {isExpanded ? t('tieringInventory.button.less') : t('tieringInventory.button.more')}
                              </button>
                              <button
                                className="btn secondary"
                                style={{ fontSize: 11, padding: '2px 8px' }}
                                onClick={e => { e.stopPropagation(); navigate(link.path); }}
                                title={link.label}
                              >
                                {link.label}
                              </button>
                            </div>
                          </div>

                          {/* Capability badges */}
                          <div style={{ padding: '0 14px 6px', display: 'flex', flexWrap: 'wrap', gap: 2 }}>
                            <CapabilityBadge label={t('tieringInventory.cap.move')} value={caps.movement_granularity} />
                            <CapabilityBadge label={t('tieringInventory.cap.pin')} value={caps.pin_scope} />
                            <CapabilityBadge label={t('tieringInventory.cap.recall')} value={caps.recall_mode && caps.recall_mode !== 'none' ? caps.recall_mode : undefined} />
                            {caps.snapshot_mode && caps.snapshot_mode !== 'none' && (
                              <CapabilityBadge label={t('tieringInventory.cap.snapshot')} value={caps.snapshot_mode} />
                            )}
                          </div>

                          {/* Advanced detail panel */}
                          {isExpanded && (
                            <div style={{
                              margin: '0 14px 10px',
                              padding: '10px 12px',
                              background: '#fafafa',
                              border: '1px solid #e0e0e0',
                              borderRadius: 4,
                              fontSize: 12,
                            }}>
                              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '6px 20px' }}>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.movementGranularity')}</span> {caps.movement_granularity || '—'}</div>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.recallMode')}</span> {caps.recall_mode || '—'}</div>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.snapshotMode')}</span> {caps.snapshot_mode || t('common.none').toLowerCase()}</div>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.checksums')}</span> {caps.supports_checksums ? t('common.yes').toLowerCase() : t('common.no').toLowerCase()}</div>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.compression')}</span> {caps.supports_compression ? t('common.yes').toLowerCase() : t('common.no').toLowerCase()}</div>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.onlineMove')}</span> {caps.supports_online_move ? t('common.yes').toLowerCase() : t('common.no').toLowerCase()}</div>
                                <div><span style={{ color: '#888' }}>{t('tieringInventory.field.pinScope')}</span> {caps.pin_scope || '—'}</div>
                                {target.backing_ref && (
                                  <div style={{ gridColumn: '1 / -1' }}>
                                    <span style={{ color: '#888' }}>{t('tieringInventory.field.backingRef')}</span> <code>{target.backing_ref}</code>
                                  </div>
                                )}
                                {targetDeg.length > 0 && (
                                  <div style={{ gridColumn: '1 / -1', marginTop: 6 }}>
                                    <div style={{ fontWeight: 600, color: '#c62828', marginBottom: 4 }}>{t('tieringInventory.section.activeDegraded')}</div>
                                    {targetDeg.map((d: any) => (
                                      <div key={d.id} style={{ marginBottom: 2 }}>
                                        <span style={{ color: d.severity === 'critical' ? '#d32f2f' : '#f9a825', fontWeight: 600 }}>
                                          {d.severity}
                                        </span>
                                        {' '}[{d.code}]: {d.message}
                                      </div>
                                    ))}
                                  </div>
                                )}
                              </div>
                            </div>
                          )}
                        </div>
                      );
                    })}
                    {(() => {
                      const domainNamespaces = namespacesForDomain(domain.id);
                      if (domainNamespaces.length === 0) return null;
                      return (
                        <div style={{ padding: '12px 14px 14px', background: '#fcfcfc', borderTop: '1px solid #e6e6e6' }}>
                          <div style={{ fontSize: 12, fontWeight: 700, color: '#555', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: 8 }}>
                            {t('tiering.section.namespaces')}
                          </div>
                          {domainNamespaces.map((ns: any) => {
                            const snaps = snapshotsByNamespace[ns.id] || [];
                            const busy = !!snapshotBusy[ns.id];
                            const err = snapshotErrors[ns.id];
                            const supportsSnapshots = ns.snapshot_mode === COORDINATED_SNAPSHOT_MODE;
                            const zfsManaged = ns.backend_kind === 'zfsmgd' || ns.backend_kind === 'zfs-managed';

                            return (
                              <div key={ns.id} style={{ border: '1px solid #e0e0e0', borderRadius: 6, marginBottom: 10, background: '#fff' }}>
                                <div style={{ display: 'grid', gridTemplateColumns: '1fr 110px 140px 130px', gap: 8, padding: '9px 10px', alignItems: 'center', fontSize: 13 }}>
                                  <div>
                                    <div style={{ fontWeight: 600 }}>{ns.name}</div>
                                    <code style={{ fontSize: 11 }}>{ns.exposed_path || ns.backend_ref || ns.id}</code>
                                  </div>
                                  <span style={{ fontSize: 12, color: '#555' }}>{ns.backend_kind}</span>
                                  <span style={{ fontSize: 12, color: supportsSnapshots ? '#2e7d32' : '#777' }}>
                                    {ns.snapshot_mode || t('common.none').toLowerCase()}
                                  </span>
                                  {supportsSnapshots ? (
                                    <button
                                      className="btn primary"
                                      style={{ fontSize: 12, padding: '3px 10px' }}
                                      disabled={busy}
                                      onClick={() => createSnapshot(ns)}
                                      title={t('tieringInventory.snapshot.createTooltip')}
                                    >
                                      {busy ? t('tiers.spindown.working') : t('tieringInventory.button.snapshot')}
                                    </button>
                                  ) : (
                                    <span />
                                  )}
                                </div>

                                {err && (
                                  <div style={{ margin: '0 10px 8px', padding: '7px 9px', background: '#ffebee', border: '1px solid #ef9a9a', borderRadius: 4, color: '#b71c1c', fontSize: 12 }}>
                                    {err}
                                  </div>
                                )}

                                {!supportsSnapshots && zfsManaged && (
                                  <div style={{ margin: '0 10px 8px', padding: '7px 9px', background: '#fff8e1', borderRadius: 4, fontSize: 12 }}>
                                    {t('tieringInventory.snapshot.crossPoolNote')}
                                  </div>
                                )}

                                {supportsSnapshots && (
                                  <div style={{ margin: '0 10px 10px', border: '1px solid #eee', borderRadius: 4, overflow: 'hidden' }}>
                                    <div style={{ display: 'grid', gridTemplateColumns: '170px 90px 1fr 80px', gap: 8, padding: '6px 8px', background: '#fafafa', color: '#888', fontSize: 11 }}>
                                      <span>{t('snapshots.col.created')}</span><span>{t('tiering.col.consistency')}</span><span>{t('tieringInventory.col.snapshotId')}</span><span></span>
                                    </div>
                                    {busy && snaps.length === 0 ? (
                                      <div style={{ padding: '8px', fontSize: 12, color: '#888' }}>{t('tieringInventory.snapshot.loading')}</div>
                                    ) : snaps.length === 0 ? (
                                      <div style={{ padding: '8px', fontSize: 12, color: '#888' }}>{t('tieringInventory.snapshot.empty')}</div>
                                    ) : (
                                      snaps.map((snap: any) => (
                                        <div key={snap.snapshot_id} style={{ display: 'grid', gridTemplateColumns: '170px 90px 1fr 80px', gap: 8, padding: '7px 8px', alignItems: 'center', borderTop: '1px solid #f0f0f0', fontSize: 12 }}>
                                          <span>{snap.created_at ? new Date(snap.created_at).toLocaleString() : '—'}</span>
                                          <span style={{
                                            width: 'fit-content',
                                            padding: '1px 7px',
                                            borderRadius: 8,
                                            background: snap.consistency === 'atomic' ? '#e8f5e9' : '#ffebee',
                                            color: snap.consistency === 'atomic' ? '#2e7d32' : '#c62828',
                                            border: `1px solid ${snap.consistency === 'atomic' ? '#a5d6a7' : '#ef9a9a'}`,
                                            fontSize: 11,
                                            fontWeight: 600,
                                          }}>
                                            {snap.consistency || t('common.unknown').toLowerCase()}
                                          </span>
                                          <code style={{ fontSize: 11 }}>{snap.snapshot_id}</code>
                                          <button
                                            className="btn danger"
                                            style={{ fontSize: 11, padding: '2px 8px' }}
                                            disabled={busy}
                                            onClick={() => deleteSnapshot(ns, snap.snapshot_id)}
                                          >
                                            {t('common.delete')}
                                          </button>
                                        </div>
                                      ))
                                    )}
                                  </div>
                                )}
                              </div>
                            );
                          })}
                        </div>
                      );
                    })()}
                  </>
                )}
              </div>
            )}
          </div>
        );
      })}

      {/* Active Movements Panel */}
      <div style={{ marginTop: 28 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 10 }}>
          <h2 style={{ margin: 0 }}>{t('tieringInventory.section.activeMovements')}</h2>
          <span style={{
            fontSize: 12, background: activeMovements.length > 0 ? '#e3f2fd' : '#f5f5f5',
            color: activeMovements.length > 0 ? '#1565c0' : '#999',
            borderRadius: 10, padding: '2px 10px', border: '1px solid #ddd'
          }}>{activeMovements.length}</span>
          <button
            className="btn secondary"
            style={{ fontSize: 12, padding: '3px 10px', marginLeft: 'auto' }}
            onClick={() => setShowHistory(v => !v)}
          >
            {showHistory ? t('tieringInventory.button.hideHistory') : t('tieringInventory.button.showHistory')}
          </button>
        </div>

        {activeMovements.length === 0 ? (
          <div style={{ fontSize: 13, color: '#999', padding: '10px 0' }}>{t('volumes.empty.movement')}</div>
        ) : (
          <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
            <div style={{
              display: 'grid',
              gridTemplateColumns: '1fr 1fr 1fr 80px 1fr 100px 70px',
              gap: 6, padding: '6px 12px',
              fontSize: 11, color: '#888', background: '#fafafa', borderBottom: '1px solid #eee'
            }}>
              <span>{t('tieringInventory.col.namespace')}</span><span>{t('tieringInventory.col.source')}</span><span>{t('tieringInventory.col.dest')}</span>
              <span>{t('volumes.col.backend')}</span><span>{t('tieringInventory.col.progress')}</span><span>{t('tieringInventory.col.triggeredBy')}</span><span></span>
            </div>
            {activeMovements.map((job: any) => (
              <div key={job.id} style={{
                display: 'grid',
                gridTemplateColumns: '1fr 1fr 1fr 80px 1fr 100px 70px',
                gap: 6, padding: '8px 12px', fontSize: 13,
                borderBottom: '1px solid #f0f0f0', alignItems: 'center',
              }}>
                <span>{nsName(job.namespace_id)}</span>
                <span style={{ color: '#555' }}>{targetName(job.source_target_id)}</span>
                <span style={{ color: '#555' }}>{targetName(job.dest_target_id)}</span>
                <span style={{ fontSize: 11 }}>{job.backend_kind}</span>
                <span style={{ fontSize: 11, color: '#555' }}>
                  {job.state === 'pending' ? t('tieringInventory.state.pending') : formatProgress(job.progress_bytes, job.total_bytes)}
                </span>
                <span style={{ fontSize: 11, color: '#777' }}>{job.triggered_by || '—'}</span>
                {job.state === 'running' || job.state === 'pending' ? (
                  <button
                    className="btn danger"
                    style={{ fontSize: 11, padding: '2px 8px' }}
                    onClick={() => cancelMovement(job.id)}
                  >
                    {t('common.cancel')}
                  </button>
                ) : (
                  <span style={{ fontSize: 11, color: '#888' }}>{job.state}</span>
                )}
              </div>
            ))}
          </div>
        )}

        {/* Movement history */}
        {showHistory && (
          <div style={{ marginTop: 16 }}>
            <div style={{ fontSize: 13, fontWeight: 600, color: '#555', marginBottom: 8 }}>
              {t('tieringInventory.section.movementHistory')}
            </div>
            {historyMovements.length === 0 ? (
              <div style={{ fontSize: 13, color: '#999' }}>{t('tieringInventory.empty.history')}</div>
            ) : (
              <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
                <div style={{
                  display: 'grid',
                  gridTemplateColumns: '1fr 1fr 1fr 80px 80px 1fr',
                  gap: 6, padding: '6px 12px',
                  fontSize: 11, color: '#888', background: '#fafafa', borderBottom: '1px solid #eee'
                }}>
                  <span>{t('tieringInventory.col.namespace')}</span><span>{t('tieringInventory.col.source')}</span><span>{t('tieringInventory.col.dest')}</span>
                  <span>{t('volumes.col.backend')}</span><span>{t('iscsi.col.state')}</span><span>{t('tieringInventory.col.reasonCompleted')}</span>
                </div>
                {historyMovements.map((job: any) => (
                  <div key={job.id} style={{
                    display: 'grid',
                    gridTemplateColumns: '1fr 1fr 1fr 80px 80px 1fr',
                    gap: 6, padding: '8px 12px', fontSize: 12,
                    borderBottom: '1px solid #f0f0f0', alignItems: 'center',
                    opacity: 0.85,
                  }}>
                    <span>{nsName(job.namespace_id)}</span>
                    <span style={{ color: '#555' }}>{targetName(job.source_target_id)}</span>
                    <span style={{ color: '#555' }}>{targetName(job.dest_target_id)}</span>
                    <span style={{ fontSize: 11 }}>{job.backend_kind}</span>
                    <span style={{
                      fontSize: 11, fontWeight: 600,
                      color: job.state === 'completed' ? '#2e7d32' :
                        job.state === 'failed' ? '#d32f2f' : '#757575',
                    }}>{job.state}</span>
                    <span style={{ fontSize: 11, color: '#777' }}>
                      {job.failure_reason || job.completed_at || job.updated_at || '—'}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Degraded States Panel */}
      <div style={{ marginTop: 28 }}>
        <div
          style={{ display: 'flex', alignItems: 'center', gap: 10, cursor: 'pointer', marginBottom: 10 }}
          onClick={() => setShowDegraded(v => !v)}
        >
          <h2 style={{ margin: 0 }}>{t('tieringInventory.section.degradedStates')}</h2>
          {degraded.length > 0 && (
            <span style={{
              fontSize: 12,
              background: degraded.some((d: any) => d.severity === 'critical') ? '#ffebee' : '#fff8e1',
              color: degraded.some((d: any) => d.severity === 'critical') ? '#d32f2f' : '#f9a825',
              borderRadius: 10, padding: '2px 10px',
              border: `1px solid ${degraded.some((d: any) => d.severity === 'critical') ? '#ef9a9a' : '#ffe082'}`,
              fontWeight: 600,
            }}>{degraded.length}</span>
          )}
          {degraded.length === 0 && (
            <span style={{ fontSize: 12, color: '#2e7d32', background: '#e8f5e9', borderRadius: 10, padding: '2px 10px', border: '1px solid #a5d6a7' }}>
              {t('common.none')}
            </span>
          )}
          <span style={{ fontSize: 13, color: '#999', marginLeft: 4 }}>{showDegraded ? '▼' : '▶'}</span>
        </div>

        {showDegraded && (
          degraded.length === 0 ? (
            <div style={{ fontSize: 13, color: '#999', padding: '6px 0' }}>{t('tieringInventory.empty.degraded')}</div>
          ) : (
            <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
              <div style={{
                display: 'grid',
                gridTemplateColumns: '80px 100px 80px 80px 120px 1fr 120px',
                gap: 6, padding: '6px 12px',
                fontSize: 11, color: '#888', background: '#fafafa', borderBottom: '1px solid #eee'
              }}>
                <span>{t('volumes.col.backend')}</span><span>{t('tieringInventory.label.domain')}</span><span>{t('tieringInventory.col.scopeKind')}</span>
                <span>{t('dashboard.alerts.severity')}</span><span>{t('tieringInventory.col.code')}</span><span>{t('tieringInventory.col.message')}</span><span>{t('tieringInventory.col.updated')}</span>
              </div>
              {degraded.map((d: any) => {
                const tgt = targets.find((t: any) => t.id === d.scope_id);
                const domain = tgt?.placement_domain || '—';
                return (
                  <div key={d.id} style={{
                    display: 'grid',
                    gridTemplateColumns: '80px 100px 80px 80px 120px 1fr 120px',
                    gap: 6, padding: '8px 12px', fontSize: 12,
                    borderBottom: '1px solid #f0f0f0', alignItems: 'center',
                    background: d.severity === 'critical' ? '#fff5f5' : '#fffde7',
                  }}>
                    <span style={{ fontSize: 11 }}>{d.backend_kind}</span>
                    <span style={{ fontSize: 11 }}>{domain}</span>
                    <span style={{ fontSize: 11, color: '#555' }}>{d.scope_kind}</span>
                    <span style={{
                      fontSize: 11, fontWeight: 600,
                      color: d.severity === 'critical' ? '#d32f2f' : '#f9a825',
                    }}>{d.severity}</span>
                    <code style={{ fontSize: 11 }}>{d.code}</code>
                    <span style={{ fontSize: 12 }}>{d.message}</span>
                    <span style={{ fontSize: 11, color: '#888' }}>{d.updated_at ? new Date(d.updated_at).toLocaleString() : '—'}</span>
                  </div>
                );
              })}
            </div>
          )
        )}
      </div>
    </div>
  );
}
