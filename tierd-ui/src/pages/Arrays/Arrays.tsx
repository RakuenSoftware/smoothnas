import { useEffect, useRef, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Arrays() {
  const { arrays, disks, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [expandedName, setExpandedName] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [newArray, setNewArray] = useState({ name: 'md0', level: 'raid5' });

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
    setShowCreate(true);
  }
  const [selectedDisks, setSelectedDisks] = useState<string[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [createStatus, setCreateStatus] = useState('');
  const [destroyStatus, setDestroyStatus] = useState('');
  const [destroyingArrays, setDestroyingArrays] = useState<Set<string>>(new Set());
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  const unassignedDisks = disks.filter((d: any) => d.assignment === 'unassigned');

  useEffect(() => {
    if (arrays !== undefined) setLoading(false);
  }, [arrays]);

  useEffect(() => {
    resumeRunningJobs();
    return () => { stopPollRef.current?.(); };
  }, []);

  function refresh() {
    invalidate('arrays');
    invalidate('disks');
  }

  function minDisksForLevel(level: string): number {
    if (level === 'raid6') return 4;
    if (['raid5', 'raid4'].includes(level)) return 3;
    if (['raid1', 'raid10'].includes(level)) return 2;
    return 1;
  }

  function toggleDisk(path: string) {
    setSelectedDisks(prev => {
      const next = prev.includes(path) ? prev.filter(d => d !== path) : [...prev, path];
      return next;
    });
  }

  function submit() {
    if (selectedDisks.length === 0) { toast.warning('Select at least one disk'); return; }
    const min = minDisksForLevel(newArray.level);
    if (selectedDisks.length < min) {
      toast.warning(`${newArray.level.toUpperCase()} requires at least ${min} disks (${selectedDisks.length} selected)`);
      return;
    }
    setSubmitting(true);
    setCreateStatus('Starting...');
    api.createArray({ name: newArray.name, level: newArray.level, disks: selectedDisks }).then((res: any) => {
      stopPollRef.current = pollJobWithStatus(res.job_id, onCreateDone, (err) => {
        setSubmitting(false);
        setCreateStatus('');
        toast.error('Failed to create array: ' + err);
        refresh();
      });
    }).catch(e => {
      setSubmitting(false);
      setCreateStatus('');
      toast.error(extractError(e, 'Failed to create array'));
    });
  }

  function pollJobWithStatus(jobId: string, onComplete: () => void, onError: (err: string) => void): () => void {
    const timer = setInterval(() => {
      api.getJobStatus(jobId).then((job: any) => {
        if (job.progress) { setCreateStatus(job.progress); setDestroyStatus(job.progress); }
        if (job.status === 'completed') { clearInterval(timer); onComplete(); }
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || 'Job failed'); }
      }).catch(() => { clearInterval(timer); onError('Lost connection'); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function onCreateDone() {
    setSubmitting(false);
    setShowCreate(false);
    setCreateStatus('');
    setSelectedDisks([]);
    toast.success('Created successfully');
    refresh();
  }

  function onDestroyDone(name?: string) {
    setDestroyStatus('');
    if (name) {
      setDestroyingArrays(prev => { const s = new Set(prev); s.delete(name); return s; });
    }
    setExpandedName(null);
    toast.success('Array destroyed');
    refresh();
  }

  function resumeRunningJobs() {
    api.listJobsByTag('array-create').then((jobs: any[]) => {
      const running = jobs.find(j => j.status === 'running');
      if (running) {
        setSubmitting(true);
        setCreateStatus(running.progress || 'Creating array...');
        stopPollRef.current = pollJobWithStatus(running.id, onCreateDone, (err) => {
          setSubmitting(false);
          setCreateStatus('');
          toast.error('Array creation failed: ' + err);
          refresh();
        });
      }
    }).catch(() => {});
    api.listJobsByTag('array-destroy').then((jobs: any[]) => {
      const running = jobs.find(j => j.status === 'running');
      if (running) {
        setDestroyStatus(running.progress || 'Destroying array...');
        stopPollRef.current = pollJobWithStatus(running.id, onDestroyDone, (err) => {
          setDestroyStatus('');
          toast.error('Array destruction failed: ' + err);
          refresh();
        });
      }
    }).catch(() => {});
  }

  function deleteArray(name: string) {
    setConfirmTitle('Destroy Array');
    setConfirmMessage(`This will permanently destroy /dev/${name}, remove all data on the array, and zero superblocks on all member disks. This cannot be undone.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      setDestroyingArrays(prev => new Set([...prev, name]));
      setDestroyStatus('Starting destruction...');
      api.deleteArray(name).then((res: any) => {
        stopPollRef.current = pollJobWithStatus(res.job_id, () => onDestroyDone(name), (err) => {
          setDestroyStatus('');
          setDestroyingArrays(prev => { const s = new Set(prev); s.delete(name); return s; });
          toast.error('Destroy failed: ' + err);
          refresh();
        });
      }).catch(e => {
        setDestroyStatus('');
        setDestroyingArrays(prev => { const s = new Set(prev); s.delete(name); return s; });
        toast.error(extractError(e, 'Destroy failed'));
      });
    };
    setConfirmVisible(true);
  }

  function scrub(name: string) {
    api.scrubArray(name).then(() => toast.info('Scrub started on ' + name))
      .catch(e => toast.error(extractError(e, 'Scrub failed')));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Arrays</h1>
        <p className="subtitle">mdadm RAID array management for tier assignments</p>
        <div className="header-actions">
          <button className="btn primary" onClick={openCreate} disabled={showCreate}>Create</button>
          <button className="refresh-btn" onClick={refresh}>Refresh</button>
        </div>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Create Array</h3>
          <div className="form-row">
            <label>Name
              <input value={newArray.name} onChange={e => setNewArray(p => ({ ...p, name: e.target.value }))} placeholder="md0" />
            </label>
            <label>RAID Level
              <select value={newArray.level} onChange={e => setNewArray(p => ({ ...p, level: e.target.value }))}>
                <option value="raid0">RAID-0 / Single (1+ disks, stripe)</option>
                <option value="raid1">RAID-1 (2+ disks, mirror)</option>
                <option value="raid5">RAID-5 (3+ disks, parity)</option>
                <option value="raid6">RAID-6 (4+ disks, dual parity)</option>
                <option value="raid10">RAID-10 (2+ disks, stripe+mirror)</option>
                <option value="linear">Linear (1+ disks, JBOD)</option>
              </select>
            </label>
          </div>
          <div style={{ marginBottom: 8, fontSize: 13, color: '#666' }}>Select drives for RAID array</div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 12 }}>
            {unassignedDisks.map((disk: any) => (
              <label key={disk.path} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '8px 12px', border: `2px solid ${selectedDisks.includes(disk.path) ? '#4fc3f7' : '#ddd'}`, borderRadius: 6, cursor: 'pointer', background: selectedDisks.includes(disk.path) ? '#e3f2fd' : '#fff' }}>
                <input type="checkbox" checked={selectedDisks.includes(disk.path)} onChange={() => toggleDisk(disk.path)} />
                <span><strong>/dev/{disk.name}</strong> {disk.size_human}{disk.model ? ` — ${disk.model}` : ''}</span>
              </label>
            ))}
            {unassignedDisks.length === 0 && <p style={{ color: '#999' }}>No unassigned disks available.</p>}
          </div>
          {selectedDisks.length > 0 && <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{selectedDisks.length} disk(s) selected</p>}
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            {createStatus && <span style={{ fontSize: 13, color: '#666' }}>{createStatus}</span>}
            <div style={{ flex: 1 }} />
            <button className="btn secondary" onClick={() => { setShowCreate(false); setCreateStatus(''); setSubmitting(false); }} disabled={submitting}>Cancel</button>
            <button className="btn primary" onClick={submit} disabled={submitting || selectedDisks.length === 0}>
              {submitting ? 'Creating...' : 'Create'}
            </button>
          </div>
        </div>
      )}

      {destroyStatus && <div className="status-banner warning">{destroyStatus}</div>}

      <Spinner loading={loading} text="Loading arrays..." />

      {!loading && (
        arrays.length === 0 && !showCreate ? (
          <div className="empty-state">
            <div className="empty-icon">⊞</div>
            <p>No mdadm arrays configured.</p>
            <p className="empty-hint">Create an array, then assign it to a tier.</p>
          </div>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 0 }}>
            {arrays.map((a: any) => (
              <div key={a.name} style={{ background: '#fff', borderRadius: 8, marginBottom: 8, overflow: 'hidden', boxShadow: '0 1px 3px rgba(0,0,0,0.08)', border: a.state === 'degraded' ? '1px solid #ff9800' : a.state === 'inactive' ? '1px solid #f44336' : '1px solid transparent', opacity: destroyingArrays.has(a.name) ? 0.5 : 1, pointerEvents: destroyingArrays.has(a.name) ? 'none' : 'auto', transition: 'opacity 0.2s' }}>
                <div onClick={() => setExpandedName(n => n === a.name ? null : a.name)} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '12px 16px', cursor: 'pointer' }}>
                  <span><code>{a.path}</code></span>
                  <span style={{ fontSize: 12, color: '#888' }}>{a.raid_level}</span>
                  <span className={`badge ${a.state}`}>{a.state}</span>
                  {a.tier && <span style={{ fontSize: 12, color: '#666' }}><code>{a.tier}</code></span>}
                  <span style={{ marginLeft: 'auto', fontSize: 13, color: '#666' }}>{a.lv_size || a.size_human}</span>
                  <span style={{ fontSize: 13, color: '#666' }}>{a.active_disks}/{a.total_disks} disks</span>
                  {a.rebuild_pct >= 0 && (
                    <span style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                      <div className="progress-bar"><div className="progress-fill" style={{ width: `${a.rebuild_pct}%` }} /></div>
                      {a.rebuild_pct}%
                    </span>
                  )}
                  <span>{expandedName === a.name ? '▲' : '▼'}</span>
                </div>
                {expandedName === a.name && (
                  <div style={{ padding: '12px 16px', borderTop: '1px solid #eee' }}>
                    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(160px, 1fr))', gap: 12, marginBottom: 16 }}>
                      {[
                        ['Path', <code>{a.path}</code>],
                        ['RAID Level', a.raid_level],
                        ['State', <span className={`badge ${a.state}`}>{a.state}</span>],
                        ['Size', a.lv_size || a.size_human],
                        ['Disks', `${a.active_disks} active / ${a.total_disks} total`],
                        a.mount_point && ['Mount', <code>{a.mount_point}</code>],
                        a.filesystem && ['Filesystem', a.filesystem],
                        ['Tier Slot', a.tier ? <code>{a.tier}</code> : 'Unassigned'],
                      ].filter(Boolean).map(([label, val]: any) => (
                        <div key={label}>
                          <div style={{ fontSize: 11, color: '#999', marginBottom: 4 }}>{label}</div>
                          <div style={{ fontSize: 13 }}>{val}</div>
                        </div>
                      ))}
                    </div>
                    {a.member_disks?.length > 0 && (
                      <div style={{ marginBottom: 12 }}>
                        <div style={{ fontSize: 11, color: '#999', marginBottom: 4 }}>Member Disks</div>
                        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                          {a.member_disks.map((d: string) => <code key={d} style={{ background: '#f5f5f5', padding: '2px 8px', borderRadius: 4 }}>{d}</code>)}
                        </div>
                      </div>
                    )}
                    <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                      <button className="btn secondary" onClick={() => scrub(a.name)}>Scrub</button>
                      {destroyingArrays.has(a.name) ? (
                        <span className="slot-assigning">Destroying…</span>
                      ) : (
                        <button className="btn danger" onClick={() => deleteArray(a.name)}>Destroy</button>
                      )}
                    </div>
                  </div>
                )}
              </div>
            ))}
          </div>
        )
      )}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText="Confirm"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
