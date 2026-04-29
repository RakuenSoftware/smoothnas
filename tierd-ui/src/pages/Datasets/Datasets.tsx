import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Datasets() {
  const { t } = useI18n();
  const { datasets, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [newDs, setNewDs] = useState({ name: '', mount_point: '', compression: 'lz4', quota: 0, reservation: 0 });
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  useEffect(() => { if (datasets !== undefined) setLoading(false); }, [datasets]);
  useEffect(() => () => { stopPollRef.current?.(); }, []);

  function refresh() { invalidate('datasets'); }

  function pollJob(jobId: string, onComplete: () => void, onError: (err: string) => void): () => void {
    const timer = setInterval(() => {
      api.getJobStatus(jobId).then((job: any) => {
        if (job.status === 'completed') { clearInterval(timer); onComplete(); }
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || t('arrays.error.jobFailed')); }
      }).catch(() => { clearInterval(timer); onError(t('arrays.error.lostConnection')); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function create() {
    setSubmitting(true);
    api.createDataset(newDs).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        toast.success(t('datasets.toast.created'));
        invalidate('datasets');
      }, (err) => {
        setSubmitting(false);
        toast.error(t('datasets.error.createPrefix', { err }));
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, t('datasets.error.create')));
    });
  }

  function deleteDs(id: string) {
    setConfirmTitle(t('datasets.confirm.destroyTitle'));
    setConfirmMessage(t('datasets.confirm.destroyMessage', { name: id }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteDataset(id.replace(/\//g, '--')).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success(t('datasets.toast.destroyed'));
          invalidate('datasets');
        }, (err) => toast.error(t('arrays.error.destroyArrayPrefix', { err })));
      }).catch(e => toast.error(extractError(e, t('datasets.error.destroy'))));
    };
    setConfirmVisible(true);
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8 }}>
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('datasets.button.create')}</button>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('datasets.create.title')}</h3>
          <div className="form-row">
            <label>{t('datasets.field.name')}
              <input value={newDs.name} onChange={e => setNewDs(p => ({ ...p, name: e.target.value }))} placeholder="tank/data" />
            </label>
            <label>{t('datasets.field.mountPoint')}
              <input value={newDs.mount_point} onChange={e => setNewDs(p => ({ ...p, mount_point: e.target.value }))} placeholder="/mnt/data" />
            </label>
          </div>
          <div className="form-row">
            <label>{t('datasets.field.compression')}
              <select value={newDs.compression} onChange={e => setNewDs(p => ({ ...p, compression: e.target.value }))}>
                <option value="off">{t('datasets.compression.off')}</option>
                <option value="lz4">{t('datasets.compression.lz4')}</option>
                <option value="gzip">{t('datasets.compression.gzip')}</option>
                <option value="zstd">{t('datasets.compression.zstd')}</option>
              </select>
            </label>
            <label>{t('datasets.field.quota')}
              <input type="number" value={newDs.quota} onChange={e => setNewDs(p => ({ ...p, quota: parseInt(e.target.value) || 0 }))} />
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={submitting}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create} disabled={submitting || !newDs.name.trim()}>
              {submitting ? t('arrays.creating') : t('arrays.button.create')}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text={t('datasets.loading')} />
      {!loading && (
        datasets.length === 0 ? (
          <div className="empty-state"><p>{t('datasets.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>{t('datasets.col.name')}</th><th>{t('datasets.col.mount')}</th><th>{t('datasets.col.used')}</th><th>{t('datasets.col.avail')}</th><th>{t('datasets.col.compression')}</th><th>{t('arrays.col.actions')}</th></tr>
            </thead>
            <tbody>
              {datasets.map((ds: any) => (
                <tr key={ds.name}>
                  <td><code>{ds.name}</code></td>
                  <td>{ds.mount_point || '—'}</td>
                  <td>{ds.used_human || ds.used}</td>
                  <td>{ds.avail_human || ds.avail}</td>
                  <td>{ds.compression}</td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteDs(ds.name)}>{t('arrays.action.destroy')}</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )
      )}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText={t('arrays.action.destroy')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
