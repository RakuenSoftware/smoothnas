import { useEffect, useRef, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Zvols() {
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
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || 'Job failed'); }
      }).catch(() => { clearInterval(timer); onError('Lost connection'); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function create() {
    setSubmitting(true);
    api.createZvol(newZvol).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        toast.success('Zvol created');
        invalidate('zvols');
      }, (err) => {
        setSubmitting(false);
        toast.error('Failed to create zvol: ' + err);
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, 'Failed to create zvol'));
    });
  }

  function deleteZvol(name: string) {
    setConfirmTitle('Destroy Zvol');
    setConfirmMessage(`This will permanently destroy zvol "${name}". This cannot be undone.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteZvol(name.replace(/\//g, '--')).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success('Zvol destroyed');
          invalidate('zvols');
        }, (err) => toast.error('Destroy failed: ' + err));
      }).catch(e => toast.error(extractError(e, 'Failed to destroy zvol')));
    };
    setConfirmVisible(true);
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8 }}>
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Create Zvol</button>
        <button className="refresh-btn" onClick={refresh}>Refresh</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Create Zvol</h3>
          <div className="form-row">
            <label>Name (pool/zvol)
              <input value={newZvol.name} onChange={e => setNewZvol(p => ({ ...p, name: e.target.value }))} placeholder="tank/vol0" />
            </label>
            <label>Size (e.g. 100G)
              <input value={newZvol.size} onChange={e => setNewZvol(p => ({ ...p, size: e.target.value }))} placeholder="100G" />
            </label>
            <label>Block Size (optional)
              <input value={newZvol.block_size} onChange={e => setNewZvol(p => ({ ...p, block_size: e.target.value }))} placeholder="4K" />
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={submitting}>Cancel</button>
            <button className="btn primary" onClick={create} disabled={submitting || !newZvol.name.trim() || !newZvol.size.trim()}>
              {submitting ? 'Creating...' : 'Create'}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text="Loading zvols..." />
      {!loading && (
        zvols.length === 0 ? (
          <div className="empty-state"><p>No zvols.</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>Name</th><th>Size</th><th>Block Size</th><th>Actions</th></tr>
            </thead>
            <tbody>
              {zvols.map((z: any) => (
                <tr key={z.name}>
                  <td><code>{z.name}</code></td>
                  <td>{z.size_human || z.size}</td>
                  <td>{z.block_size || '—'}</td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteZvol(z.name)}>Destroy</button>
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
        confirmText="Destroy"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
