import { useEffect, useState } from 'react';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function NfsExports() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [exports, setExports] = useState<any[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [newExport, setNewExport] = useState({ path: '', networks: '', sync: true, root_squash: true, read_only: false });
  const [paths, setPaths] = useState<any[]>([]);

  useEffect(() => { loadData(); loadPaths(); }, []);

  function loadData() {
    setLoading(true);
    api.getNfsExports().then(e => { setExports(e); setLoading(false); })
      .catch(e => { setError(extractError(e, 'Failed to load NFS exports')); setLoading(false); });
    api.getProtocols().then(protocols => {
      const p = protocols.find((x: any) => x.name === 'nfs');
      if (p) setEnabled(p.enabled);
    }).catch(() => {});
  }

  function loadPaths() {
    api.getFilesystemPaths().then(setPaths).catch(() => {});
  }

  function toggleProtocol() {
    setToggling(true);
    api.toggleProtocol('nfs', !enabled).then(() => {
      setEnabled(v => !v);
      setToggling(false);
    }).catch(e => { setError(extractError(e, 'Failed to toggle NFS')); setToggling(false); });
  }

  function create() {
    const data = { ...newExport, networks: newExport.networks.split(',').map((n: string) => n.trim()).filter(Boolean) };
    api.createNfsExport(data).then(() => {
      setShowCreate(false);
      setNewExport({ path: '', networks: '', sync: true, root_squash: true, read_only: false });
      loadData();
    }).catch(e => setError(extractError(e, 'Failed to create NFS export')));
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <span>NFS: <span className={`badge ${enabled ? 'active' : 'inactive'}`}>{enabled ? 'Enabled' : 'Disabled'}</span></span>
        <button className="btn secondary" onClick={toggleProtocol} disabled={toggling}>
          {enabled ? 'Disable' : 'Enable'}
        </button>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Add Export</button>
        <button className="refresh-btn" onClick={loadData}>Refresh</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Add NFS Export</h3>
          <div className="form-row">
            <label>Path
              <select value={newExport.path} onChange={e => setNewExport(p => ({ ...p, path: e.target.value }))}>
                <option value="">— select path —</option>
                {paths.map((p: any) => <option key={p.path} value={p.path}>{p.name} ({p.path})</option>)}
              </select>
            </label>
            <label>Allowed Networks (comma-separated)
              <input value={newExport.networks} onChange={e => setNewExport(p => ({ ...p, networks: e.target.value }))} placeholder="10.0.0.0/24, 192.168.1.0/24" />
            </label>
          </div>
          <div className="form-row">
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newExport.sync} onChange={e => setNewExport(p => ({ ...p, sync: e.target.checked }))} />
              Sync
            </label>
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newExport.root_squash} onChange={e => setNewExport(p => ({ ...p, root_squash: e.target.checked }))} />
              Root Squash
            </label>
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newExport.read_only} onChange={e => setNewExport(p => ({ ...p, read_only: e.target.checked }))} />
              Read Only
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn primary" onClick={create} disabled={!newExport.path}>Create</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        exports.length === 0 ? (
          <div className="empty-state"><p>No NFS exports configured.</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>Path</th><th>Networks</th><th>Sync</th><th>Root Squash</th></tr></thead>
            <tbody>
              {exports.map((e: any, i: number) => (
                <tr key={i}>
                  <td><code>{e.path}</code></td>
                  <td>{(e.networks || []).join(', ')}</td>
                  <td>{e.sync ? 'Yes' : 'No'}</td>
                  <td>{e.root_squash ? 'Yes' : 'No'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )
      )}
    </div>
  );
}
