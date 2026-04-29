import { useEffect, useRef, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';
import Datasets from '../Datasets/Datasets';
import Zvols from '../Zvols/Zvols';
import Snapshots from '../Snapshots/Snapshots';

type Tab = 'pools' | 'datasets' | 'zvols' | 'snapshots';

export default function Zfs() {
  const { pools, disks, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState<Tab>('pools');
  const [showCreate, setShowCreate] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [newPool, setNewPool] = useState({ name: 'tank', vdev_type: 'raidz1' });
  const [selectedData, setSelectedData] = useState<string[]>([]);
  const [selectedSlog, setSelectedSlog] = useState<string[]>([]);
  const [selectedL2arc, setSelectedL2arc] = useState<string[]>([]);
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const [spindownByPool, setSpindownByPool] = useState<Record<string, any>>({});
  const [spindownBusy, setSpindownBusy] = useState<Record<string, boolean>>({});
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  const unassignedDisks = disks.filter((d: any) => d.assignment === 'unassigned');

  useEffect(() => {
    if (pools !== undefined) {
      setLoading(false);
      refreshSpindown(pools);
    }
  }, [pools]);
  useEffect(() => () => { stopPollRef.current?.(); }, []);

  function refresh() { invalidate('pools'); invalidate('disks'); }

  function toggleDisk(path: string, role: 'data' | 'slog' | 'l2arc') {
    const set = role === 'data' ? setSelectedData : role === 'slog' ? setSelectedSlog : setSelectedL2arc;
    set(prev => prev.includes(path) ? prev.filter(p => p !== path) : [...prev, path]);
  }

  function pollJob(jobId: string, onComplete: () => void, onError: (err: string) => void): () => void {
    const timer = setInterval(() => {
      api.getJobStatus(jobId).then((job: any) => {
        if (job.status === 'completed') { clearInterval(timer); onComplete(); }
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || 'Job failed'); }
      }).catch(() => { clearInterval(timer); onError('Lost connection'); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function createPool() {
    const data = {
      name: newPool.name, vdev_type: newPool.vdev_type === 'stripe' ? '' : newPool.vdev_type,
      data_disks: selectedData,
      slog_disks: selectedSlog,
      l2arc_disks: selectedL2arc,
    };
    setSubmitting(true);
    api.createPool(data).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        setSelectedData([]); setSelectedSlog([]); setSelectedL2arc([]);
        toast.success('Pool created');
        invalidate('pools'); invalidate('disks');
      }, (err) => {
        setSubmitting(false);
        toast.error('Failed to create pool: ' + err);
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, 'Failed to create pool'));
    });
  }

  function deletePool(name: string) {
    setConfirmTitle('Destroy Pool');
    setConfirmMessage(`This will permanently destroy pool "${name}" and all its datasets, zvols, and snapshots. This cannot be undone.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deletePool(name).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success('Pool destroyed');
          invalidate('pools');
        }, (err) => toast.error('Destroy failed: ' + err));
      }).catch(e => toast.error(extractError(e, 'Failed to destroy pool')));
    };
    setConfirmVisible(true);
  }

  function scrub(name: string) {
    api.scrubPool(name).then(() => toast.info('Scrub started on ' + name))
      .catch(e => toast.error(extractError(e, 'Failed to start scrub')));
  }

  function refreshSpindown(poolList = pools) {
    Promise.all(
      (poolList || []).map((p: any) =>
        api.getPoolSpindown(p.name)
          .then((policy: any) => ({ name: p.name, policy }))
          .catch(() => ({ name: p.name, policy: null }))
      )
    ).then(rows => {
      setSpindownByPool(prev => {
        const next = { ...prev };
        for (const row of rows) {
          if (row.policy) next[row.name] = row.policy;
        }
        return next;
      });
    });
  }

  function setRawPoolSpindown(poolName: string, enabled: boolean, activeWindows?: any[]) {
    setSpindownBusy(prev => ({ ...prev, [poolName]: true }));
    api.setPoolSpindown(poolName, enabled, activeWindows)
      .then((policy: any) => {
        setSpindownByPool(prev => ({ ...prev, [poolName]: policy }));
        toast.success(enabled ? 'Pool spindown enabled' : 'Pool spindown disabled');
      })
      .catch(e => toast.error(extractError(e, 'Failed to update pool spindown')))
      .finally(() => setSpindownBusy(prev => ({ ...prev, [poolName]: false })));
  }

  return (
    <>
      <div className="tabs">
        {(['pools', 'datasets', 'zvols', 'snapshots'] as Tab[]).map(tab => (
          <button key={tab} className={`tab${activeTab === tab ? ' active' : ''}`} onClick={() => setActiveTab(tab)}>
            {tab.charAt(0).toUpperCase() + tab.slice(1)}
          </button>
        ))}
      </div>

      {activeTab === 'pools' && (
        <>
          <div style={{ marginBottom: 16 }}>
            <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Create Pool</button>
          </div>
          {showCreate && (
            <div className="create-form">
              <h3>Create Pool</h3>
              <div className="form-row">
                <label>Name <input value={newPool.name} onChange={e => setNewPool(p => ({ ...p, name: e.target.value }))} /></label>
                <label>vdev Type
                  <select value={newPool.vdev_type} onChange={e => setNewPool(p => ({ ...p, vdev_type: e.target.value }))}>
                    <option value="mirror">Mirror</option>
                    <option value="raidz1">RAIDZ-1</option>
                    <option value="raidz2">RAIDZ-2</option>
                    <option value="raidz3">RAIDZ-3</option>
                    <option value="stripe">Stripe</option>
                  </select>
                </label>
              </div>
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>Data disks (required)</div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
                {unassignedDisks.map((disk: any) => {
                  const usedElsewhere = selectedSlog.includes(disk.path) || selectedL2arc.includes(disk.path);
                  return (
                    <label key={disk.path} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px', border: `2px solid ${selectedData.includes(disk.path) ? '#4fc3f7' : '#ddd'}`, borderRadius: 6, cursor: usedElsewhere ? 'not-allowed' : 'pointer', background: selectedData.includes(disk.path) ? '#e3f2fd' : '#fff', opacity: usedElsewhere ? 0.4 : 1 }}>
                      <input type="checkbox" checked={selectedData.includes(disk.path)} disabled={usedElsewhere} onChange={() => toggleDisk(disk.path, 'data')} />
                      <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
                    </label>
                  );
                })}
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>No unassigned disks available.</p>}
              </div>
              {selectedData.length > 0 && <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{selectedData.length} data disk(s) selected</p>}
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>SLOG disks — write cache (optional)</div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
                {unassignedDisks.map((disk: any) => {
                  const usedElsewhere = selectedData.includes(disk.path) || selectedL2arc.includes(disk.path);
                  return (
                    <label key={disk.path} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px', border: `2px solid ${selectedSlog.includes(disk.path) ? '#81c784' : '#ddd'}`, borderRadius: 6, cursor: usedElsewhere ? 'not-allowed' : 'pointer', background: selectedSlog.includes(disk.path) ? '#e8f5e9' : '#fff', opacity: usedElsewhere ? 0.4 : 1 }}>
                      <input type="checkbox" checked={selectedSlog.includes(disk.path)} disabled={usedElsewhere} onChange={() => toggleDisk(disk.path, 'slog')} />
                      <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
                    </label>
                  );
                })}
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>No unassigned disks available.</p>}
              </div>
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>L2ARC disks — read cache (optional)</div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
                {unassignedDisks.map((disk: any) => {
                  const usedElsewhere = selectedData.includes(disk.path) || selectedSlog.includes(disk.path);
                  return (
                    <label key={disk.path} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px', border: `2px solid ${selectedL2arc.includes(disk.path) ? '#ffb74d' : '#ddd'}`, borderRadius: 6, cursor: usedElsewhere ? 'not-allowed' : 'pointer', background: selectedL2arc.includes(disk.path) ? '#fff3e0' : '#fff', opacity: usedElsewhere ? 0.4 : 1 }}>
                      <input type="checkbox" checked={selectedL2arc.includes(disk.path)} disabled={usedElsewhere} onChange={() => toggleDisk(disk.path, 'l2arc')} />
                      <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
                    </label>
                  );
                })}
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>No unassigned disks available.</p>}
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button className="btn secondary" onClick={() => { setShowCreate(false); setSelectedData([]); setSelectedSlog([]); setSelectedL2arc([]); }} disabled={submitting}>Cancel</button>
                <button className="btn primary" onClick={createPool} disabled={submitting || selectedData.length === 0}>
                  {submitting ? 'Creating...' : 'Create'}
                </button>
              </div>
            </div>
          )}
          <Spinner loading={loading} text="Loading pools..." />
          {!loading && (
            pools.length === 0 ? (
              <div className="empty-state"><div className="empty-icon">○</div><p>No ZFS pools configured.</p></div>
            ) : (
              <table className="data-table">
                <thead>
                  <tr><th>Pool</th><th>State</th><th>Size</th><th>Used</th><th>Free</th><th>Actions</th></tr>
                </thead>
                <tbody>
                  {pools.map((pool: any) => {
                    const policy = spindownByPool[pool.name];
                    const busy = !!spindownBusy[pool.name];
                    return (
                    <tr key={pool.name}>
                      <td>
                        <strong>{pool.name}</strong>
                        {policy && (
                          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginTop: 4 }}>
                            <span className={`badge ${policy.enabled ? 'online' : policy.eligible ? '' : 'degraded'}`}>
                              spindown {policy.enabled ? 'on' : policy.eligible ? 'eligible' : 'blocked'}
                            </span>
                            {policy.enabled && policy.active_windows?.length > 0 && (
                              <span className={`badge ${policy.active_now ? 'online' : 'degraded'}`}>
                                {policy.active_now ? 'window open' : 'deferred'}
                              </span>
                            )}
                            {!policy.eligible && policy.reasons?.length > 0 && (
                              <span style={{ fontSize: 12, color: '#777' }}>{policy.reasons.join('; ')}</span>
                            )}
                          </div>
                        )}
                      </td>
                      <td><span className={`badge ${(pool.health || pool.state || '').toLowerCase()}`}>{pool.health || pool.state || 'unknown'}</span></td>
                      <td>{pool.size_human}</td>
                      <td>{pool.alloc_human} ({pool.used_pct}%)</td>
                      <td>{pool.free_human}</td>
                      <td className="action-cell">
                        <button className="btn secondary" onClick={() => scrub(pool.name)}>Scrub</button>
                        {' '}
                        {policy && (
                          <>
                            <button
                              className="btn secondary"
                              disabled={busy || (!policy.enabled && !policy.eligible)}
                              onClick={() => setRawPoolSpindown(pool.name, !policy.enabled)}
                            >
                              {busy ? 'Working...' : policy.enabled ? 'Disable Spindown' : 'Enable Spindown'}
                            </button>
                            {' '}
                            <button
                              className="btn secondary"
                              disabled={busy}
                              onClick={() => setRawPoolSpindown(pool.name, !!policy.enabled, [{ days: ['daily'], start: '01:00', end: '06:00' }])}
                            >
                              Nightly
                            </button>
                            {' '}
                          </>
                        )}
                        <button className="btn danger" onClick={() => deletePool(pool.name)}>Destroy</button>
                      </td>
                    </tr>
                    );
                  })}
                </tbody>
              </table>
            )
          )}
        </>
      )}

      {activeTab === 'datasets' && <Datasets />}
      {activeTab === 'zvols' && <Zvols />}
      {activeTab === 'snapshots' && <Snapshots />}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText="Destroy"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </>
  );
}
