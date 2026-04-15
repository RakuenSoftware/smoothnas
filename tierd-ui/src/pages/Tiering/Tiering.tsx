import { useEffect, useRef, useState } from 'react';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

function formatTs(ts: string): string {
  if (!ts) return '—';
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

export default function Tiering() {
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [namespaces, setNamespaces] = useState<any[]>([]);
  const [selectedNS, setSelectedNS] = useState<any>(null);
  const [nsLoading, setNsLoading] = useState(false);
  const [snapshots, setSnapshots] = useState<any[]>([]);
  const [snapsLoading, setSnapsLoading] = useState(false);
  const [snapCreating, setSnapCreating] = useState(false);

  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);

  useEffect(() => { loadNamespaces(); }, []);

  function loadNamespaces() {
    setLoading(true);
    setError('');
    api.listTieringNamespaces()
      .then(ns => {
        setNamespaces(ns || []);
        setLoading(false);
      })
      .catch(e => {
        const msg = extractError(e, 'Failed to load tiering namespaces');
        setError(msg);
        setLoading(false);
      });
  }

  function selectNamespace(ns: any) {
    setNsLoading(true);
    setSnapshots([]);
    api.getTieringNamespace(ns.id)
      .then(full => {
        setSelectedNS(full);
        setNsLoading(false);
        if (full.snapshot_mode === 'coordinated-namespace') {
          loadSnapshots(full.id);
        }
      })
      .catch(e => {
        toast.error(extractError(e, 'Failed to load namespace'));
        setNsLoading(false);
      });
  }

  function loadSnapshots(nsID: string) {
    setSnapsLoading(true);
    api.listTieringNamespaceSnapshots(nsID)
      .then(snaps => {
        setSnapshots(snaps || []);
        setSnapsLoading(false);
      })
      .catch(e => {
        toast.error(extractError(e, 'Failed to load snapshots'));
        setSnapsLoading(false);
      });
  }

  function createSnapshot() {
    if (!selectedNS) return;
    setSnapCreating(true);
    api.createTieringNamespaceSnapshot(selectedNS.id)
      .then(() => {
        toast.success('Snapshot created');
        loadSnapshots(selectedNS.id);
        setSnapCreating(false);
      })
      .catch(e => {
        toast.error(extractError(e, 'Failed to create snapshot'));
        setSnapCreating(false);
      });
  }

  function confirmDeleteSnapshot(snap: any) {
    setConfirmTitle('Delete Snapshot');
    setConfirmMessage(
      `Delete snapshot ${snap.snapshot_id}? This will permanently destroy the ZFS snapshot data for all backing datasets. This cannot be undone.`
    );
    confirmAction.current = () => deleteSnapshot(snap.snapshot_id);
    setConfirmVisible(true);
  }

  function deleteSnapshot(snapID: string) {
    setConfirmVisible(false);
    api.deleteTieringNamespaceSnapshot(selectedNS.id, snapID)
      .then(() => {
        toast.success('Snapshot deleted');
        setSnapshots(prev => prev.filter(s => s.snapshot_id !== snapID));
      })
      .catch(e => {
        toast.error(extractError(e, 'Failed to delete snapshot'));
      });
  }

  function isZFSManaged(ns: any) {
    return ns && ns.backend_kind === 'zfs-managed';
  }

  return (
    <div className="page-content">
      <div className="page-header">
        <h1>Tiering</h1>
        <button className="btn secondary" onClick={loadNamespaces}>Refresh</button>
      </div>

      <Spinner loading={loading} />
      {error && <div className="error-message">{error}</div>}

      {!loading && !error && (
        <div style={{ display: 'flex', gap: '1.5rem', alignItems: 'flex-start' }}>
          {/* Namespace list */}
          <div style={{ minWidth: '280px' }}>
            <h2 style={{ marginBottom: '0.75rem' }}>Namespaces</h2>
            {namespaces.length === 0 && (
              <div className="empty-state">No namespaces found.</div>
            )}
            {namespaces.map(ns => (
              <div
                key={ns.id}
                className={`list-item${selectedNS && selectedNS.id === ns.id ? ' active' : ''}`}
                style={{ cursor: 'pointer', padding: '0.5rem 0.75rem', borderRadius: '4px', marginBottom: '0.25rem', background: selectedNS && selectedNS.id === ns.id ? 'var(--accent-dim, #1e3a5f)' : 'transparent' }}
                onClick={() => selectNamespace(ns)}
              >
                <strong>{ns.name}</strong>
                <div style={{ fontSize: '0.8rem', color: 'var(--text-muted, #888)' }}>
                  {ns.backend_kind} · {ns.health}
                </div>
              </div>
            ))}
          </div>

          {/* Namespace detail panel */}
          {selectedNS && (
            <div style={{ flex: 1 }}>
              {nsLoading ? <Spinner loading={true} /> : (
                <>
                  <h2 style={{ marginBottom: '0.5rem' }}>{selectedNS.name}</h2>
                  <table className="detail-table" style={{ marginBottom: '1.25rem' }}>
                    <tbody>
                      <tr><td>ID</td><td>{selectedNS.id}</td></tr>
                      <tr><td>Backend</td><td>{selectedNS.backend_kind}</td></tr>
                      <tr><td>Kind</td><td>{selectedNS.namespace_kind}</td></tr>
                      <tr><td>Health</td><td>{selectedNS.health}</td></tr>
                      <tr><td>Placement</td><td>{selectedNS.placement_state}</td></tr>
                      <tr><td>Pin State</td><td>{selectedNS.pin_state}</td></tr>
                      <tr><td>Exposed Path</td><td>{selectedNS.exposed_path || '—'}</td></tr>
                      {selectedNS.snapshot_mode && (
                        <tr><td>Snapshot Mode</td><td>{selectedNS.snapshot_mode}</td></tr>
                      )}
                    </tbody>
                  </table>

                  {/* Snapshot section — only for managed ZFS namespaces */}
                  {isZFSManaged(selectedNS) && (
                    <>
                      {selectedNS.snapshot_mode === 'coordinated-namespace' ? (
                        <>
                          <div style={{ marginBottom: '1rem' }}>
                            <button
                              className="btn primary"
                              onClick={createSnapshot}
                              disabled={snapCreating}
                            >
                              {snapCreating ? 'Creating…' : 'Take Snapshot'}
                            </button>
                          </div>

                          <h3 style={{ marginBottom: '0.5rem' }}>Snapshots</h3>
                          <Spinner loading={snapsLoading} />
                          {!snapsLoading && snapshots.length === 0 && (
                            <div className="empty-state">No snapshots yet.</div>
                          )}
                          {!snapsLoading && snapshots.length > 0 && (
                            <>
                              {snapshots.length === 50 && (
                                <div className="info-note" style={{ marginBottom: '0.5rem' }}>
                                  Showing 50 most recent snapshots.
                                </div>
                              )}
                              <table className="data-table">
                                <thead>
                                  <tr>
                                    <th>Created</th>
                                    <th>Consistency</th>
                                    <th></th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {snapshots.map(snap => (
                                    <tr key={snap.snapshot_id}>
                                      <td>{formatTs(snap.created_at)}</td>
                                      <td>
                                        {snap.consistency === 'atomic' ? (
                                          <span style={{ color: '#4caf50', fontWeight: 600 }}>Atomic</span>
                                        ) : (
                                          <span
                                            style={{ color: '#f44336', fontWeight: 600 }}
                                            title="This snapshot may not be consistent across all datasets"
                                          >
                                            Inconsistent
                                          </span>
                                        )}
                                      </td>
                                      <td>
                                        <button
                                          className="btn danger small"
                                          onClick={() => confirmDeleteSnapshot(snap)}
                                        >
                                          Delete
                                        </button>
                                      </td>
                                    </tr>
                                  ))}
                                </tbody>
                              </table>
                            </>
                          )}
                        </>
                      ) : (
                        <div className="info-note" style={{ padding: '0.75rem', background: 'var(--info-bg, #1a2a3a)', borderRadius: '4px', borderLeft: '3px solid var(--info-border, #4a90d9)' }}>
                          Coordinated snapshots require all tier datasets to be in the same ZFS pool.
                          This namespace uses backing datasets across multiple pools and cannot be
                          snapshotted consistently.
                        </div>
                      )}
                    </>
                  )}
                </>
              )}
            </div>
          )}
        </div>
      )}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText="Delete"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current && confirmAction.current()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
