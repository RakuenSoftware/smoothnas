import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
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

export default function TieringInventory() {
  const toast = useToast();
  const navigate = useNavigate();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [domains, setDomains] = useState<any[]>([]);
  const [targets, setTargets] = useState<any[]>([]);
  const [movements, setMovements] = useState<any[]>([]);
  const [degraded, setDegraded] = useState<any[]>([]);
  const [namespaces, setNamespaces] = useState<any[]>([]);

  // UI state
  const [collapsedDomains, setCollapsedDomains] = useState<Set<string>>(new Set());
  const [expandedTargets, setExpandedTargets] = useState<Set<string>>(new Set());
  const [domainFilter, setDomainFilter] = useState('');
  const [showHistory, setShowHistory] = useState(false);
  const [showDegraded, setShowDegraded] = useState(true);

  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);

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
    }).catch(e => {
      const msg = extractError(e, 'Failed to load tiering inventory');
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
    setConfirmTitle('Cancel Movement');
    setConfirmMessage('Cancel this movement job? The source will remain authoritative and any partial copy will be cleaned up.');
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.cancelTieringMovement(id).then(() => {
        toast.success('Movement cancelled');
        setMovements(prev => prev.map(j => j.id === id ? { ...j, state: 'cancelled' } : j));
      }).catch(e => toast.error(extractError(e, 'Failed to cancel movement')));
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
      return { label: 'Manage in ZFS', path: '/pools' };
    }
    return { label: 'Manage in mdadm', path: '/tiers' };
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
        confirmText="Cancel Movement"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />

      <div className="page-header">
        <h1>Tiering Inventory</h1>
        <p className="subtitle">
          Unified view of all tier targets across mdadm and managed ZFS backends, grouped by placement domain.
        </p>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <button className="refresh-btn" onClick={load}>Refresh</button>
          {domains.length > 0 && (
            <select
              value={domainFilter}
              onChange={e => setDomainFilter(e.target.value)}
              style={{ fontSize: 13, padding: '4px 8px', borderRadius: 4, border: '1px solid #ccc' }}
            >
              <option value="">All domains</option>
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
        <div className="empty-state">No placement domains registered yet. Backend adapters register domains when tier targets are created.</div>
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
                Domain
              </span>
              <span style={{ fontWeight: 600, fontSize: 15 }}>{domain.id}</span>
              <span style={{
                fontSize: 11, background: '#e3f2fd', color: '#1565c0',
                borderRadius: 8, padding: '1px 7px', border: '1px solid #90caf9'
              }}>{domain.backend_kind}</span>
              {criticalCount > 0 && (
                <span style={{ fontSize: 12, color: '#d32f2f', fontWeight: 700 }}>
                  ● {criticalCount} critical
                </span>
              )}
              {criticalCount === 0 && warningCount > 0 && (
                <span style={{ fontSize: 12, color: '#f9a825', fontWeight: 700 }}>
                  ● {warningCount} warning
                </span>
              )}
              <span style={{ marginLeft: 'auto', fontSize: 12, color: '#888' }}>
                {domainTargets.length} target{domainTargets.length !== 1 ? 's' : ''}
              </span>
              <span style={{ fontSize: 13, color: '#999', marginLeft: 4 }}>{collapsed ? '▶' : '▼'}</span>
            </div>

            {/* Targets */}
            {!collapsed && (
              <div>
                {domainTargets.length === 0 ? (
                  <div style={{ padding: '10px 14px', fontSize: 13, color: '#999' }}>
                    No targets in this domain.
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
                      <span>Name</span>
                      <span>Backend</span>
                      <span>Rank</span>
                      <span>Fill%</span>
                      <span>Full%</span>
                      <span>Health</span>
                      <span>Activity</span>
                      <span>Queue</span>
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
                                {isExpanded ? 'Less' : 'More'}
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
                            <CapabilityBadge label="move" value={caps.movement_granularity} />
                            <CapabilityBadge label="pin" value={caps.pin_scope} />
                            <CapabilityBadge label="recall" value={caps.recall_mode && caps.recall_mode !== 'none' ? caps.recall_mode : undefined} />
                            {caps.fuse_mode && caps.fuse_mode !== 'n/a' && (
                              <CapabilityBadge label="FUSE" value={caps.fuse_mode} />
                            )}
                            {caps.snapshot_mode && caps.snapshot_mode !== 'none' && (
                              <CapabilityBadge label="snapshot" value={caps.snapshot_mode} />
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
                                <div><span style={{ color: '#888' }}>Movement granularity:</span> {caps.movement_granularity || '—'}</div>
                                <div><span style={{ color: '#888' }}>Recall mode:</span> {caps.recall_mode || '—'}</div>
                                <div><span style={{ color: '#888' }}>FUSE mode:</span> {caps.fuse_mode || '—'}</div>
                                <div><span style={{ color: '#888' }}>Snapshot mode:</span> {caps.snapshot_mode || 'none'}</div>
                                <div><span style={{ color: '#888' }}>Checksums:</span> {caps.supports_checksums ? 'yes' : 'no'}</div>
                                <div><span style={{ color: '#888' }}>Compression:</span> {caps.supports_compression ? 'yes' : 'no'}</div>
                                <div><span style={{ color: '#888' }}>Online move:</span> {caps.supports_online_move ? 'yes' : 'no'}</div>
                                <div><span style={{ color: '#888' }}>Pin scope:</span> {caps.pin_scope || '—'}</div>
                                {target.backing_ref && (
                                  <div style={{ gridColumn: '1 / -1' }}>
                                    <span style={{ color: '#888' }}>Backing ref:</span> <code>{target.backing_ref}</code>
                                  </div>
                                )}
                                {targetDeg.length > 0 && (
                                  <div style={{ gridColumn: '1 / -1', marginTop: 6 }}>
                                    <div style={{ fontWeight: 600, color: '#c62828', marginBottom: 4 }}>Active degraded states</div>
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
                                {caps.snapshot_mode === 'none' && target.backend_kind !== 'zfsmgd' && (
                                  <div style={{ gridColumn: '1 / -1', color: '#888', fontStyle: 'italic', marginTop: 4 }}>
                                    Snapshot button will appear after proposal 06 ships.
                                  </div>
                                )}
                                {caps.snapshot_mode === 'none' && (target.backend_kind === 'zfsmgd' || target.backend_kind === 'zfs-managed') && (
                                  <div style={{ gridColumn: '1 / -1', padding: '6px 10px', background: '#fff8e1', borderRadius: 4, marginTop: 4 }}>
                                    Coordinated snapshots require all tier datasets to be in the same ZFS pool.
                                  </div>
                                )}
                              </div>
                            </div>
                          )}
                        </div>
                      );
                    })}
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
          <h2 style={{ margin: 0 }}>Active Movements</h2>
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
            {showHistory ? 'Hide history' : 'Show history (last 30 days)'}
          </button>
        </div>

        {activeMovements.length === 0 ? (
          <div style={{ fontSize: 13, color: '#999', padding: '10px 0' }}>No active movement jobs.</div>
        ) : (
          <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
            <div style={{
              display: 'grid',
              gridTemplateColumns: '1fr 1fr 1fr 80px 1fr 100px 70px',
              gap: 6, padding: '6px 12px',
              fontSize: 11, color: '#888', background: '#fafafa', borderBottom: '1px solid #eee'
            }}>
              <span>Namespace</span><span>Source</span><span>Dest</span>
              <span>Backend</span><span>Progress</span><span>Triggered by</span><span></span>
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
                  {job.state === 'pending' ? 'pending' : formatProgress(job.progress_bytes, job.total_bytes)}
                </span>
                <span style={{ fontSize: 11, color: '#777' }}>{job.triggered_by || '—'}</span>
                {job.state === 'running' || job.state === 'pending' ? (
                  <button
                    className="btn danger"
                    style={{ fontSize: 11, padding: '2px 8px' }}
                    onClick={() => cancelMovement(job.id)}
                  >
                    Cancel
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
              Movement History (last 30 days)
            </div>
            {historyMovements.length === 0 ? (
              <div style={{ fontSize: 13, color: '#999' }}>No completed movement jobs in the last 30 days.</div>
            ) : (
              <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
                <div style={{
                  display: 'grid',
                  gridTemplateColumns: '1fr 1fr 1fr 80px 80px 1fr',
                  gap: 6, padding: '6px 12px',
                  fontSize: 11, color: '#888', background: '#fafafa', borderBottom: '1px solid #eee'
                }}>
                  <span>Namespace</span><span>Source</span><span>Dest</span>
                  <span>Backend</span><span>State</span><span>Reason / completed</span>
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
          <h2 style={{ margin: 0 }}>Degraded States</h2>
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
              None
            </span>
          )}
          <span style={{ fontSize: 13, color: '#999', marginLeft: 4 }}>{showDegraded ? '▼' : '▶'}</span>
        </div>

        {showDegraded && (
          degraded.length === 0 ? (
            <div style={{ fontSize: 13, color: '#999', padding: '6px 0' }}>No active degraded states.</div>
          ) : (
            <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflow: 'hidden' }}>
              <div style={{
                display: 'grid',
                gridTemplateColumns: '80px 100px 80px 80px 120px 1fr 120px',
                gap: 6, padding: '6px 12px',
                fontSize: 11, color: '#888', background: '#fafafa', borderBottom: '1px solid #eee'
              }}>
                <span>Backend</span><span>Domain</span><span>Scope kind</span>
                <span>Severity</span><span>Code</span><span>Message</span><span>Updated</span>
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
