import { useEffect, useRef, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Snapshots() {
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
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || 'Job failed'); }
      }).catch(() => { clearInterval(timer); onError('Lost connection'); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function create() {
    setSubmitting(true);
    api.createSnapshot(newSnap.dataset, newSnap.name).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        toast.success('Snapshot created');
        invalidate('snapshots');
      }, (err) => {
        setSubmitting(false);
        toast.error('Failed to create snapshot: ' + err);
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, 'Failed to create snapshot'));
    });
  }

  function deleteSnap(name: string) {
    setConfirmTitle('Destroy Snapshot');
    setConfirmMessage(`This will permanently destroy snapshot "${name}". This cannot be undone.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      const id = name.replace(/\//g, '--').replace('@', '~');
      api.deleteSnapshot(id).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success('Snapshot destroyed');
          invalidate('snapshots');
        }, (err) => toast.error('Destroy failed: ' + err));
      }).catch(e => toast.error(extractError(e, 'Failed to destroy snapshot')));
    };
    setConfirmVisible(true);
  }

  function rollback(name: string) {
    setConfirmTitle('Rollback Snapshot');
    setConfirmMessage(`Rollback to "${name}"? This will destroy all newer snapshots.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      const id = name.replace(/\//g, '--').replace('@', '~');
      api.rollbackSnapshot(id).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success('Rollback complete');
          invalidate('snapshots');
        }, (err) => toast.error('Rollback failed: ' + err));
      }).catch(e => toast.error(extractError(e, 'Rollback failed')));
    };
    setConfirmVisible(true);
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8 }}>
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Create Snapshot</button>
        <button className="refresh-btn" onClick={refresh}>Refresh</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Create Snapshot</h3>
          <div className="form-row">
            <label>Dataset
              <input value={newSnap.dataset} onChange={e => setNewSnap(p => ({ ...p, dataset: e.target.value }))} placeholder="tank/data" />
            </label>
            <label>Snapshot Name
              <input value={newSnap.name} onChange={e => setNewSnap(p => ({ ...p, name: e.target.value }))} placeholder="backup-2026" />
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={submitting}>Cancel</button>
            <button className="btn primary" onClick={create} disabled={submitting || !newSnap.dataset.trim() || !newSnap.name.trim()}>
              {submitting ? 'Creating...' : 'Create'}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text="Loading snapshots..." />
      {!loading && (
        snapshots.length === 0 ? (
          <div className="empty-state"><p>No snapshots.</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>Name</th><th>Used</th><th>Created</th><th>Actions</th></tr>
            </thead>
            <tbody>
              {snapshots.map((s: any) => (
                <tr key={s.name}>
                  <td><code>{s.name}</code></td>
                  <td>{s.used_human || s.used}</td>
                  <td>{s.created || '—'}</td>
                  <td className="action-cell">
                    <button className="btn secondary" onClick={() => rollback(s.name)}>Rollback</button>
                    {' '}
                    <button className="btn danger" onClick={() => deleteSnap(s.name)}>Destroy</button>
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
        confirmText="Confirm"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
