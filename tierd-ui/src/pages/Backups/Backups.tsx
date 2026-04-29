import React, { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
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
  ssh_user: string;
  has_ssh_creds: boolean;
  local_path: string;
  remote_path: string;
  direction: 'push' | 'pull';
  method: 'cp' | 'rsync';
  parallelism: number;
  use_ssh: boolean;
  compress: boolean;
  delete_mode: boolean;
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
  ssh_user: '',
  ssh_pass: '',
  local_path: '',
  remote_path: '',
  direction: 'push' as 'push' | 'pull',
  method: 'rsync' as 'cp' | 'rsync',
  parallelism: 1,
  use_ssh: false,
  compress: false,
  delete_mode: false,
};

function cfgHasSSHCreds(id: number, configs: BackupConfig[]): boolean {
  return !!configs.find(c => c.id === id)?.has_ssh_creds;
}
function cfgHasSMBCreds(id: number, configs: BackupConfig[]): boolean {
  return !!configs.find(c => c.id === id)?.has_creds;
}

// parseRateFromProgress extracts bytes/sec from a watchdog progress string like
// "rsync: 12.34MB/s". Returns Infinity for non-rate strings so they never
// trigger the stall detector.
function parseRateFromProgress(progress: string): number {
  const m = progress.match(/rsync:\s*([\d.]+)(GB|MB|KB|B)\/s/);
  if (!m) return Infinity;
  const v = parseFloat(m[1]);
  switch (m[2]) {
    case 'GB': return v * 1e9;
    case 'MB': return v * 1e6;
    case 'KB': return v * 1e3;
    default:   return v;
  }
}

