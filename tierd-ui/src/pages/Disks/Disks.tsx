import { useEffect, useRef, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import { pollJob } from '../../utils/poll';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Disks() {
  const { disks, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [wipingDisks, setWipingDisks] = useState(new Set<string>());
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    setLoading(false);
    return () => { stopPollRef.current?.(); };
  }, []);

  useEffect(() => {
    if (disks.length > 0 || !loading) setLoading(false);
  }, [disks]);

  function refresh() {
    setLoading(true);
    invalidate('disks');
    setTimeout(() => setLoading(false), 500);
  }

  function identify(name: string) {
    api.identifyDisk(name).catch(e => setError(extractError(e, 'Identify failed')));
  }

  function wipeDisk(disk: any) {
    setConfirmTitle('Wipe Disk');
    setConfirmMessage(`This will erase all filesystem signatures, partition tables, and RAID superblocks on ${disk.path}. All data on this disk will be lost.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      setWipingDisks(prev => new Set(prev).add(disk.name));
      api.wipeDisk(disk.name).then((res: any) => {
        stopPollRef.current = pollJob(
          res.job_id,
          null,
          () => {
            setWipingDisks(prev => { const s = new Set(prev); s.delete(disk.name); return s; });
            toast.success('Disk wiped');
            invalidate('disks');
          },
          (err) => {
            setWipingDisks(prev => { const s = new Set(prev); s.delete(disk.name); return s; });
            toast.error(err);
          }
        );
      }).catch(e => {
        setWipingDisks(prev => { const s = new Set(prev); s.delete(disk.name); return s; });
        toast.error(extractError(e, 'Wipe failed'));
      });
    };
    setConfirmVisible(true);
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Disks</h1>
        <p className="subtitle">Physical disk inventory and management</p>
        <button className="refresh-btn" onClick={refresh}>Refresh</button>
      </div>

      {error && <div className="error-msg">{error}</div>}
      <Spinner loading={loading} text="Loading disks..." />

      {!loading && (
        disks.length === 0 ? (
          <div className="empty-state">
            <div className="empty-icon">⊙</div>
            <p>No disks found.</p>
          </div>
        ) : (
          <table className="data-table">
            <thead>
              <tr>
                <th>Device</th><th>Model</th><th>Size</th><th>Type</th><th>Assignment</th><th>Temp</th><th>Health</th><th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {disks.map((disk: any) => (
                <tr key={disk.name}>
                  <td><code>{disk.path}</code></td>
                  <td>{disk.model || '—'}</td>
                  <td>{disk.size_human}</td>
                  <td><span className={`badge ${disk.type?.toLowerCase()}`}>{disk.type}</span></td>
                  <td><span className="badge assignment">{disk.assignment || 'unassigned'}</span></td>
                  <td>{disk.temp_c != null ? `${disk.temp_c}°C` : '—'}</td>
                  <td>
                    {disk.smart_status && (
                      <span className={`badge ${disk.smart_status === 'PASSED' ? 'healthy' : 'critical'}`}>
                        {disk.smart_status}
                      </span>
                    )}
                  </td>
                  <td className="action-cell">
                    <button className="btn secondary" onClick={() => identify(disk.name)} title="Blink disk LED">Identify</button>
                    {' '}
                    <button
                      className="btn danger"
                      onClick={() => wipeDisk(disk)}
                      disabled={wipingDisks.has(disk.name) || disk.assignment !== 'unassigned'}
                      title={disk.assignment !== 'unassigned' ? 'Disk is in use' : 'Wipe disk'}
                    >
                      {wipingDisks.has(disk.name) ? 'Wiping...' : 'Wipe'}
                    </button>
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
        confirmText="Wipe"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
