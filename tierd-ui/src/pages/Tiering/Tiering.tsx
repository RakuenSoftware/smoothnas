import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
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
  const { t } = useI18n();
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

  // eslint-disable-next-line react-hooks/exhaustive-deps
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
        const msg = extractError(e, t('tiering.error.loadNamespaces'));
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
        toast.error(extractError(e, t('tiering.error.loadNamespace')));
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
        toast.error(extractError(e, t('tiering.error.loadSnapshots')));
        setSnapsLoading(false);
      });
  }

  function createSnapshot() {
    if (!selectedNS) return;
    setSnapCreating(true);
    api.createTieringNamespaceSnapshot(selectedNS.id)
      .then(() => {
        toast.success(t('tiering.toast.snapshotCreated'));
        loadSnapshots(selectedNS.id);
        setSnapCreating(false);
      })
      .catch(e => {
        toast.error(extractError(e, t('tiering.error.createSnapshot')));
        setSnapCreating(false);
      });
  }

  function confirmDeleteSnapshot(snap: any) {
    setConfirmTitle(t('tiering.confirm.deleteTitle'));
    setConfirmMessage(t('tiering.confirm.deleteMessage', { id: snap.snapshot_id }));
    confirmAction.current = () => deleteSnapshot(snap.snapshot_id);
    setConfirmVisible(true);
  }

  function deleteSnapshot(snapID: string) {
    setConfirmVisible(false);
    api.deleteTieringNamespaceSnapshot(selectedNS.id, snapID)
      .then(() => {
        toast.success(t('tiering.toast.snapshotDeleted'));
        setSnapshots(prev => prev.filter(s => s.snapshot_id !== snapID));
      })
      .catch(e => {
        toast.error(extractError(e, t('tiering.error.deleteSnapshot')));
      });
  }

  function isZFSManaged(ns: any) {
    return ns && ns.backend_kind === 'zfs-managed';
  }

  return (
    <div className="page-content">
      <div className="page-header">
        <h1>{t('tiering.title')}</h1>
        <button className="btn secondary" onClick={loadNamespaces}>{t('common.refresh')}</button>
      </div>

      <Spinner loading={loading} />
      {error && <div className="error-message">{error}</div>}

      {!loading && !error && (
        <div style={{ display: 'flex', gap: '1.5rem', alignItems: 'flex-start' }}>
          {/* Namespace list */}
          <div style={{ minWidth: '280px' }}>
            <h2 style={{ marginBottom: '0.75rem' }}>{t('tiering.section.namespaces')}</h2>
            {namespaces.length === 0 && (
              <div className="empty-state">{t('tiering.empty.namespaces')}</div>
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
                      <tr><td>{t('tiering.detail.id')}</td><td>{selectedNS.id}</td></tr>
                      <tr><td>{t('volumes.col.backend')}</td><td>{selectedNS.backend_kind}</td></tr>
                      <tr><td>{t('tiering.detail.kind')}</td><td>{selectedNS.namespace_kind}</td></tr>
                      <tr><td>{t('volumes.col.health')}</td><td>{selectedNS.health}</td></tr>
                      <tr><td>{t('volumes.detail.placement')}</td><td>{selectedNS.placement_state}</td></tr>
                      <tr><td>{t('tiering.detail.pinState')}</td><td>{selectedNS.pin_state}</td></tr>
                      <tr><td>{t('tiering.detail.exposedPath')}</td><td>{selectedNS.exposed_path || '—'}</td></tr>
                      {selectedNS.snapshot_mode && (
                        <tr><td>{t('volumes.detail.snapshotMode')}</td><td>{selectedNS.snapshot_mode}</td></tr>
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
                              {snapCreating ? t('tiers.button.creating') : t('tiering.button.takeSnapshot')}
                            </button>
                          </div>

                          <h3 style={{ marginBottom: '0.5rem' }}>{t('pools.tab.snapshots')}</h3>
                          <Spinner loading={snapsLoading} />
                          {!snapsLoading && snapshots.length === 0 && (
                            <div className="empty-state">{t('tiering.empty.snapshots')}</div>
                          )}
                          {!snapsLoading && snapshots.length > 0 && (
                            <>
                              {snapshots.length === 50 && (
                                <div className="info-note" style={{ marginBottom: '0.5rem' }}>
                                  {t('tiering.snapshot.showing50')}
                                </div>
                              )}
                              <table className="data-table">
                                <thead>
                                  <tr>
                                    <th>{t('snapshots.col.created')}</th>
                                    <th>{t('tiering.col.consistency')}</th>
                                    <th></th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {snapshots.map(snap => (
                                    <tr key={snap.snapshot_id}>
                                      <td>{formatTs(snap.created_at)}</td>
                                      <td>
                                        {snap.consistency === 'atomic' ? (
                                          <span style={{ color: '#4caf50', fontWeight: 600 }}>{t('tiering.consistency.atomic')}</span>
                                        ) : (
                                          <span
                                            style={{ color: '#f44336', fontWeight: 600 }}
                                            title={t('tiering.consistency.inconsistentTooltip')}
                                          >
                                            {t('tiering.consistency.inconsistent')}
                                          </span>
                                        )}
                                      </td>
                                      <td>
                                        <button
                                          className="btn danger small"
                                          onClick={() => confirmDeleteSnapshot(snap)}
                                        >
                                          {t('common.delete')}
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
                          {t('tiering.snapshot.crossPoolNote')}
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
        confirmText={t('common.delete')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current && confirmAction.current()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