export default function Backups() {
  const { t } = useI18n();
  const { success: toastSuccess, error: toastError } = useToast();
  const [configs, setConfigs] = useState<BackupConfig[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({ ...EMPTY_FORM });
  const [saving, setSaving] = useState(false);
  const [formError, setFormError] = useState('');
  // When non-null, the form edits this existing config instead of creating one.
  const [editingId, setEditingId] = useState<number | null>(null);
  const [localPaths, setLocalPaths] = useState<any[]>([]);
  const [runs, setRuns] = useState<Record<number, BackupRun>>({});
  const [deleteTarget, setDeleteTarget] = useState<BackupConfig | null>(null);
  const pollRefs = useRef<Record<number, ReturnType<typeof setInterval>>>({});
  // Per-run stall tracking: timestamp of last sample above the stall threshold.
  const stallRefs = useRef<Record<number, { lastHealthyAt: number; notified: boolean }>>({});

  useEffect(() => {
    api.getFilesystemPaths().then(setLocalPaths).catch(() => {});
    load();
    // Capture pollRefs.current at effect-setup time so the cleanup
    // sees the same map even if pollRefs is reassigned later.
    const intervals = pollRefs.current;
    return () => {
      Object.values(intervals).forEach(clearInterval);
    };
  // eslint-disable-next-line react-hooks/exhaustive-deps
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

  function setField(key: keyof typeof EMPTY_FORM, value: string | number | boolean) {
    setForm(f => ({ ...f, [key]: value }));
  }

  async function saveConfig() {
    setFormError('');
    setSaving(true);
    try {
      if (editingId != null) {
        await api.updateBackupConfig(editingId, form);
      } else {
        await api.createBackupConfig(form);
      }
      setForm({ ...EMPTY_FORM });
      setShowForm(false);
      setEditingId(null);
      load();
      toastSuccess(editingId != null ? t('backups.toast.updated') : t('backups.toast.created'));
    } catch (e: any) {
      setFormError(e.message || t('backups.error.save'));
    } finally {
      setSaving(false);
    }
  }

  function startEdit(cfg: BackupConfig) {
    // Passwords are never returned by the API — leave blank. The backend
    // treats blank smb_pass / ssh_pass on update as "keep existing secret".
    setForm({
      name: cfg.name,
      target_type: cfg.target_type,
      host: cfg.host,
      share: cfg.share,
      smb_user: cfg.smb_user,
      smb_pass: '',
      ssh_user: cfg.ssh_user,
      ssh_pass: '',
      local_path: cfg.local_path,
      remote_path: cfg.remote_path,
      direction: cfg.direction,
      method: cfg.method,
      parallelism: cfg.parallelism,
      use_ssh: cfg.use_ssh,
      compress: cfg.compress,
      delete_mode: cfg.delete_mode,
    });
    setEditingId(cfg.id);
    setFormError('');
    setShowForm(true);
  }

  function cancelForm() {
    setShowForm(false);
    setEditingId(null);
    setForm({ ...EMPTY_FORM });
    setFormError('');
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
      toastSuccess(t('backups.toast.deleted'));
    } catch (e: any) {
      toastError(e.message || t('backups.error.delete'));
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
          progress: t('backups.status.starting'), files_done: 0, files_total: -1,
          progress_pct: -1, error: '', summary: '',
          started_at: new Date().toISOString(), completed_at: '',
        },
      }));
      pollRun(cfg.id, runId);
    }).catch((e: any) => {
      toastError(e.message || t('backups.error.start'));
    });
  }

  function pollRun(cfgId: number, runId: number) {
    if (pollRefs.current[cfgId]) clearInterval(pollRefs.current[cfgId]);
    stallRefs.current[cfgId] = { lastHealthyAt: Date.now(), notified: false };
    pollRefs.current[cfgId] = setInterval(() => {
      api.getBackupRun(runId).then((run: BackupRun) => {
        setRuns(prev => ({ ...prev, [cfgId]: run }));
        if (run.status === 'completed') {
          clearInterval(pollRefs.current[cfgId]);
          delete stallRefs.current[cfgId];
          toastSuccess(t('backups.toast.complete'));
        } else if (run.status === 'failed') {
          clearInterval(pollRefs.current[cfgId]);
          delete stallRefs.current[cfgId];
          if (run.error && run.error !== 'Cancelled') {
            toastError(t('backups.toast.stopped', { err: run.error }));
          }
        } else if (run.status === 'running') {
          // Stall detection: fire once when rate has been below 10 KB/s for 30s.
          const stallState = stallRefs.current[cfgId];
          if (stallState) {
            const bytesPerSec = parseRateFromProgress(run.progress);
            if (bytesPerSec >= 10_000) {
              stallState.lastHealthyAt = Date.now();
              stallState.notified = false;
            } else if (!stallState.notified && Date.now() - stallState.lastHealthyAt > 30_000) {
              stallState.notified = true;
              toastError(t('backups.toast.stalled'));
            }
          }
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
        <h1>{t('backups.title')}</h1>
        <p className="subtitle">{t('backups.subtitle')}</p>
        <button className="btn primary" onClick={() => {
          if (showForm) { cancelForm(); }
          else { setShowForm(true); setFormError(''); }
        }}>
          {showForm ? t('common.cancel') : t('backups.button.add')}
        </button>
      </div>

      {showForm && (
        <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
          <h3 style={{ margin: '0 0 16px' }}>{editingId != null ? t('backups.form.editTitle') : t('backups.form.newTitle')}</h3>

          <div className="form-row">
            <label>{t('datasets.col.name')}
              {/* i18n-allow: example value, illustrative placeholder */}
              <input value={form.name} onChange={e => setField('name', e.target.value)} placeholder="e.g. nightly-push" />
            </label>
            <label>{t('backups.field.direction')}
              <select value={form.direction} onChange={e => setField('direction', e.target.value as any)}>
                <option value="push">{t('backups.direction.push')}</option>
                <option value="pull">{t('backups.direction.pull')}</option>
              </select>
            </label>
            <label>{t('backups.field.method')}
              <select value={form.method} onChange={e => setField('method', e.target.value as any)}>
                <option value="rsync">rsync</option>
                <option value="cp">{t('backups.method.cp')}</option>
              </select>
            </label>
          </div>

          {form.method === 'rsync' && (
            <div className="form-row">
              <label style={{ flexDirection: 'row', alignItems: 'center', gap: 6 }} title={t('backups.tooltip.useSsh')}>
                <input
                  type="checkbox"
                  checked={form.use_ssh}
                  onChange={e => {
                    const on = e.target.checked;
                    // Compression only does anything in SSH transport mode —
                    // in mount mode --compress only compresses the in-process
                    // rsync pipe, not the NFS/SMB wire. Clear it when SSH is
                    // turned off so the stored config stays honest.
                    setForm(f => ({ ...f, use_ssh: on, compress: on ? f.compress : false }));
                  }}
                />
                {t('backups.field.useSsh')}
              </label>
              {form.use_ssh && (
                <label style={{ flexDirection: 'row', alignItems: 'center', gap: 6 }} title={t('backups.tooltip.compress')}>
                  <input
                    type="checkbox"
                    checked={form.compress}
                    onChange={e => setField('compress', e.target.checked)}
                  />
                  {t('backups.field.compression')}
                </label>
              )}
              <label style={{ flexDirection: 'row', alignItems: 'center', gap: 6 }} title={t('backups.tooltip.delete')}>
                <input
                  type="checkbox"
                  checked={form.delete_mode}
                  onChange={e => setField('delete_mode', e.target.checked)}
                />
                {t('backups.field.deleteExtraneous')}
              </label>
            </div>
          )}

          <div className="form-row">
            <label>{t('backups.field.host')}
              <input value={form.host} onChange={e => setField('host', e.target.value)} placeholder="192.168.1.10" />
            </label>
            <label>{(form.method === 'rsync' && form.use_ssh) ? t('backups.field.remotePath') : (form.target_type === 'nfs' ? t('backups.field.exportPath') : t('backups.field.shareName'))}
              <input
                value={form.share}
                onChange={e => setField('share', e.target.value)}
                placeholder={(form.method === 'rsync' && form.use_ssh) ? '/volume1/backup' : (form.target_type === 'nfs' ? '/exports/backup' : 'backup')}
              />
            </label>
          </div>

          {form.method === 'rsync' && form.use_ssh && (
            <div className="form-row">
              <label title={t('backups.tooltip.sshUser')}>
                {t('backups.field.sshUser')}
                <input value={form.ssh_user} onChange={e => setField('ssh_user', e.target.value)} placeholder="root" />
              </label>
              <label title={t('backups.tooltip.sshPass')}>
                {t('backups.field.sshPass')}
                <input
                  type="password"
                  value={form.ssh_pass}
                  onChange={e => setField('ssh_pass', e.target.value)}
                  placeholder={editingId != null && cfgHasSSHCreds(editingId, configs) ? t('backups.field.unchanged') : ''}
                />
              </label>
            </div>
          )}

          {(form.method === 'cp' || (form.method === 'rsync' && !form.use_ssh)) && (
            <div className="form-row">
              <label>{t('backups.field.targetType')}
                <select value={form.target_type} onChange={e => setField('target_type', e.target.value as any)}>
                  <option value="nfs">NFS</option>
                  <option value="smb">{t('backups.targetType.smb')}</option>
                </select>
              </label>
              {form.target_type === 'smb' && (
                <>
                  <label>{t('backups.field.smbUser')}
                    <input value={form.smb_user} onChange={e => setField('smb_user', e.target.value)} />
                  </label>
                  <label title={t('backups.tooltip.smbPass')}>
                    {t('backups.field.smbPass')}
                    <input
                      type="password"
                      value={form.smb_pass}
                      onChange={e => setField('smb_pass', e.target.value)}
                      placeholder={editingId != null && cfgHasSMBCreds(editingId, configs) ? t('backups.field.unchanged') : ''}
                    />
                  </label>
                </>
              )}
            </div>
          )}

          <div className="form-row">
            <label>{t('backups.field.localPath')}
              {localPaths.length > 0 ? (
                <select value={form.local_path} onChange={e => setField('local_path', e.target.value)}>
                  <option value="">{t('backups.field.localPathPlaceholder')}</option>
                  {localPaths.map((p: any) => (
                    <option key={p.path} value={p.path}>{p.name} ({p.source}) — {p.path}</option>
                  ))}
                </select>
              ) : (
                <input value={form.local_path} onChange={e => setField('local_path', e.target.value)} placeholder="/mnt/mypool" />
              )}
            </label>
            {localPaths.length > 0 && (
              <label>{t('backups.field.overrideLocalPath')}
                <input
                  value={form.local_path}
                  onChange={e => setField('local_path', e.target.value)}
                  placeholder="/mnt/mypool"
                />
              </label>
            )}
            <label>{t('backups.field.remoteSubpath')}
              <input value={form.remote_path} onChange={e => setField('remote_path', e.target.value)} placeholder="backups/server1" />
            </label>
          </div>

          {formError && <div className="error-msg">{formError}</div>}

          <button className="btn primary" onClick={saveConfig} disabled={saving} style={{ marginTop: 8 }}>
            {saving ? t('backups.button.saving') : (editingId != null ? t('backups.button.update') : t('backups.button.save'))}
          </button>
        </div>
      )}

      {loading ? (
        <div className="empty-state">{t('common.loading')}</div>
      ) : configs.length === 0 ? (
        <div className="empty-state">{t('backups.empty')}</div>
      ) : (
        <div style={{ background: '#fff', borderRadius: 8, boxShadow: '0 1px 3px rgba(0,0,0,0.08)', overflow: 'hidden' }}>
          <table className="data-table">
            <thead>
              <tr>
                <th>{t('datasets.col.name')}</th>
                <th>{t('backups.col.direction')}</th>
                <th>{t('backups.col.method')}</th>
                <th>{t('backups.col.target')}</th>
                <th>{t('backups.col.localPath')}</th>
                <th>{t('backups.col.remoteSubpath')}</th>
                <th>{t('arrays.col.actions')}</th>
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
                          {cfg.direction === 'push' ? t('backups.badge.push') : t('backups.badge.pull')}
                        </span>
                      </td>
                      <td>
                        <span className="badge">
                          {cfg.method === 'rsync' ? 'rsync' : 'cp+sha256'}
                        </span>
                      </td>
                      <td>
                        {cfg.method === 'rsync' && cfg.use_ssh ? (
                          <>
                            <span className="badge">SSH</span>{' '}
                            {cfg.ssh_user ? `${cfg.ssh_user}@` : ''}{cfg.host}:{cfg.share}
                            {cfg.has_ssh_creds && <span style={{ marginLeft: 4, color: '#888', fontSize: 11 }}>{t('backups.badge.password')}</span>}
                            {cfg.compress && <span className="badge" style={{ marginLeft: 4 }}>zstd</span>}
                            {cfg.delete_mode && <span className="badge" style={{ marginLeft: 4 }}>delete</span>}
                          </>
                        ) : (
                          <>
                            <span className="badge">{cfg.target_type.toUpperCase()}</span>{' '}
                            {cfg.host}:{cfg.target_type === 'nfs' ? '' : '/'}{cfg.share}
                            {cfg.has_creds && <span style={{ marginLeft: 4, color: '#888', fontSize: 11 }}>({cfg.smb_user})</span>}
                            {cfg.method === 'rsync' && cfg.compress && <span className="badge" style={{ marginLeft: 4 }}>zstd</span>}
                            {cfg.method === 'rsync' && cfg.delete_mode && <span className="badge" style={{ marginLeft: 4 }}>delete</span>}
                          </>
                        )}
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
                            {running ? t('backups.button.running') : t('backups.button.runNow')}
                          </button>
                          <button
                            className="btn secondary"
                            onClick={() => startEdit(cfg)}
                            disabled={running}
                            style={{ fontSize: 12 }}
                          >
                            {t('common.edit')}
                          </button>
                          <button
                            className="btn danger"
                            onClick={() => confirmDelete(cfg)}
                            disabled={running}
                            style={{ fontSize: 12 }}
                          >
                            {t('common.delete')}
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
        title={t('backups.confirm.deleteTitle')}
        message={t('backups.confirm.deleteMessage', { name: deleteTarget?.name || '' })}
        confirmText={t('common.delete')}
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
  const { t } = useI18n();
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
            {cancelling ? t('backups.run.cancelling') : (run.progress || t('backups.run.running'))}
          </span>
        )}
        {run.status === 'completed' && (
          <span style={{ color: '#276749', flex: 1 }}>{run.summary || t('backups.run.complete')}</span>
        )}
        {run.status === 'failed' && (
          <span style={{ color: '#c53030', flex: 1 }}>
            {isCancelled ? t('backups.run.cancelled') : (run.error || t('backups.run.failed'))}
          </span>
        )}
        {!isDone && (
          <button
            className="btn danger"
            onClick={handleCancel}
            disabled={cancelling}
            style={{ fontSize: 11, padding: '2px 8px' }}
          >
            {cancelling ? t('backups.run.cancelling') : t('common.cancel')}
          </button>
        )}
        {isDone && (
          <button className="btn secondary" onClick={onDismiss} style={{ fontSize: 11, padding: '2px 8px' }}>
            {t('backups.run.dismiss')}
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
            {t('backups.run.fileCount', { done: run.files_done, total: run.files_total })}
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
