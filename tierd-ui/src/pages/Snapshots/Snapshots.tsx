import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Snapshots() {
  const { t } = useI18n();
  const { snapshots, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [newSnap, setNewSnap] = useState({ dataset: '', name: '' });
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  useEffect(() => { if (snapshots !== undefined) setLoading(false); }, [snapshots]);
  useEffect(() => () => { stopPollRef.current?.(); }, []);

  function refresh() { invalidate('snapshots'); }

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
    api.createSnapshot(newSnap.dataset, newSnap.name).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        toast.success(t('snapshots.toast.created'));
        invalidate('snapshots');
      }, (err) => {
        setSubmitting(false);
        toast.error(t('snapshots.error.createPrefix', { err }));
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, t('snapshots.error.create')));
    });
  }

  function deleteSnap(name: string) {
    setConfirmTitle(t('snapshots.confirm.destroyTitle'));
    setConfirmMessage(t('snapshots.confirm.destroyMessage', { name }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      const id = name.replace(/\//g, '--').replace('@', '~');
      api.deleteSnapshot(id).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success(t('snapshots.toast.destroyed'));
          invalidate('snapshots');
        }, (err) => toast.error(t('arrays.error.destroyArrayPrefix', { err })));
      }).catch(e => toast.error(extractError(e, t('snapshots.error.destroy'))));
    };
    setConfirmVisible(true);
  }

  function rollback(name: string) {
    setConfirmTitle(t('snapshots.confirm.rollbackTitle'));
    setConfirmMessage(t('snapshots.confirm.rollbackMessage', { name }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      const id = name.replace(/\//g, '--').replace('@', '~');
      api.rollbackSnapshot(id).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success(t('snapshots.toast.rollbackDone'));
          invalidate('snapshots');
        }, (err) => toast.error(t('snapshots.error.rollbackPrefix', { err })));
      }).catch(e => toast.error(extractError(e, t('snapshots.error.rollback'))));
    };
    setConfirmVisible(true);
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8 }}>
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('snapshots.button.create')}</button>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('snapshots.create.title')}</h3>
          <div className="form-row">
            <label>{t('snapshots.field.dataset')}
              <input value={newSnap.dataset} onChange={e => setNewSnap(p => ({ ...p, dataset: e.target.value }))} placeholder="tank/data" />
            </label>
            <label>{t('snapshots.field.name')}
              <input value={newSnap.name} onChange={e => setNewSnap(p => ({ ...p, name: e.target.value }))} placeholder="backup-2026" />
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={submitting}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create} disabled={submitting || !newSnap.dataset.trim() || !newSnap.name.trim()}>
              {submitting ? t('arrays.creating') : t('arrays.button.create')}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text={t('snapshots.loading')} />
      {!loading && (
        snapshots.length === 0 ? (
          <div className="empty-state"><p>{t('snapshots.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>{t('datasets.col.name')}</th><th>{t('datasets.col.used')}</th><th>{t('snapshots.col.created')}</th><th>{t('arrays.col.actions')}</th></tr>
            </thead>
            <tbody>
              {snapshots.map((s: any) => (
                <tr key={s.name}>
                  <td><code>{s.name}</code></td>
                  <td>{s.used_human || s.used}</td>
                  <td>{s.created || '—'}</td>
                  <td className="action-cell">
                    <button className="btn secondary" onClick={() => rollback(s.name)}>{t('snapshots.action.rollback')}</button>
                    {' '}
                    <button className="btn danger" onClick={() => deleteSnap(s.name)}>{t('arrays.action.destroy')}</button>
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
        confirmText={t('arrays.confirm.confirm')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
