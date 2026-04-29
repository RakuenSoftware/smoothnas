import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Zvols() {
  const { t } = useI18n();
  const { zvols, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [newZvol, setNewZvol] = useState({ name: '', size: '', block_size: '' });
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  useEffect(() => { if (zvols !== undefined) setLoading(false); }, [zvols]);
  useEffect(() => () => { stopPollRef.current?.(); }, []);

  function refresh() { invalidate('zvols'); }

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
    api.createZvol(newZvol).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        toast.success(t('zvols.toast.created'));
        invalidate('zvols');
      }, (err) => {
        setSubmitting(false);
        toast.error(t('zvols.error.createPrefix', { err }));
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, t('zvols.error.create')));
    });
  }

  function deleteZvol(name: string) {
    setConfirmTitle(t('zvols.confirm.destroyTitle'));
    setConfirmMessage(t('zvols.confirm.destroyMessage', { name }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteZvol(name.replace(/\//g, '--')).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success(t('zvols.toast.destroyed'));
          invalidate('zvols');
        }, (err) => toast.error(t('arrays.error.destroyArrayPrefix', { err })));
      }).catch(e => toast.error(extractError(e, t('zvols.error.destroy'))));
    };
    setConfirmVisible(true);
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8 }}>
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('zvols.button.create')}</button>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('zvols.create.title')}</h3>
          <div className="form-row">
            <label>{t('zvols.field.name')}
              <input value={newZvol.name} onChange={e => setNewZvol(p => ({ ...p, name: e.target.value }))} placeholder="tank/vol0" />
            </label>
            <label>{t('zvols.field.size')}
              <input value={newZvol.size} onChange={e => setNewZvol(p => ({ ...p, size: e.target.value }))} placeholder="100G" />
            </label>
            <label>{t('zvols.field.blockSize')}
              <input value={newZvol.block_size} onChange={e => setNewZvol(p => ({ ...p, block_size: e.target.value }))} placeholder="4K" />
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={submitting}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create} disabled={submitting || !newZvol.name.trim() || !newZvol.size.trim()}>
              {submitting ? t('arrays.creating') : t('arrays.button.create')}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text={t('zvols.loading')} />
      {!loading && (
        zvols.length === 0 ? (
          <div className="empty-state"><p>{t('zvols.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>{t('datasets.col.name')}</th><th>{t('zvols.col.size')}</th><th>{t('zvols.col.blockSize')}</th><th>{t('arrays.col.actions')}</th></tr>
            </thead>
            <tbody>
              {zvols.map((z: any) => (
                <tr key={z.name}>
                  <td><code>{z.name}</code></td>
                  <td>{z.size_human || z.size}</td>
                  <td>{z.block_size || '—'}</td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteZvol(z.name)}>{t('arrays.action.destroy')}</button>
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
