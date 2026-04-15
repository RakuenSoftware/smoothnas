import { useEffect, useState } from 'react';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function SmbShares() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [shares, setShares] = useState<any[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [newShare, setNewShare] = useState({ name: '', path: '', read_only: false, guest_ok: false });
  const [paths, setPaths] = useState<any[]>([]);

  useEffect(() => { loadData(); loadPaths(); }, []);

  function loadData() {
    setLoading(true);
    api.getSmbShares().then(s => { setShares(s); setLoading(false); })
      .catch(e => { setError(extractError(e, 'Failed to load SMB shares')); setLoading(false); });
    api.getProtocols().then(protocols => {
      const p = protocols.find((x: any) => x.name === 'smb');
      if (p) setEnabled(p.enabled);
    }).catch(() => {});
  }

  function loadPaths() {
    api.getFilesystemPaths().then(setPaths).catch(() => {});
  }

  function toggleProtocol() {
    setToggling(true);
    api.toggleProtocol('smb', !enabled).then(() => {
      setEnabled(v => !v);
      setToggling(false);
    }).catch(e => { setError(extractError(e, 'Failed to toggle SMB')); setToggling(false); });
  }

  function create() {
    api.createSmbShare(newShare).then(() => {
      setShowCreate(false);
      setNewShare({ name: '', path: '', read_only: false, guest_ok: false });
      loadData();
    }).catch(e => setError(extractError(e, 'Failed to create SMB share')));
  }

  function deleteShare(name: string) {
    if (!confirm('Delete share ' + name + '?')) return;
    api.deleteSmbShare(name).then(loadData).catch(e => setError(extractError(e, 'Failed to delete SMB share')));
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <span>Samba: <span className={`badge ${enabled ? 'active' : 'inactive'}`}>{enabled ? 'Enabled' : 'Disabled'}</span></span>
        <button className="btn secondary" onClick={toggleProtocol} disabled={toggling}>
          {enabled ? 'Disable' : 'Enable'}
        </button>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Add Share</button>
        <button className="refresh-btn" onClick={loadData}>Refresh</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Add SMB Share</h3>
          <div className="form-row">
            <label>Share Name <input value={newShare.name} onChange={e => setNewShare(p => ({ ...p, name: e.target.value }))} /></label>
            <label>Path
              <select value={newShare.path} onChange={e => setNewShare(p => ({ ...p, path: e.target.value }))}>
                <option value="">— select path —</option>
                {paths.map((p: any) => <option key={p.path} value={p.path}>{p.name} ({p.path})</option>)}
              </select>
            </label>
          </div>
          <div className="form-row">
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newShare.read_only} onChange={e => setNewShare(p => ({ ...p, read_only: e.target.checked }))} />
              Read Only
            </label>
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newShare.guest_ok} onChange={e => setNewShare(p => ({ ...p, guest_ok: e.target.checked }))} />
              Guest OK
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn primary" onClick={create} disabled={!newShare.name.trim() || !newShare.path}>Create</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        shares.length === 0 ? (
          <div className="empty-state"><p>No SMB shares configured.</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>Name</th><th>Path</th><th>Read Only</th><th>Guest OK</th><th>Actions</th></tr></thead>
            <tbody>
              {shares.map((s: any) => (
                <tr key={s.name}>
                  <td>{s.name}</td>
                  <td><code>{s.path}</code></td>
                  <td>{s.read_only ? 'Yes' : 'No'}</td>
                  <td>{s.guest_ok ? 'Yes' : 'No'}</td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteShare(s.name)}>Delete</button>
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
