import { useEffect, useState } from 'react';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function Settings() {
  const [loadingHostname, setLoadingHostname] = useState(true);
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');
  const [hostname, setHostname] = useState('');
  const [pwChange, setPwChange] = useState({ current_password: '', new_password: '' });

  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoadingHostname(true);
    let hostPending = 2;
    const hostDone = () => { if (--hostPending <= 0) setLoadingHostname(false); };
    api.getHostname().then((h: any) => { setHostname(h.hostname); hostDone(); }).catch(hostDone);
    api.getDns().then(hostDone).catch(hostDone);
  }

  function saveHostname() {
    api.setHostname(hostname).then(() => setSuccess('Hostname updated'))
      .catch(e => setError(extractError(e, 'Failed to update hostname')));
  }

  function changePassword() {
    api.changePassword(pwChange.current_password, pwChange.new_password).then(() => {
      setPwChange({ current_password: '', new_password: '' });
      setSuccess('Password changed');
    }).catch(e => setError(extractError(e, 'Failed to change password')));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Settings</h1>
        <p className="subtitle">System configuration</p>
        <button className="refresh-btn" onClick={loadData}>Refresh</button>
      </div>

      {error && <div className="error-msg">{error}</div>}
      {success && <div className="success">{success}</div>}

      <div className="section">
        <h2>Hostname</h2>
        <Spinner loading={loadingHostname} />
        {!loadingHostname && (
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <input value={hostname} onChange={e => setHostname(e.target.value)} style={{ padding: '8px 10px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, width: 300 }} />
            <button className="btn primary" onClick={saveHostname}>Save</button>
          </div>
        )}
      </div>

      <div className="section">
        <h2>Change Password</h2>
        <div className="form-row">
          <label>Current Password <input type="password" value={pwChange.current_password} onChange={e => setPwChange(p => ({ ...p, current_password: e.target.value }))} /></label>
          <label>New Password <input type="password" value={pwChange.new_password} onChange={e => setPwChange(p => ({ ...p, new_password: e.target.value }))} /></label>
          <button className="btn primary" style={{ alignSelf: 'flex-end' }} onClick={changePassword}>Change</button>
        </div>
      </div>
    </div>
  );
}
