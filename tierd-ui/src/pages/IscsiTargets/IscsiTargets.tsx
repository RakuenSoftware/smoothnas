import { useEffect, useState } from 'react';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function IscsiTargets() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [targets, setTargets] = useState<any[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [newTarget, setNewTarget] = useState({ iqn: '', block_device: '', chap_user: '', chap_pass: '' });

  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoading(true);
    api.getIscsiTargets().then(t => { setTargets(t); setLoading(false); })
      .catch(e => { setError(extractError(e, 'Failed to load iSCSI targets')); setLoading(false); });
    api.getProtocols().then(protocols => {
      const p = protocols.find((x: any) => x.name === 'iscsi');
      if (p) setEnabled(p.enabled);
    }).catch(() => {});
  }

  function toggleProtocol() {
    setToggling(true);
    api.toggleProtocol('iscsi', !enabled).then(() => {
      setEnabled(v => !v);
      setToggling(false);
    }).catch(e => { setError(extractError(e, 'Failed to toggle iSCSI')); setToggling(false); });
  }

  function create() {
    api.createIscsiTarget(newTarget).then(() => {
      setShowCreate(false);
      setNewTarget({ iqn: '', block_device: '', chap_user: '', chap_pass: '' });
      loadData();
    }).catch(e => setError(extractError(e, 'Failed to create iSCSI target')));
  }

  function deleteTarget(iqn: string) {
    if (!confirm('Destroy target?')) return;
    api.deleteIscsiTarget(iqn).then(loadData)
      .catch(e => setError(extractError(e, 'Failed to destroy iSCSI target')));
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <span>iSCSI: <span className={`badge ${enabled ? 'active' : 'inactive'}`}>{enabled ? 'Enabled' : 'Disabled'}</span></span>
        <button className="btn secondary" onClick={toggleProtocol} disabled={toggling}>
          {enabled ? 'Disable' : 'Enable'}
        </button>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Add Target</button>
        <button className="refresh-btn" onClick={loadData}>Refresh</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Add iSCSI Target</h3>
          <div className="form-row">
            <label>IQN <input value={newTarget.iqn} onChange={e => setNewTarget(p => ({ ...p, iqn: e.target.value }))} placeholder="iqn.2024-01.com.example:storage" /></label>
            <label>Block Device <input value={newTarget.block_device} onChange={e => setNewTarget(p => ({ ...p, block_device: e.target.value }))} placeholder="/dev/zvol/tank/vol0" /></label>
          </div>
          <div className="form-row">
            <label>CHAP User (optional) <input value={newTarget.chap_user} onChange={e => setNewTarget(p => ({ ...p, chap_user: e.target.value }))} /></label>
            <label>CHAP Password <input type="password" value={newTarget.chap_pass} onChange={e => setNewTarget(p => ({ ...p, chap_pass: e.target.value }))} /></label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn primary" onClick={create} disabled={!newTarget.iqn.trim() || !newTarget.block_device.trim()}>Create</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        targets.length === 0 ? (
          <div className="empty-state"><p>No iSCSI targets configured.</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>IQN</th><th>Block Device</th><th>Actions</th></tr></thead>
            <tbody>
              {targets.map((t: any) => (
                <tr key={t.iqn}>
                  <td><code>{t.iqn}</code></td>
                  <td><code>{t.block_device}</code></td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteTarget(t.iqn)}>Destroy</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )
      )}
    </div>
  );
}
