import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

type CreateTab = 'mdadm' | 'zfs';

export default function Arrays() {
  const { t } = useI18n();
  const { arrays, pools, disks, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [expandedName, setExpandedName] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [createTab, setCreateTab] = useState<CreateTab>('mdadm');

  // mdadm create state
  const [newArray, setNewArray] = useState({ name: 'md0', level: 'raid5' });
  const [selectedDisks, setSelectedDisks] = useState<string[]>([]);

  // ZFS create state
  const [newPool, setNewPool] = useState({ name: 'tank', vdev_type: 'raidz1' });
  const [selectedData, setSelectedData] = useState<string[]>([]);
  const [selectedSlog, setSelectedSlog] = useState<string[]>([]);
  const [selectedL2arc, setSelectedL2arc] = useState<string[]>([]);
  const [importablePools, setImportablePools] = useState<any[]>([]);
  const [selectedZfsMembers, setSelectedZfsMembers] = useState<string[]>([]);

  const [submitting, setSubmitting] = useState(false);
  const [createStatus, setCreateStatus] = useState('');
  const [destroyStatus, setDestroyStatus] = useState('');
  const [destroyingArrays, setDestroyingArrays] = useState<Set<string>>(new Set());
  const [destroyingPools, setDestroyingPools] = useState<Set<string>>(new Set());
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  const unassignedDisks = disks.filter((d: any) => d.assignment === 'unassigned');
  const zfsMemberDisks = disks.filter((d: any) => d.assignment === 'zfs-pool');

  function nextArrayName(): string {
    const used = new Set(
      (arrays || [])
        .map((a: any) => { const m = (a.name || '').match(/^md(\d+)$/); return m ? parseInt(m[1], 10) : -1; })
        .filter((n: number) => n >= 0)
    );
    let i = 0;
    while (used.has(i)) i++;
    return `md${i}`;
  }

  function openCreate() {
    setNewArray({ name: nextArrayName(), level: 'raid5' });
    setNewPool({ name: 'tank', vdev_type: 'raidz1' });
    setSelectedDisks([]);
    setSelectedData([]);
    setSelectedSlog([]);
    setSelectedL2arc([]);
    setShowCreate(true);
  }

  function cancelCreate() {
    setShowCreate(false);
    setCreateStatus('');
    setSubmitting(false);
  }

  useEffect(() => {
    if (arrays !== undefined && pools !== undefined) setLoading(false);
  }, [arrays, pools]);

  useEffect(() => {
    loadImportablePools();
    resumeRunningJobs();
    return () => { stopPollRef.current?.(); };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function refresh() {
    invalidate('arrays');
    invalidate('pools');
    invalidate('disks');
    loadImportablePools();
  }

  // --- mdadm helpers -------------------------------------------------------

  function minDisksForLevel(level: string): number {
    if (level === 'raid6') return 4;
    if (['raid5', 'raid4'].includes(level)) return 3;
    if (['raid1', 'raid10'].includes(level)) return 2;
    return 1;
  }

  function toggleDisk(path: string) {
    setSelectedDisks(prev => prev.includes(path) ? prev.filter(d => d !== path) : [...prev, path]);
  }

  function submitMdadm() {
    if (selectedDisks.length === 0) { toast.warning(t('arrays.warn.selectAtLeastOneDisk')); return; }
    const min = minDisksForLevel(newArray.level);
    if (selectedDisks.length < min) {
      toast.warning(t('arrays.warn.minDisks', { level: newArray.level.toUpperCase(), min, selected: selectedDisks.length }));
      return;
    }
    setSubmitting(true);
    setCreateStatus(t('arrays.status.starting'));
    api.createArray({ name: newArray.name, level: newArray.level, disks: selectedDisks }).then((res: any) => {
      stopPollRef.current = pollJobWithStatus(res.job_id, onCreateDone, (err) => {
        setSubmitting(false);
        setCreateStatus('');
        toast.error(t('arrays.error.createArrayPrefix', { err }));
        refresh();
      });
    }).catch(e => {
      setSubmitting(false);
      setCreateStatus('');
      toast.error(extractError(e, t('arrays.error.createArray')));
    });
  }

  // --- ZFS helpers ---------------------------------------------------------

  function toggleZfsDisk(path: string, role: 'data' | 'slog' | 'l2arc') {
    const set = role === 'data' ? setSelectedData : role === 'slog' ? setSelectedSlog : setSelectedL2arc;
    set(prev => prev.includes(path) ? prev.filter(p => p !== path) : [...prev, path]);
  }

  function submitZfs() {
    if (selectedData.length === 0) { toast.warning(t('arrays.warn.selectAtLeastOneData')); return; }
    setSubmitting(true);
    setCreateStatus(t('arrays.status.starting'));
    api.createPool({
      name: newPool.name,
      vdev_type: newPool.vdev_type,
      data_disks: selectedData,
      slog_disks: selectedSlog,
      l2arc_disks: selectedL2arc,
    }).then((res: any) => {
      stopPollRef.current = pollJobWithStatus(res.job_id, onCreateDone, (err) => {
        setSubmitting(false);
        setCreateStatus('');
        toast.error(t('arrays.error.createPoolPrefix', { err }));
        refresh();
      });
    }).catch(e => {
      setSubmitting(false);
      setCreateStatus('');
      toast.error(extractError(e, t('arrays.error.createPool')));
    });
  }

  function destroyPool(name: string) {
    setConfirmTitle(t('arrays.confirm.destroyPoolTitle'));
    setConfirmMessage(t('arrays.confirm.destroyPoolMessage', { name }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      setDestroyingPools(prev => new Set([...prev, name]));
      api.deletePool(name).then((res: any) => {
        stopPollRef.current = pollJobWithStatus(res.job_id,
          () => {
            setDestroyingPools(prev => { const s = new Set(prev); s.delete(name); return s; });
            toast.success(t('arrays.toast.poolDestroyed'));
            refresh();
          },
          (err) => {
            setDestroyingPools(prev => { const s = new Set(prev); s.delete(name); return s; });
            toast.error(t('arrays.error.destroyArrayPrefix', { err }));
            refresh();
          }
        );
      }).catch(e => {
        setDestroyingPools(prev => { const s = new Set(prev); s.delete(name); return s; });
        toast.error(extractError(e, t('arrays.error.destroyPool')));
      });
    };
    setConfirmVisible(true);
  }

  function scrubPool(name: string) {
    api.scrubPool(name).then(() => toast.info(t('arrays.toast.scrubStarted', { name })))
      .catch(e => toast.error(extractError(e, t('arrays.error.scrubStart'))));
  }

  function loadImportablePools() {
    api.getImportablePools()
      .then((items: any[]) => setImportablePools(items || []))
      .catch(() => setImportablePools([]));
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
        stopPollRef.current = pollJobWithStatus(res.job_id, () => {
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

  // --- shared --------------------------------------------------------------

  function pollJobWithStatus(jobId: string, onComplete: () => void, onError: (err: string) => void): () => void {
    const timer = setInterval(() => {
      api.getJobStatus(jobId).then((job: any) => {
        if (job.progress) { setCreateStatus(job.progress); setDestroyStatus(job.progress); }
        if (job.status === 'completed') { clearInterval(timer); onComplete(); }
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || t('arrays.error.jobFailed')); }
      }).catch(() => { clearInterval(timer); onError(t('arrays.error.lostConnection')); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function onCreateDone() {
    setSubmitting(false);
    setShowCreate(false);
    setCreateStatus('');
    setSelectedDisks([]);
    setSelectedData([]);
    setSelectedSlog([]);
    setSelectedL2arc([]);
    toast.success(t('arrays.toast.created'));
    refresh();
  }

  function onDestroyDone(name?: string) {
    setDestroyStatus('');
    if (name) {
      setDestroyingArrays(prev => { const s = new Set(prev); s.delete(name); return s; });
    }
    setExpandedName(null);
    toast.success(t('arrays.toast.arrayDestroyed'));
    refresh();
  }

  function resumeRunningJobs() {
    api.listJobsByTag('array-create').then((jobs: any[]) => {
      const running = jobs.find(j => j.status === 'running');
      if (running) {
        setSubmitting(true);
        setCreateStatus(running.progress || t('arrays.status.creatingArray'));
        stopPollRef.current = pollJobWithStatus(running.id, onCreateDone, (err) => {
          setSubmitting(false);
          setCreateStatus('');
          toast.error(t('arrays.error.arrayCreationFailed', { err }));
          refresh();
        });
      }
    }).catch(() => {});
    api.listJobsByTag('array-destroy').then((jobs: any[]) => {
      const running = jobs.find(j => j.status === 'running');
      if (running) {
        setDestroyStatus(running.progress || t('arrays.status.destroyingArray'));
        stopPollRef.current = pollJobWithStatus(running.id, onDestroyDone, (err) => {
          setDestroyStatus('');
          toast.error(t('arrays.error.arrayDestructionFailed', { err }));
          refresh();
        });
      }
    }).catch(() => {});
  }

  function deleteArray(name: string) {
    setConfirmTitle(t('arrays.confirm.destroyArrayTitle'));
    setConfirmMessage(t('arrays.confirm.destroyArrayMessage', { name }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      setDestroyingArrays(prev => new Set([...prev, name]));
      setDestroyStatus(t('arrays.status.startingDestruction'));
      api.deleteArray(name).then((res: any) => {
        stopPollRef.current = pollJobWithStatus(res.job_id, () => onDestroyDone(name), (err) => {
          setDestroyStatus('');
          setDestroyingArrays(prev => { const s = new Set(prev); s.delete(name); return s; });
          toast.error(t('arrays.error.destroyArrayPrefix', { err }));
          refresh();
        });
      }).catch(e => {
        setDestroyStatus('');
        setDestroyingArrays(prev => { const s = new Set(prev); s.delete(name); return s; });
        toast.error(extractError(e, t('arrays.error.destroyArray')));
      });
    };
    setConfirmVisible(true);
  }

  function scrubArray(name: string) {
    api.scrubArray(name).then(() => toast.info(t('arrays.toast.scrubStarted', { name })))
      .catch(e => toast.error(extractError(e, t('arrays.error.scrubArray'))));
  }

  // --- render --------------------------------------------------------------

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('arrays.title')}</h1>
        <p className="subtitle">{t('arrays.subtitle')}</p>
        <div className="header-actions">
          <button className="btn primary" onClick={openCreate} disabled={showCreate}>{t('arrays.button.create')}</button>
          <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
        </div>
      </div>

      {showCreate && (
        <div className="create-form">
          <div className="tabs" style={{ marginBottom: 12 }}>
            {(['mdadm', 'zfs'] as CreateTab[]).map(tab => (
              <button
                key={tab}
                className={`tab${createTab === tab ? ' active' : ''}`}
                onClick={() => setCreateTab(tab)}
                disabled={submitting}
              >
                {tab === 'mdadm' ? t('arrays.tab.mdadm') : t('arrays.tab.zfs')}
              </button>
            ))}
          </div>

          {createTab === 'mdadm' && (
            <>
              <h3>{t('arrays.create.mdadmTitle')}</h3>
              <div className="form-row">
                <label>{t('arrays.field.name')}
                  <input value={newArray.name} onChange={e => setNewArray(p => ({ ...p, name: e.target.value }))} placeholder="md0" />
                </label>
                <label>{t('arrays.field.raidLevel')}
                  <select value={newArray.level} onChange={e => setNewArray(p => ({ ...p, level: e.target.value }))}>
                    <option value="raid0">{t('arrays.level.raid0')}</option>
                    <option value="raid1">{t('arrays.level.raid1')}</option>
                    <option value="raid5">{t('arrays.level.raid5')}</option>
                    <option value="raid6">{t('arrays.level.raid6')}</option>
                    <option value="raid10">{t('arrays.level.raid10')}</option>
                    <option value="linear">{t('arrays.level.linear')}</option>
                  </select>
                </label>
              </div>
              <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>{t('arrays.disks.selectForRaid')}</div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
                {unassignedDisks.map((disk: any) => (
                  <label key={disk.path} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px', border: `2px solid ${selectedDisks.includes(disk.path) ? '#4fc3f7' : '#ddd'}`, borderRadius: 6, cursor: 'pointer', background: selectedDisks.includes(disk.path) ? '#e3f2fd' : '#fff' }}>
                    <input type="checkbox" checked={selectedDisks.includes(disk.path)} onChange={() => toggleDisk(disk.path)} />
                    <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
                  </label>
                ))}
                {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>{t('arrays.disks.noUnassigned')}</p>}
              </div>
              {selectedDisks.length > 0 && <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{t('arrays.disks.selected', { count: selectedDisks.length })}</p>}
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                {createStatus && <span style={{ fontSize: 13, color: '#666' }}>{createStatus}</span>}
                <div style={{ flex: 1 }} />
                <button className="btn secondary" onClick={cancelCreate} disabled={submitting}>{t('common.cancel')}</button>
                <button className="btn primary" onClick={submitMdadm} disabled={submitting || selectedDisks.length === 0}>
                  {submitting ? t('arrays.creating') : t('arrays.button.create')}
                </button>
              </div>
            </>
          )}

          {createTab === 'zfs' && (
            <>
              <h3>{t('arrays.create.zfsTitle')}</h3>
              <div className="form-row">
                <label>{t('arrays.field.name')}
                  <input value={newPool.name} onChange={e => setNewPool(p => ({ ...p, name: e.target.value }))} placeholder="tank" />
                </label>
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
              <ZfsDiskPicker
                label={t('arrays.zfs.dataLabel')}
                disks={unassignedDisks}
                selected={selectedData}
                otherSelections={[selectedSlog, selectedL2arc]}
                onToggle={(p) => toggleZfsDisk(p, 'data')}
                accent="#4fc3f7"
                tint="#e3f2fd"
                emptyMessage={t('arrays.disks.noUnassigned')}
              />
              {selectedData.length > 0 && <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{t('arrays.zfs.dataSelected', { count: selectedData.length })}</p>}
              <ZfsDiskPicker
                label={t('arrays.zfs.slogLabel')}
                disks={unassignedDisks}
                selected={selectedSlog}
                otherSelections={[selectedData, selectedL2arc]}
                onToggle={(p) => toggleZfsDisk(p, 'slog')}
                accent="#81c784"
                tint="#e8f5e9"
                emptyMessage={t('arrays.disks.noUnassigned')}
              />
              <ZfsDiskPicker
                label={t('arrays.zfs.l2arcLabel')}
                disks={unassignedDisks}
                selected={selectedL2arc}
                otherSelections={[selectedData, selectedSlog]}
                onToggle={(p) => toggleZfsDisk(p, 'l2arc')}
                accent="#ffb74d"
                tint="#fff3e0"
                emptyMessage={t('arrays.disks.noUnassigned')}
              />
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                {createStatus && <span style={{ fontSize: 13, color: '#666' }}>{createStatus}</span>}
                <div style={{ flex: 1 }} />
                <button className="btn secondary" onClick={cancelCreate} disabled={submitting}>{t('common.cancel')}</button>
                <button className="btn primary" onClick={submitZfs} disabled={submitting || selectedData.length === 0}>
                  {submitting ? t('arrays.creating') : t('arrays.button.create')}
                </button>
              </div>
            </>
          )}
        </div>
      )}

      {destroyStatus && <div className="status-banner warning">{destroyStatus}</div>}

      <Spinner loading={loading} text={t('arrays.loading')} />

      {!loading && (
        <>
          {/* --- mdadm section --- */}
          <h3 style={{ marginTop: 24, marginBottom: 8 }}>{t('arrays.section.mdadm')}</h3>
          {arrays.length === 0 ? (
            <div className="empty-state" style={{ marginBottom: 24 }}>
              <p>{t('arrays.empty.mdadm')}</p>
              <p className="empty-hint">{t('arrays.empty.mdadmHint')}</p>
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 0, marginBottom: 24 }}>
              {arrays.map((a: any) => (
                <div key={a.name} style={{ background: '#fff', borderRadius: 8, marginBottom: 8, overflow: 'hidden', boxShadow: '0 1px 3px rgba(0,0,0,0.08)', border: a.state === 'degraded' ? '1px solid #ff9800' : a.state === 'inactive' ? '1px solid #f44336' : '1px solid transparent', opacity: destroyingArrays.has(a.name) ? 0.5 : 1, pointerEvents: destroyingArrays.has(a.name) ? 'none' : 'auto', transition: 'opacity 0.2s' }}>
                  <div onClick={() => setExpandedName(n => n === `md:${a.name}` ? null : `md:${a.name}`)} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '12px 16px', cursor: 'pointer' }}>
                    <span><code>{a.path}</code></span>
                    <span style={{ fontSize: 12, color: '#888' }}>{a.raid_level}</span>
                    <span className={`badge ${a.state}`}>{a.state}</span>
                    {a.tier && <span style={{ fontSize: 12, color: '#666' }}><code>{a.tier}</code></span>}
                    <span style={{ marginLeft: 'auto', fontSize: 13, color: '#666' }}>{a.lv_size || a.size_human}</span>
                    <span style={{ fontSize: 13, color: '#666' }}>{t('arrays.summary.disks', { active: a.active_disks, total: a.total_disks })}</span>
                    {a.rebuild_pct >= 0 && (
                      <span style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                        <div className="progress-bar"><div className="progress-fill" style={{ width: `${a.rebuild_pct}%` }} /></div>
                        {a.rebuild_pct}%
                      </span>
                    )}
                    <span>{expandedName === `md:${a.name}` ? '▲' : '▼'}</span>
                  </div>
                  {expandedName === `md:${a.name}` && (
                    <div style={{ padding: '12px 16px', borderTop: '1px solid #eee' }}>
                      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(160px, 1fr))', gap: 12, marginBottom: 16 }}>
                        {[
                          [t('arrays.detail.path'), <code>{a.path}</code>],
                          [t('arrays.detail.raidLevel'), a.raid_level],
                          [t('arrays.detail.state'), <span className={`badge ${a.state}`}>{a.state}</span>],
                          [t('arrays.detail.size'), a.lv_size || a.size_human],
                          [t('arrays.detail.disks'), t('arrays.detail.disksValue', { active: a.active_disks, total: a.total_disks })],
                          a.mount_point && [t('arrays.detail.mount'), <code>{a.mount_point}</code>],
                          a.filesystem && [t('arrays.detail.filesystem'), a.filesystem],
                          [t('arrays.detail.tierSlot'), a.tier ? <code>{a.tier}</code> : t('arrays.detail.unassigned')],
                        ].filter(Boolean).map(([label, val]: any) => (
                          <div key={label}>
                            <div style={{ fontSize: 11, color: '#999', marginBottom: 4 }}>{label}</div>
                            <div style={{ fontSize: 13 }}>{val}</div>
                          </div>
                        ))}
                      </div>
                      {a.member_disks?.length > 0 && (
                        <div style={{ marginBottom: 12 }}>
                          <div style={{ fontSize: 11, color: '#999', marginBottom: 4 }}>{t('arrays.detail.memberDisks')}</div>
                          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                            {a.member_disks.map((d: string) => <code key={d} style={{ background: '#f5f5f5', padding: '2px 8px', borderRadius: 4 }}>{d}</code>)}
                          </div>
                        </div>
                      )}
                      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                        <button className="btn secondary" onClick={() => scrubArray(a.name)}>{t('arrays.action.scrub')}</button>
                        {destroyingArrays.has(a.name) ? (
                          <span className="slot-assigning">{t('arrays.action.destroying')}</span>
                        ) : (
                          <button className="btn danger" onClick={() => deleteArray(a.name)}>{t('arrays.action.destroy')}</button>
                        )}
                      </div>
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}

          {/* --- ZFS section --- */}
          <h3 style={{ marginTop: 16, marginBottom: 8 }}>{t('arrays.section.zfs')}</h3>
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
                  <ZfsMemberDiskPicker
                    disks={zfsMemberDisks}
                    selected={selectedZfsMembers}
                    onToggle={toggleZfsMember}
                  />
                  <button className="btn danger" onClick={wipeSelectedZfsMembers} disabled={submitting || selectedZfsMembers.length === 0}>
                    {t('arrays.zfs.wipeMembers')}
                  </button>
                </>
              )}
            </div>
          )}
          {pools.length === 0 ? (
            <div className="empty-state">
              <p>{t('arrays.empty.zfs')}</p>
              <p className="empty-hint">{t('arrays.empty.zfsHint')}</p>
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 0 }}>
              {pools.map((p: any) => (
                <div key={p.name} style={{ background: '#fff', borderRadius: 8, marginBottom: 8, overflow: 'hidden', boxShadow: '0 1px 3px rgba(0,0,0,0.08)', opacity: destroyingPools.has(p.name) ? 0.5 : 1, pointerEvents: destroyingPools.has(p.name) ? 'none' : 'auto', transition: 'opacity 0.2s' }}>
                  <div onClick={() => setExpandedName(n => n === `zfs:${p.name}` ? null : `zfs:${p.name}`)} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '12px 16px', cursor: 'pointer' }}>
                    <span><strong>{p.name}</strong></span>
                    <span className={`badge ${(p.state || '').toLowerCase()}`}>{p.state}</span>
                    <span style={{ marginLeft: 'auto', fontSize: 13, color: '#666' }}>{p.size_human}</span>
                    <span style={{ fontSize: 13, color: '#666' }}>{t('arrays.summary.usedWithPct', { used: p.alloc_human, pct: p.used_pct })}</span>
                    <span style={{ fontSize: 13, color: '#666' }}>{t('arrays.summary.free', { free: p.free_human })}</span>
                    <span>{expandedName === `zfs:${p.name}` ? '▲' : '▼'}</span>
                  </div>
                  {expandedName === `zfs:${p.name}` && (
                    <div style={{ padding: '12px 16px', borderTop: '1px solid #eee' }}>
                      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                        <button className="btn secondary" onClick={() => scrubPool(p.name)}>{t('arrays.action.scrub')}</button>
                        {destroyingPools.has(p.name) ? (
                          <span className="slot-assigning">{t('arrays.action.destroying')}</span>
                        ) : (
                          <button className="btn danger" onClick={() => destroyPool(p.name)}>{t('arrays.action.destroy')}</button>
                        )}
                      </div>
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText={confirmTitle === t('arrays.confirm.wipeMembersTitle') ? t('arrays.confirm.wipe') : t('arrays.confirm.confirm')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}

function ZfsMemberDiskPicker({
  disks, selected, onToggle,
}: {
  disks: any[];
  selected: string[];
  onToggle: (path: string) => void;
}) {
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
      {disks.map((disk: any) => (
        <label key={disk.path} style={{
          display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px',
          border: `2px solid ${selected.includes(disk.path) ? '#ef5350' : '#ddd'}`,
          borderRadius: 6, cursor: 'pointer',
          background: selected.includes(disk.path) ? '#ffebee' : '#fff',
        }}>
          <input type="checkbox" checked={selected.includes(disk.path)} onChange={() => onToggle(disk.path)} />
          <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
        </label>
      ))}
    </div>
  );
}

// ZfsDiskPicker renders one of the three disk-selection groups (data / slog
// / l2arc). A disk picked for another role shows disabled here so the same
// disk can't be assigned to two roles at once.
function ZfsDiskPicker({
  label, disks, selected, otherSelections, onToggle, accent, tint, emptyMessage,
}: {
  label: string;
  disks: any[];
  selected: string[];
  otherSelections: string[][];
  onToggle: (path: string) => void;
  accent: string;
  tint: string;
  emptyMessage: string;
}) {
  const takenElsewhere = (path: string) => otherSelections.some(sel => sel.includes(path));
  return (
    <>
      <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>{label}</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
        {disks.map((disk: any) => {
          const used = takenElsewhere(disk.path);
          return (
            <label key={disk.path} style={{
              display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px',
              border: `2px solid ${selected.includes(disk.path) ? accent : '#ddd'}`,
              borderRadius: 6, cursor: used ? 'not-allowed' : 'pointer',
              background: selected.includes(disk.path) ? tint : '#fff',
              opacity: used ? 0.4 : 1,
            }}>
              <input type="checkbox" checked={selected.includes(disk.path)} disabled={used} onChange={() => onToggle(disk.path)} />
              <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
            </label>
          );
        })}
        {disks.length === 0 && <p style={{ color: '#999' }}>{emptyMessage}</p>}
      </div>
    </>
  );
}
