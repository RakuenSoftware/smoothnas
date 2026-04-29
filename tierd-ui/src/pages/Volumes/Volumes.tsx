import { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { useToast } from '../../contexts/ToastContext';
import Spinner from '../../components/Spinner/Spinner';
import { extractError } from '../../utils/errors';

type Namespace = {
  id: string;
  name: string;
  placement_domain: string;
  backend_kind: string;
  namespace_kind: string;
  exposed_path?: string;
  pin_state?: string;
  health?: string;
  placement_state?: string;
  backend_ref?: string;
  capacity_bytes?: number;
  used_bytes?: number;
  snapshot_mode?: string;
};

type Target = {
  id: string;
  name: string;
  placement_domain: string;
  backend_kind: string;
  rank: number;
  target_fill_pct?: number;
  full_threshold_pct?: number;
  health?: string;
  activity_band?: string;
};

type Movement = {
  id: string;
  namespace_id: string;
  source_target_id: string;
  dest_target_id: string;
  backend_kind: string;
  state: string;
  triggered_by?: string;
  progress_bytes?: number;
  total_bytes?: number;
  failure_reason?: string;
  updated_at?: string;
};

type NamespaceFile = {
  path: string;
  size: number;
  inode: number;
  tier_rank: number;
  pin_state: string;
};

function formatBytes(n: number | undefined): string {
  if (!n || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function fillPct(ns: Namespace): number | null {
  if (!ns.capacity_bytes || ns.capacity_bytes <= 0) return null;
  return Math.round(((ns.used_bytes || 0) / ns.capacity_bytes) * 1000) / 10;
}

function movementProgress(job: Movement): string {
  if (job.state === 'pending') return 'pending';
  if (!job.total_bytes) return job.state;
  const pct = Math.round(((job.progress_bytes || 0) / job.total_bytes) * 100);
  return `${formatBytes(job.progress_bytes)} / ${formatBytes(job.total_bytes)} (${pct}%)`;
}

function stateColor(state: string | undefined): string {
  switch (state) {
    case 'healthy':
    case 'placed':
    case 'completed':
      return '#2e7d32';
    case 'degraded':
    case 'pending':
    case 'running':
      return '#e65100';
    case 'failed':
    case 'critical':
      return '#c62828';
    default:
      return '#555';
  }
}

function rankColor(rank: number): string {
  if (rank <= 1) return '#c62828';
  if (rank === 2) return '#f57c00';
  return '#1565c0';
}

export default function Volumes() {
  const { t } = useI18n();
  const { id } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [operationError, setOperationError] = useState('');
  const [namespaces, setNamespaces] = useState<Namespace[]>([]);
  const [targets, setTargets] = useState<Target[]>([]);
  const [movements, setMovements] = useState<Movement[]>([]);
  const [files, setFiles] = useState<NamespaceFile[]>([]);
  const [filesLoading, setFilesLoading] = useState(false);
  const [busyNamespace, setBusyNamespace] = useState('');

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load(); }, []);

  const selected = useMemo(
    () => namespaces.find(ns => ns.id === id),
    [id, namespaces]
  );

  useEffect(() => {
    if (!selected) {
      setFiles([]);
      return;
    }
    setFilesLoading(true);
    api.listNamespaceFiles(selected.id, '', 200)
      .then(rows => setFiles(rows || []))
      .catch(e => {
        const msg = extractError(e, t('volumes.error.loadFiles'));
        setOperationError(msg);
      })
      .finally(() => setFilesLoading(false));
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected?.id]);

  function load() {
    setLoading(true);
    setError('');
    setOperationError('');
    Promise.all([
      api.getTieringNamespaces(),
      api.getTieringTargets(),
      api.getTieringMovements(),
    ]).then(([ns, ts, jobs]) => {
      const sorted = (ns || []).sort((a: Namespace, b: Namespace) => a.name.localeCompare(b.name));
      setNamespaces(sorted);
      setTargets(ts || []);
      setMovements(jobs || []);
      setLoading(false);
    }).catch(e => {
      const msg = extractError(e, t('volumes.error.load'));
      setError(msg);
      toast.error(msg);
      setLoading(false);
    });
  }

  function updateNamespace(updated: Namespace) {
    setNamespaces(prev => prev.map(ns => ns.id === updated.id ? { ...ns, ...updated } : ns));
  }

  function pinNamespace(ns: Namespace, pinState: 'pinned-hot' | 'pinned-cold') {
    setBusyNamespace(ns.id);
    setOperationError('');
    api.pinTieringNamespace(ns.id, pinState)
      .then(updated => {
        updateNamespace(updated as Namespace);
        toast.success(t('volumes.toast.pinUpdated', { name: ns.name }));
      })
      .catch(e => {
        const msg = extractError(e, t('volumes.error.pin'));
        setOperationError(msg);
        toast.error(msg);
      })
      .finally(() => setBusyNamespace(''));
  }

  function unpinNamespace(ns: Namespace) {
    setBusyNamespace(ns.id);
    setOperationError('');
    api.unpinTieringNamespace(ns.id)
      .then(updated => {
        updateNamespace(updated as Namespace);
        toast.success(t('volumes.toast.unpinned', { name: ns.name }));
      })
      .catch(e => {
        const msg = extractError(e, t('volumes.error.unpin'));
        setOperationError(msg);
        toast.error(msg);
      })
      .finally(() => setBusyNamespace(''));
  }

  function targetName(targetID: string): string {
    return targets.find(t => t.id === targetID)?.name || targetID.slice(0, 8);
  }

  function domainTargets(domainID: string): Target[] {
    return targets
      .filter(t => t.placement_domain === domainID)
      .sort((a, b) => a.rank - b.rank);
  }

  function activeMovementsFor(nsID: string): Movement[] {
    return movements.filter(job =>
      job.namespace_id === nsID && (job.state === 'pending' || job.state === 'running')
    );
  }

  const volumeCount = namespaces.length;
  const pinnedCount = namespaces.filter(ns => ns.pin_state && ns.pin_state !== 'none').length;
  const activeMovementCount = movements.filter(job => job.state === 'pending' || job.state === 'running').length;

  const fileSummary = useMemo(() => {
    const byRank = new Map<number, { bytes: number; count: number }>();
    for (const file of files) {
      const current = byRank.get(file.tier_rank) || { bytes: 0, count: 0 };
      current.bytes += file.size || 0;
      current.count += 1;
      byRank.set(file.tier_rank, current);
    }
    const totalBytes = files.reduce((sum, file) => sum + (file.size || 0), 0);
    return Array.from(byRank.entries())
      .sort(([a], [b]) => a - b)
      .map(([rank, summary]) => ({ rank, ...summary, pct: totalBytes ? (summary.bytes / totalBytes) * 100 : 0 }));
  }, [files]);

  if (loading) return <div className="page"><Spinner loading /></div>;

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('volumes.title')}</h1>
        <p className="subtitle">{t('volumes.subtitle')}</p>
        <button className="refresh-btn" onClick={load}>{t('common.refresh')}</button>
      </div>

      {error && <div className="error-msg">{error}</div>}
      {operationError && <div className="error-msg">{operationError}</div>}

      <div className="cards">
        <div className="card">
          <div className="card-label">{t('volumes.card.volumes')}</div>
          <div className="card-value">{volumeCount}</div>
          <div className="card-detail">{t('volumes.card.volumesDetail')}</div>
        </div>
        <div className="card">
          <div className="card-label">{t('volumes.card.pinned')}</div>
          <div className="card-value">{pinnedCount}</div>
          <div className="card-detail">{t('volumes.card.pinnedDetail')}</div>
        </div>
        <div className="card">
          <div className="card-label">{t('volumes.card.movement')}</div>
          <div className="card-value">{activeMovementCount}</div>
          <div className="card-detail">{t('volumes.card.movementDetail')}</div>
        </div>
        <div className="card">
          <div className="card-label">{t('volumes.card.lifecycle')}</div>
          <div className="card-value" style={{ fontSize: 18 }}>smoothfs</div>
          <div className="card-detail">
            <Link to="/tiers">{t('nav.tiers')}</Link> / <Link to="/smoothfs-pools">{t('nav.smoothfsPools')}</Link> / <Link to="/sharing">{t('nav.sharing')}</Link>
          </div>
        </div>
      </div>

      {namespaces.length === 0 ? (
        <div className="empty-state">{t('volumes.empty')}</div>
      ) : (
        <div className="section">
          <h2>{t('volumes.section.list')}</h2>
          <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflowX: 'auto' }}>
            <div style={{ minWidth: 900 }}>
            <div style={{
              display: 'grid',
              gridTemplateColumns: '1.5fr 120px 130px 100px 100px 130px 170px',
              gap: 8,
              padding: '7px 12px',
              fontSize: 11,
              color: '#888',
              background: '#fafafa',
              borderBottom: '1px solid #eee',
            }}>
              <span>{t('datasets.col.name')}</span><span>{t('volumes.col.backend')}</span><span>{t('volumes.col.domain')}</span><span>{t('volumes.col.health')}</span><span>{t('volumes.col.fill')}</span><span>{t('volumes.col.pin')}</span><span></span>
            </div>
            {namespaces.map(ns => {
              const pct = fillPct(ns);
              const busy = busyNamespace === ns.id;
              const isSelected = selected?.id === ns.id;
              return (
                <div key={ns.id} style={{
                  display: 'grid',
                  gridTemplateColumns: '1.5fr 120px 130px 100px 100px 130px 170px',
                  gap: 8,
                  padding: '9px 12px',
                  fontSize: 13,
                  alignItems: 'center',
                  borderBottom: '1px solid #f0f0f0',
                  background: isSelected ? '#f8fbff' : '#fff',
                }}>
                  <div>
                    <div style={{ fontWeight: 600 }}>{ns.name}</div>
                    <code style={{ fontSize: 11 }}>{ns.exposed_path || ns.backend_ref || ns.id}</code>
                  </div>
                  <span>{ns.backend_kind || t('common.unknown').toLowerCase()}</span>
                  <span style={{ fontSize: 12, color: '#555' }}>{ns.placement_domain || t('common.none').toLowerCase()}</span>
                  <span style={{ color: stateColor(ns.health), fontWeight: 600 }}>{ns.health || t('common.unknown').toLowerCase()}</span>
                  <span>{pct == null ? t('common.na') : `${pct}%`}</span>
                  <span style={{ fontSize: 12 }}>{ns.pin_state || t('common.none').toLowerCase()}</span>
                  <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
                    <button
                      className="btn secondary"
                      style={{ fontSize: 11, padding: '2px 8px' }}
                      onClick={() => navigate(`/volumes/${encodeURIComponent(ns.id)}`)}
                    >
                      {t('volumes.button.details')}
                    </button>
                    {ns.pin_state && ns.pin_state !== 'none' ? (
                      <button
                        style={{ fontSize: 11, padding: '2px 8px' }}
                        disabled={busy}
                        onClick={() => unpinNamespace(ns)}
                      >
                        {t('volumes.button.unpin')}
                      </button>
                    ) : (
                      <button
                        style={{ fontSize: 11, padding: '2px 8px' }}
                        disabled={busy}
                        onClick={() => pinNamespace(ns, 'pinned-hot')}
                      >
                        {t('volumes.button.pinHot')}
                      </button>
                    )}
                  </div>
                </div>
              );
            })}
            </div>
          </div>
        </div>
      )}

      {selected && (
        <div className="section">
          <h2>{selected.name}</h2>
          <div className="cards">
            <div className="card">
              <div className="card-label">{t('volumes.detail.placement')}</div>
              <div className="card-value" style={{ color: stateColor(selected.placement_state), fontSize: 22 }}>
                {selected.placement_state || t('common.unknown').toLowerCase()}
              </div>
              <div className="card-detail">{selected.backend_ref || selected.placement_domain}</div>
            </div>
            <div className="card">
              <div className="card-label">{t('volumes.detail.capacity')}</div>
              <div className="card-value" style={{ fontSize: 22 }}>{formatBytes(selected.used_bytes)}</div>
              <div className="card-detail">{t('volumes.detail.totalSuffix', { total: formatBytes(selected.capacity_bytes) })}</div>
            </div>
            <div className="card">
              <div className="card-label">{t('volumes.detail.snapshotMode')}</div>
              <div className="card-value" style={{ fontSize: 18 }}>{selected.snapshot_mode || t('common.none').toLowerCase()}</div>
              <div className="card-detail"><Link to="/tiering">{t('dashboard.link.tieringInventory')}</Link></div>
            </div>
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) minmax(280px, 420px)', gap: 16 }}>
            <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflowX: 'auto' }}>
              <div style={{ minWidth: 560 }}>
              <div style={{ padding: '8px 12px', background: '#fafafa', borderBottom: '1px solid #eee', fontWeight: 600 }}>
                {t('volumes.section.filePlacement')}
              </div>
              {filesLoading ? (
                <div style={{ padding: 14 }}><Spinner loading /></div>
              ) : files.length === 0 ? (
                <div style={{ padding: 12, color: '#777', fontSize: 13 }}>{t('volumes.empty.files')}</div>
              ) : (
                <>
                  <div style={{ padding: 12 }}>
                    {fileSummary.map(item => (
                      <div key={item.rank} style={{ marginBottom: 8 }}>
                        <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginBottom: 3 }}>
                          <span>{t('volumes.summary.tierRank', { rank: item.rank })}</span>
                          <span>{t('volumes.summary.tierCount', { count: item.count, bytes: formatBytes(item.bytes) })}</span>
                        </div>
                        <div style={{ height: 8, borderRadius: 4, background: '#f1f1f1', overflow: 'hidden' }}>
                          <div style={{ width: `${Math.max(item.pct, 2)}%`, height: '100%', background: rankColor(item.rank) }} />
                        </div>
                      </div>
                    ))}
                  </div>
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 90px 80px 110px', gap: 8, padding: '6px 12px', background: '#fafafa', color: '#888', fontSize: 11, borderTop: '1px solid #eee', borderBottom: '1px solid #eee' }}>
                    <span>{t('iscsi.col.path')}</span><span>{t('volumes.col.size')}</span><span>{t('volumes.col.tier')}</span><span>{t('volumes.col.pin')}</span>
                  </div>
                  {files.slice(0, 25).map(file => (
                    <div key={`${file.inode}-${file.path}`} style={{ display: 'grid', gridTemplateColumns: '1fr 90px 80px 110px', gap: 8, padding: '7px 12px', alignItems: 'center', fontSize: 12, borderBottom: '1px solid #f0f0f0' }}>
                      <code style={{ fontSize: 11, overflow: 'hidden', textOverflow: 'ellipsis' }}>{file.path}</code>
                      <span>{formatBytes(file.size)}</span>
                      <span style={{ color: rankColor(file.tier_rank), fontWeight: 600 }}>#{file.tier_rank}</span>
                      <span>{file.pin_state || t('common.none').toLowerCase()}</span>
                    </div>
                  ))}
                </>
              )}
              </div>
            </div>

            <div style={{ border: '1px solid #e0e0e0', borderRadius: 6, overflowX: 'auto' }}>
              <div style={{ minWidth: 320 }}>
              <div style={{ padding: '8px 12px', background: '#fafafa', borderBottom: '1px solid #eee', fontWeight: 600 }}>
                {t('volumes.section.movement')}
              </div>
              {activeMovementsFor(selected.id).length === 0 ? (
                <div style={{ padding: 12, color: '#777', fontSize: 13 }}>{t('volumes.empty.movement')}</div>
              ) : activeMovementsFor(selected.id).map(job => (
                <div key={job.id} style={{ padding: 12, borderBottom: '1px solid #f0f0f0', fontSize: 13 }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, marginBottom: 4 }}>
                    <strong style={{ color: stateColor(job.state) }}>{job.state}</strong>
                    <span style={{ fontSize: 12, color: '#777' }}>{job.backend_kind}</span>
                  </div>
                  <div>{t('volumes.movement.fromTo', { from: targetName(job.source_target_id), to: targetName(job.dest_target_id) })}</div>
                  <div style={{ fontSize: 12, color: '#555', marginTop: 4 }}>{movementProgress(job)}</div>
                  {job.failure_reason && <div className="error-msg" style={{ marginTop: 8 }}>{job.failure_reason}</div>}
                </div>
              ))}

              <div style={{ padding: '8px 12px', background: '#fafafa', borderTop: '1px solid #eee', borderBottom: '1px solid #eee', fontWeight: 600 }}>
                {t('volumes.section.policyTargets')}
              </div>
              {domainTargets(selected.placement_domain).length === 0 ? (
                <div style={{ padding: 12, color: '#777', fontSize: 13 }}>{t('volumes.empty.targets')}</div>
              ) : domainTargets(selected.placement_domain).map(target => (
                <div key={target.id} style={{ display: 'grid', gridTemplateColumns: '1fr 60px 70px', gap: 8, padding: '7px 12px', alignItems: 'center', borderBottom: '1px solid #f0f0f0', fontSize: 12 }}>
                  <div>
                    <strong>{target.name}</strong>
                    <div style={{ color: '#777' }}>{target.activity_band || target.health || t('common.unknown').toLowerCase()}</div>
                  </div>
                  <span>#{target.rank}</span>
                  <span>{target.target_fill_pct ?? t('common.na')}% / {target.full_threshold_pct ?? t('common.na')}%</span>
                </div>
              ))}
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
