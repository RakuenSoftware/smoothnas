import { useEffect, useRef, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Datasets() {
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
        else if (job.status === 'failed') { clearInterval(timer); onError(job.error || 'Job failed'); }
      }).catch(() => { clearInterval(timer); onError('Lost connection'); });
    }, 2000);
    return () => clearInterval(timer);
  }

  function create() {
    setSubmitting(true);
    api.createDataset(newDs).then((res: any) => {
      stopPollRef.current = pollJob(res.job_id, () => {
        setSubmitting(false);
        setShowCreate(false);
        toast.success('Dataset created');
        invalidate('datasets');
      }, (err) => {
        setSubmitting(false);
        toast.error('Failed to create dataset: ' + err);
      });
    }).catch(e => {
      setSubmitting(false);
      toast.error(extractError(e, 'Failed to create dataset'));
    });
  }

  function deleteDs(id: string) {
    setConfirmTitle('Destroy Dataset');
    setConfirmMessage(`This will permanently destroy dataset "${id}". This cannot be undone.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteDataset(id.replace(/\//g, '--')).then((res: any) => {
        stopPollRef.current = pollJob(res.job_id, () => {
          toast.success('Dataset destroyed');
          invalidate('datasets');
        }, (err) => toast.error('Destroy failed: ' + err));
      }).catch(e => toast.error(extractError(e, 'Failed to destroy dataset')));
    };
    setConfirmVisible(true);
  }

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8 }}>
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Create Dataset</button>
        <button className="refresh-btn" onClick={refresh}>Refresh</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Create Dataset</h3>
          <div className="form-row">
            <label>Name (pool/dataset)
              <input value={newDs.name} onChange={e => setNewDs(p => ({ ...p, name: e.target.value }))} placeholder="tank/data" />
            </label>
            <label>Mount Point
              <input value={newDs.mount_point} onChange={e => setNewDs(p => ({ ...p, mount_point: e.target.value }))} placeholder="/mnt/data" />
            </label>
          </div>
          <div className="form-row">
            <label>Compression
              <select value={newDs.compression} onChange={e => setNewDs(p => ({ ...p, compression: e.target.value }))}>
                <option value="off">Off</option>
                <option value="lz4">LZ4</option>
                <option value="gzip">GZIP</option>
                <option value="zstd">ZSTD</option>
              </select>
            </label>
            <label>Quota (bytes, 0=none)
              <input type="number" value={newDs.quota} onChange={e => setNewDs(p => ({ ...p, quota: parseInt(e.target.value) || 0 }))} />
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={submitting}>Cancel</button>
            <button className="btn primary" onClick={create} disabled={submitting || !newDs.name.trim()}>
              {submitting ? 'Creating...' : 'Create'}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} text="Loading datasets..." />
      {!loading && (
        datasets.length === 0 ? (
          <div className="empty-state"><p>No datasets.</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>Name</th><th>Mount</th><th>Used</th><th>Avail</th><th>Compression</th><th>Actions</th></tr>
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
                    <button className="btn danger" onClick={() => deleteDs(ds.name)}>Destroy</button>
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
