import React, { useEffect, useRef, useState } from 'react';
import { api } from '../../api/api';
import { useToast } from '../../contexts/ToastContext';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

interface BackupConfig {
  id: number;
  name: string;
  target_type: 'nfs' | 'smb';
  host: string;
  share: string;
  smb_user: string;
  has_creds: boolean;
  local_path: string;
  remote_path: string;
  direction: 'push' | 'pull';
  method: 'cp' | 'rsync';
  parallelism: number;
  created_at: string;
}

interface BackupRun {
  id: number;
  config_id: number;
  status: 'running' | 'completed' | 'failed';
  progress: string;
  files_done: number;
  files_total: number;
  progress_pct: number; // 0-100, or -1 for indeterminate
  error: string;
  summary: string;
  started_at: string;
  completed_at: string;
}

const EMPTY_FORM = {
  name: '',
  target_type: 'nfs' as 'nfs' | 'smb',
  host: '',
  share: '',
  smb_user: '',
  smb_pass: '',
  local_path: '',
  remote_path: '',
  direction: 'push' as 'push' | 'pull',
  method: 'rsync' as 'cp' | 'rsync',
  parallelism: 1,
};

export default function Backups() {
  const { success: toastSuccess, error: toastError } = useToast();
  const [configs, setConfigs] = useState<BackupConfig[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({ ...EMPTY_FORM });
  const [saving, setSaving] = useState(false);
  const [formError, setFormError] = useState('');
  const [localPaths, setLocalPaths] = useState<any[]>([]);
  const [runs, setRuns] = useState<Record<number, BackupRun>>({});
  const [deleteTarget, setDeleteTarget] = useState<BackupConfig | null>(null);
  const pollRefs = useRef<Record<number, ReturnType<typeof setInterval>>>({});

  useEffect(() => {
    api.getFilesystemPaths().then(setLocalPaths).catch(() => {});
    load();
    return () => {
      Object.values(pollRefs.current).forEach(clearInterval);
    };
  }, []);

  function load() {
    setLoading(true);
    Promise.all([
      api.getBackupConfigs(),
      api.listBackupRuns(), // active runs across all configs
    ]).then(([cfgs, activeRuns]) => {
      setConfigs(cfgs);
      // Restore polling for any runs that are still in progress.
      const restored: Record<number, BackupRun> = {};
      for (const run of activeRuns as BackupRun[]) {
        restored[run.config_id] = run;
        pollRun(run.config_id, run.id);
      }
      setRuns(restored);
      setLoading(false);
    }).catch(() => {
      setLoading(false);
    });
  }

  function setField(key: keyof typeof EMPTY_FORM, value: string | number) {
    setForm(f => ({ ...f, [key]: value }));
  }

  async function saveConfig() {
    setFormError('');
    setSaving(true);
    try {
      await api.createBackupConfig(form);
      setForm({ ...EMPTY_FORM });
      setShowForm(false);
      load();
      toastSuccess('Backup config created');
    } catch (e: any) {
      setFormError(e.message || 'Failed to create backup config');
    } finally {
      setSaving(false);
    }
  }

  function confirmDelete(cfg: BackupConfig) {
    setDeleteTarget(cfg);
  }

  async function doDelete() {
    if (!deleteTarget) return;
    try {
      await api.deleteBackupConfig(deleteTarget.id);
      setDeleteTarget(null);
      load();
      toastSuccess('Backup config deleted');
    } catch (e: any) {
      toastError(e.message || 'Delete failed');
    }
  }

  function runBackup(cfg: BackupConfig) {
    api.runBackup(cfg.id).then((res: any) => {
      const runId: number = res.run_id;
      // Seed the run state optimistically so the UI shows immediately.
      setRuns(prev => ({
        ...prev,
        [cfg.id]: {
          id: runId, config_id: cfg.id, status: 'running',
          progress: 'Starting...', files_done: 0, files_total: -1,
          progress_pct: -1, error: '', summary: '',
          started_at: new Date().toISOString(), completed_at: '',
        },
      }));
      pollRun(cfg.id, runId);
    }).catch((e: any) => {
      toastError(e.message || 'Failed to start backup');
    });
  }

  function pollRun(cfgId: number, runId: number) {
    if (pollRefs.current[cfgId]) clearInterval(pollRefs.current[cfgId]);
    pollRefs.current[cfgId] = setInterval(() => {
      api.getBackupRun(runId).then((run: BackupRun) => {
        setRuns(prev => ({ ...prev, [cfgId]: run }));
        if (run.status === 'completed') {
          clearInterval(pollRefs.current[cfgId]);
          toastSuccess('Backup complete');
        } else if (run.status === 'failed') {
          clearInterval(pollRefs.current[cfgId]);
        }
      }).catch(() => clearInterval(pollRefs.current[cfgId]));
    }, 500);
  }

  function dismissRun(cfgId: number) {
    setRuns(prev => {
      const next = { ...prev };
      delete next[cfgId];
      return next;
    });
  }

  const isRunning = (cfgId: number) => runs[cfgId]?.status === 'running';

  return (
    <div className="page">
      <div className="page-header">
        <h1>Backups</h1>
        <p className="subtitle">Push or pull backups to NFS or SMB targets using cp+hash or rsync</p>
        <button className="btn primary" onClick={() => { setShowForm(v => !v); setFormError(''); }}>
          {showForm ? 'Cancel' : '+ Add Backup Config'}
        </button>
      </div>

      {showForm && (
        <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
          <h3 style={{ margin: '0 0 16px' }}>New Backup Config</h3>

          <div className="form-row">
            <label>Name
              <input value={form.name} onChange={e => setField('name', e.target.value)} placeholder="e.g. nightly-push" />
            </label>
            <label>Direction
              <select value={form.direction} onChange={e => setField('direction', e.target.value as any)}>
                <option value="push">Push (local → remote)</option>
                <option value="pull">Pull (remote → local)</option>
              </select>
            </label>
            <label>Method
              <select value={form.method} onChange={e => setField('method', e.target.value as any)}>
                <option value="rsync">rsync</option>
                <option value="cp">cp + sha256 verify</option>
              </select>
            </label>
            {form.method === 'rsync' && (
              <label title="Number of concurrent rsync streams. Splits the source's top-level entries round-robin. >1 keeps throughput steady when a single stream stalls on slow source files.">
                Parallelism
                <input
                  type="number"
                  min={1}
                  max={16}
                  value={form.parallelism}
                  onChange={e => setField('parallelism', Math.max(1, Math.min(16, Number(e.target.value) || 1)))}
                />
              </label>
            )}
          </div>

          <div className="form-row">
            <label>Target Type
              <select value={form.target_type} onChange={e => setField('target_type', e.target.value as any)}>
                <option value="nfs">NFS</option>
                <option value="smb">SMB / CIFS</option>
              </select>
            </label>
            <label>Host
              <input value={form.host} onChange={e => setField('host', e.target.value)} placeholder="192.168.1.10" />
            </label>
            <label>{form.target_type === 'nfs' ? 'Export Path' : 'Share Name'}
              <input
                value={form.share}
                onChange={e => setField('share', e.target.value)}
                placeholder={form.target_type === 'nfs' ? '/exports/backup' : 'backup'}
              />
            </label>
          </div>

          {form.target_type === 'smb' && (
            <div className="form-row">
              <label>SMB User
                <input value={form.smb_user} onChange={e => setField('smb_user', e.target.value)} />
              </label>
              <label>SMB Password
                <input type="password" value={form.smb_pass} onChange={e => setField('smb_pass', e.target.value)} />
              </label>
            </div>
          )}

          <div className="form-row">
            <label>Local Path
              {localPaths.length > 0 ? (
                <select value={form.local_path} onChange={e => setField('local_path', e.target.value)}>
                  <option value="">— select or type below —</option>
                  {localPaths.map((p: any) => (
                    <option key={p.path} value={p.path}>{p.name} ({p.source}) — {p.path}</option>
                  ))}
                </select>
              ) : (
                <input value={form.local_path} onChange={e => setField('local_path', e.target.value)} placeholder="/mnt/mypool" />
              )}
            </label>
            {localPaths.length > 0 && (
              <label>Override Local Path
                <input
                  value={form.local_path}
                  onChange={e => setField('local_path', e.target.value)}
                  placeholder="/mnt/mypool"
                />
              </label>
            )}
            <label>Remote Subpath (optional)
              <input value={form.remote_path} onChange={e => setField('remote_path', e.target.value)} placeholder="backups/server1" />
            </label>
          </div>

          {formError && <div className="error-msg">{formError}</div>}

          <button className="btn primary" onClick={saveConfig} disabled={saving} style={{ marginTop: 8 }}>
            {saving ? 'Saving...' : 'Save Config'}
          </button>
        </div>
      )}

      {loading ? (
        <div className="empty-state">Loading...</div>
      ) : configs.length === 0 ? (
        <div className="empty-state">No backup configs yet. Add one above.</div>
      ) : (
        <div style={{ background: '#fff', borderRadius: 8, boxShadow: '0 1px 3px rgba(0,0,0,0.08)', overflow: 'hidden' }}>
          <table className="data-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Direction</th>
                <th>Method</th>
                <th>Target</th>
                <th>Local Path</th>
                <th>Remote Subpath</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {configs.map(cfg => {
                const run = runs[cfg.id];
                const running = isRunning(cfg.id);
                return (
                  <React.Fragment key={cfg.id}>
                    <tr>
                      <td>{cfg.name}</td>
                      <td>
                        <span className={`badge ${cfg.direction === 'push' ? 'primary' : 'secondary'}`}>
                          {cfg.direction === 'push' ? '↑ push' : '↓ pull'}
                        </span>
                      </td>
                      <td>
                        <span className="badge">
                          {cfg.method === 'rsync' ? 'rsync' : 'cp+sha256'}
                        </span>
                      </td>
                      <td>
                        <span className="badge">{cfg.target_type.toUpperCase()}</span>{' '}
                        {cfg.host}:{cfg.target_type === 'nfs' ? '' : '/'}{cfg.share}
                        {cfg.has_creds && <span style={{ marginLeft: 4, color: '#888', fontSize: 11 }}>({cfg.smb_user})</span>}
                      </td>
                      <td style={{ fontFamily: 'monospace', fontSize: 12 }}>{cfg.local_path}</td>
                      <td style={{ fontFamily: 'monospace', fontSize: 12, color: cfg.remote_path ? '#333' : '#bbb' }}>
                        {cfg.remote_path || '—'}
                      </td>
                      <td>
                        <div style={{ display: 'flex', gap: 8 }}>
                          <button
                            className="btn primary"
                            onClick={() => runBackup(cfg)}
                            disabled={running}
                            style={{ fontSize: 12 }}
                          >
                            {running ? 'Running...' : 'Run Now'}
                          </button>
                          <button
                            className="btn danger"
                            onClick={() => confirmDelete(cfg)}
                            disabled={running}
                            style={{ fontSize: 12 }}
                          >
                            Delete
                          </button>
                        </div>
                      </td>
                    </tr>
                    {run && (
                      <tr>
                        <td colSpan={7}>
                          <BackupRunPanel
                            run={run}
                            onDismiss={() => dismissRun(cfg.id)}
                            onCancel={() => api.cancelBackupRun(run.id).catch(() => {})}
                          />
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <ConfirmDialog
        visible={!!deleteTarget}
        title="Delete Backup Config"
        message={`Delete backup config "${deleteTarget?.name}"? This cannot be undone.`}
        confirmText="Delete"
        confirmClass="btn danger"
        onConfirm={doDelete}
        onCancel={() => setDeleteTarget(null)}
      />
    </div>
  );
}

function BackupRunPanel({ run, onDismiss, onCancel }: {
  run: BackupRun;
  onDismiss: () => void;
  onCancel: () => void;
}) {
  const [cancelling, setCancelling] = React.useState(false);
  const isDone = run.status !== 'running';
  const hasPct = run.progress_pct >= 0;
  const isCancelled = run.status === 'failed' && run.error === 'Cancelled';

  function handleCancel() {
    setCancelling(true);
    onCancel();
  }

  return (
    <div style={{
      background: run.status === 'failed' ? '#fff5f5' : run.status === 'completed' ? '#f0fff4' : '#f0f4ff',
      borderRadius: 6,
      padding: '10px 14px',
      fontSize: 13,
    }}>
      {/* Status / message line */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 6 }}>
        {!isDone && (
          <span style={{ color: cancelling ? '#888' : '#555', flex: 1 }}>
            {cancelling ? 'Cancelling...' : (run.progress || 'Running...')}
          </span>
        )}
        {run.status === 'completed' && (
          <span style={{ color: '#276749', flex: 1 }}>{run.summary || 'Backup complete'}</span>
        )}
        {run.status === 'failed' && (
          <span style={{ color: '#c53030', flex: 1 }}>
            {isCancelled ? 'Cancelled' : (run.error || 'Backup failed')}
          </span>
        )}
        {!isDone && (
          <button
            className="btn danger"
            onClick={handleCancel}
            disabled={cancelling}
            style={{ fontSize: 11, padding: '2px 8px' }}
          >
            {cancelling ? 'Cancelling...' : 'Cancel'}
          </button>
        )}
        {isDone && (
          <button className="btn secondary" onClick={onDismiss} style={{ fontSize: 11, padding: '2px 8px' }}>
            Dismiss
          </button>
        )}
      </div>

      {/* Progress bar */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <div style={{
          flex: 1,
          height: 6,
          background: '#d0d9f0',
          borderRadius: 3,
          overflow: 'hidden',
          position: 'relative',
        }}>
          {hasPct ? (
            <div style={{
              width: `${run.progress_pct}%`,
              height: '100%',
              background: run.status === 'completed' ? '#48bb78' : '#4c6ef5',
              borderRadius: 3,
              transition: 'width 0.4s ease',
            }} />
          ) : !isDone ? (
            <div style={{
              position: 'absolute',
              left: 0, top: 0, bottom: 0,
              width: '35%',
              background: '#4c6ef5',
              borderRadius: 3,
              animation: 'backup-slide 1.4s ease-in-out infinite',
            }} />
          ) : null}
        </div>
        {hasPct && (
          <span style={{ fontSize: 11, color: '#666', minWidth: 34, textAlign: 'right' }}>
            {run.progress_pct}%
          </span>
        )}
        {run.files_total > 0 && (
          <span style={{ fontSize: 11, color: '#888' }}>
            {run.files_done}/{run.files_total} files
          </span>
        )}
      </div>

      <style>{`
        @keyframes backup-slide {
          0%   { transform: translateX(-100%); }
          50%  { transform: translateX(185%); }
          100% { transform: translateX(185%); }
        }
      `}</style>
    </div>
  );
}
