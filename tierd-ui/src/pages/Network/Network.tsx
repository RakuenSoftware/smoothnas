import { useEffect, useState } from 'react';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function Network() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [interfaces, setInterfaces] = useState<any[]>([]);
  const [bonds, setBonds] = useState<any[]>([]);
  const [vlans, setVlans] = useState<any[]>([]);
  const [hostname, setHostname] = useState('');
  const [pending, setPending] = useState<any>(null);

  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoading(true);
    let count = 4;
    const done = () => { if (--count <= 0) setLoading(false); };
    api.getInterfaces().then(i => { setInterfaces(i); done(); }).catch(done);
    api.getBonds().then(b => { setBonds(b); done(); }).catch(done);
    api.getVlans().then(v => { setVlans(v); done(); }).catch(done);
    api.getHostname().then((h: any) => { setHostname(h.hostname); done(); }).catch(done);
    api.getPendingChange().then(setPending).catch(() => {});
  }

  function confirm() {
    api.confirmChange().then(() => { setPending(null); loadData(); })
      .catch(e => setError(extractError(e, 'Failed to confirm network change')));
  }

  function revert() {
    api.revertChange().then(() => { setPending(null); loadData(); })
      .catch(e => setError(extractError(e, 'Failed to revert network change')));
  }

  function identify(name: string) {
    api.identifyInterface(name).catch(e => setError(extractError(e, 'Identify failed')));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Network</h1>
        <p className="subtitle">Network interface and routing configuration</p>
        <button className="refresh-btn" onClick={loadData}>Refresh</button>
      </div>

      {error && <div className="error-msg">{error}</div>}

      {pending && (
        <div className="safe-apply-banner">
          <span>⚠ Pending network change — changes will auto-revert in {pending.revert_in || '?'}s if not confirmed.</span>
          <button className="btn primary" onClick={confirm}>Confirm</button>
          <button className="btn danger" onClick={revert}>Revert</button>
        </div>
      )}

      {hostname && (
        <div className="section">
          <h2>Hostname</h2>
          <p><code>{hostname}</code></p>
        </div>
      )}

      <Spinner loading={loading} text="Loading interfaces..." />

      {!loading && (
        <>
          <div className="section">
            <h2>Interfaces</h2>
            {interfaces.length === 0 ? <p>No interfaces found.</p> : (
              <table className="data-table">
                <thead>
                  <tr><th>Interface</th><th>State</th><th>Speed</th><th>IP Address</th><th>MAC</th><th>Actions</th></tr>
                </thead>
                <tbody>
                  {interfaces.map((iface: any) => (
                    <tr key={iface.name}>
                      <td><strong>{iface.name}</strong></td>
                      <td><span className={`badge ${iface.link === 'up' ? 'online' : 'inactive'}`}>{iface.link || 'unknown'}</span></td>
                      <td>{iface.speed_mbps ? `${iface.speed_mbps} Mb/s` : '—'}</td>
                      <td>{(iface.addresses || []).join(', ') || '—'}</td>
                      <td><code>{iface.mac || '—'}</code></td>
                      <td className="action-cell">
                        <button className="btn secondary" onClick={() => identify(iface.name)}>Identify</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>

          {bonds.length > 0 && (
            <div className="section">
              <h2>Bonds</h2>
              <table className="data-table">
                <thead><tr><th>Name</th><th>Mode</th><th>Members</th></tr></thead>
                <tbody>
                  {bonds.map((b: any) => (
                    <tr key={b.name}>
                      <td><code>{b.name}</code></td>
                      <td>{b.mode}</td>
                      <td>{(b.members || []).join(', ')}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {vlans.length > 0 && (
            <div className="section">
              <h2>VLANs</h2>
              <table className="data-table">
                <thead><tr><th>Name</th><th>Parent</th><th>VLAN ID</th></tr></thead>
                <tbody>
                  {vlans.map((v: any) => (
                    <tr key={v.name}>
                      <td><code>{v.name}</code></td>
                      <td>{v.parent}</td>
                      <td>{v.vlan_id}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
}
