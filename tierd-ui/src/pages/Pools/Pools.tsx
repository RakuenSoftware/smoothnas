import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
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

export default function Pools() {
  const { t } = useI18n();
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
  const [importablePools, setImportablePools] = useState<any[]>([]);
  const [selectedZfsMembers, setSelectedZfsMembers] = useState<string[]>([]);
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  const unassignedDisks = disks.filter((d: any) => d.assignment === 'unassigned');
  const zfsMemberDisks = importablePools.length > 0 ? disks.filter((d: any) => d.assignment === 'zfs-pool') : [];

  useEffect(() => { if (pools !== undefined) setLoading(false); }, [pools]);
  useEffect(() => {
    loadImportablePools();
    return () => { stopPollRef.current?.(); };
  }, []);

  function refresh() { invalidate('pools'); invalidate('disks'); loadImportablePools(); }

  function loadImportablePools() {
    api.getImportablePools()
      .then((items: any[]) => setImportablePools(items || []))
      .catch(() => setImportablePools([]));
  }

  function toggleDisk(path: string, role: 'data' | 'slog' | 'l2arc') {
    const set = role === 'data' ? setSelectedData : role === 'slog' ? setSelectedSlog : setSelectedL2arc;
    set(prev => prev.includes(path) ? prev.filter(p => p !== path) : [...prev, path]);
  }

  function pollJob(jobId: string, onComplete: () => void, onError: (err: string) => void): () => void {
    const timer = setInterval(() => {
      api.getJobStatus(jobId).then((job: any) => {
        if (job.status === 'completed') { clearInterval(timer); onComplete(); }
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || t('arrays.error.jobFailed')); }
      }).catch(() => { clearInterval(timer); onError(t('arrays.error.lostConnection')); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function createPool() {
    const data = {
      name: newPool.name, vdev_type: newPool.vdev_type,
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
        toast.success(t('pools.toast.created'));
        invalidate('pools'); invalidate('disks');
      }, (err) => {
        setSubmitting(false);
        toast.error(t('arrays.error.createPoolPrefix', { err }));
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, t('arrays.error.createPool')));
    });
  }

  function deletePool(name: string) {
    setConfirmTitle(t('arrays.confirm.destroyPoolTitle'));
    setConfirmMessage(t('arrays.confirm.destroyPoolMessage', { name }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deletePool(name).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success(t('arrays.toast.poolDestroyed'));
          invalidate('pools');
        }, (err) => toast.error(t('arrays.error.destroyArrayPrefix', { err })));
      }).catch(e => toast.error(extractError(e, t('arrays.error.destroyPool'))));
    };
    setConfirmVisible(true);
  }

  function scrub(name: string) {
    api.scrubPool(name).then(() => toast.info(t('arrays.toast.scrubStarted', { name })))
      .catch(e => toast.error(extractError(e, t('arrays.error.scrubStart'))));
  }

  function importPool(name: string) {
    api.importPool(name).then(() => {
      toast.success(t('arrays.toast.poolImported', { name }));
      refresh();
    }).catch(e => toast.error(extractError(e, t('arrays.error.import'))));
  }

  function toggleZfsMember(path: string) {
    setSelectedZfsMembers(prev => prev.includes(path) ? prev.filter(p => p !== path) : [...prev, path]);
  }

  function wipeSelectedZfsMembers() {
    if (selectedZfsMembers.length === 0) return;
    setConfirmTitle(t('arrays.confirm.wipeMembersTitle'));
    setConfirmMessage(t('arrays.confirm.wipeMembersMessage', { disks: selectedZfsMembers.join(', ') }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      setSubmitting(true);
      api.wipeZfsMemberDisks(selectedZfsMembers).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          setSubmitting(false);
          setSelectedZfsMembers([]);
          toast.success(t('arrays.toast.zfsMembersWiped'));
          refresh();
        }, (err) => {
          setSubmitting(false);
          toast.error(t('arrays.error.wipeFailedPrefix', { err }));
        });
      }).catch(e => {
        setSubmitting(false);
        toast.error(extractError(e, t('arrays.error.wipeFailed')));
      });
    };
    setConfirmVisible(true);
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('pools.title')}</h1>
        <p className="subtitle">{t('pools.subtitle')}</p>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      <div className="tabs">
        {(['pools', 'datasets', 'zvols', 'snapshots'] as Tab[]).map(tab => (
          <button key={tab} className={`tab${activeTab === tab ? ' active' : ''}`} onClick={() => setActiveTab(tab)}>
            {t(`pools.tab.${tab}`)}
          </button>
        ))}
      </div>

      {activeTab === 'pools' && (
        <>
          <div style={{ marginBottom: 16 }}>
            <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('pools.button.createPool')}</button>
          </div>
          {(importablePools.length > 0 || zfsMemberDisks.length > 0) && (
            <div className="create-form">
              <h3>{t('arrays.section.existingZfs')}</h3>
              {importablePools.length > 0 && (
                <table className="data-table" style={{ marginBottom: 16 }}>
                  <thead>
                    <tr><th>{t('arrays.col.pool')}</th><th>{t('arrays.col.state')}</th><th>{t('arrays.col.id')}</th><th>{t('arrays.col.status')}</th><th>{t('arrays.col.actions')}</th></tr>
                  </thead>
                  <tbody>
                    {importablePools.map((pool: any) => (
                      <tr key={pool.id || pool.name}>
                        <td><strong>{pool.name}</strong></td>
                        <td><span className={`badge ${pool.state?.toLowerCase()}`}>{pool.state || t('arrays.state.unknown')}</span></td>
                        <td><code>{pool.id || '—'}</code></td>
                        <td>{pool.status || t('arrays.import.readyToImport')}</td>
                        <td className="action-cell">
                          <button className="btn primary" onClick={() => importPool(pool.name)}>{t('arrays.import.button')}</button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {zfsMemberDisks.length > 0 && (
                <>
                  <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>{t('arrays.zfs.importableMembers')}</div>
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
                    {zfsMemberDisks.map((disk: any) => (
                      <label key={disk.path} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px', border: `2px solid ${selectedZfsMembers.includes(disk.path) ? '#ef5350' : '#ddd'}`, borderRadius: 6, cursor: 'pointer', background: selectedZfsMembers.includes(disk.path) ? '#ffebee' : '#fff' }}>
                        <input type="checkbox" checked={selectedZfsMembers.includes(disk.path)} onChange={() => toggleZfsMember(disk.path)} />
                        <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
                      </label>
                    ))}
                  </div>
                  <button className="btn danger" onClick={wipeSelectedZfsMembers} disabled={submitting || selectedZfsMembers.length === 0}>
                    {t('arrays.zfs.wipeMembers')}
                  </button>
                </>
              )}
            </div>
          )}
          {showCreate && (
            <div className="create-form">
              <h3>{t('pools.create.title')}</h3>
              <div className="form-row">
                <label>{t('arrays.field.name')} <input value={newPool.name} onChange={e => setNewPool(p => ({ ...p, name: e.target.value }))} /></label>
                <label>{t('arrays.field.vdevType')}
                  <select value={newPool.vdev_type} onChange={e => setNewPool(p => ({ ...p, vdev_type: e.target.value }))}>
                    <option value="mirror">{t('arrays.vdev.mirror')}</option>
                    <option value="raidz1">{t('arrays.vdev.raidz1')}</option>
                    <option value="raidz2">{t('arrays.vdev.raidz2')}</option>
                    <option value="raidz3">{t('arrays.vdev.raidz3')}</option>
                    <option value="stripe">{t('arrays.vdev.stripe')}</option>
                  </select>
                </label>
              </div>
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>{t('arrays.zfs.dataLabel')}</div>
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
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>{t('arrays.disks.noUnassigned')}</p>}
              </div>
              {selectedData.length > 0 && <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{t('arrays.zfs.dataSelected', { count: selectedData.length })}</p>}
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>{t('arrays.zfs.slogLabel')}</div>
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
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>{t('arrays.disks.noUnassigned')}</p>}
              </div>
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>{t('arrays.zfs.l2arcLabel')}</div>
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
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>{t('arrays.disks.noUnassigned')}</p>}
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button className="btn secondary" onClick={() => { setShowCreate(false); setSelectedData([]); setSelectedSlog([]); setSelectedL2arc([]); }} disabled={submitting}>{t('common.cancel')}</button>
                <button className="btn primary" onClick={createPool} disabled={submitting || selectedData.length === 0}>
                  {submitting ? t('arrays.creating') : t('arrays.button.create')}
                </button>
              </div>
            </div>
          )}
          <Spinner loading={loading} text={t('pools.loading')} />
          {!loading && (
            pools.length === 0 ? (
              <div className="empty-state"><div className="empty-icon">○</div><p>{t('arrays.empty.zfs')}</p></div>
            ) : (
              <table className="data-table">
                <thead>
                  <tr><th>{t('arrays.col.pool')}</th><th>{t('arrays.col.state')}</th><th>{t('pools.col.size')}</th><th>{t('pools.col.used')}</th><th>{t('pools.col.free')}</th><th>{t('arrays.col.actions')}</th></tr>
                </thead>
                <tbody>
                  {pools.map((pool: any) => (
                    <tr key={pool.name}>
                      <td><strong>{pool.name}</strong></td>
                      <td><span className={`badge ${pool.state?.toLowerCase()}`}>{pool.state}</span></td>
                      <td>{pool.size_human}</td>
                      <td>{t('pools.summary.usedWithPct', { used: pool.alloc_human, pct: pool.used_pct })}</td>
                      <td>{pool.free_human}</td>
                      <td className="action-cell">
                        <button className="btn secondary" onClick={() => scrub(pool.name)}>{t('arrays.action.scrub')}</button>
                        {' '}
                        <button className="btn danger" onClick={() => deletePool(pool.name)}>{t('arrays.action.destroy')}</button>
                      </td>
                    </tr>
                  ))}
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
        confirmText={confirmTitle === t('arrays.confirm.wipeMembersTitle') ? t('arrays.confirm.wipe') : t('arrays.action.destroy')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
