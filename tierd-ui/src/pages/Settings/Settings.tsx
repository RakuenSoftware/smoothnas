import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function Settings() {
  const { t } = useI18n();
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
    api.setHostname(hostname).then(() => setSuccess(t('settings.toast.hostnameUpdated')))
      .catch(e => setError(extractError(e, t('settings.error.hostname'))));
  }

  function changePassword() {
    api.changePassword(pwChange.current_password, pwChange.new_password).then(() => {
      setPwChange({ current_password: '', new_password: '' });
      setSuccess(t('settings.toast.passwordChanged'));
    }).catch(e => setError(extractError(e, t('settings.error.password'))));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('settings.title')}</h1>
        <p className="subtitle">{t('settings.subtitle')}</p>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {error && <div className="error-msg">{error}</div>}
      {success && <div className="success">{success}</div>}

      <div className="section">
        <h2>{t('settings.section.hostname')}</h2>
        <Spinner loading={loadingHostname} />
        {!loadingHostname && (
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <input value={hostname} onChange={e => setHostname(e.target.value)} style={{ padding: '8px 10px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, width: 300 }} />
            <button className="btn primary" onClick={saveHostname}>{t('common.save')}</button>
          </div>
        )}
      </div>

      <div className="section">
        <h2>{t('settings.section.changePassword')}</h2>
        <div className="form-row">
          <label>{t('settings.field.currentPassword')} <input type="password" value={pwChange.current_password} onChange={e => setPwChange(p => ({ ...p, current_password: e.target.value }))} /></label>
          <label>{t('settings.field.newPassword')} <input type="password" value={pwChange.new_password} onChange={e => setPwChange(p => ({ ...p, new_password: e.target.value }))} /></label>
          <button className="btn primary" style={{ alignSelf: 'flex-end' }} onClick={changePassword}>{t('settings.button.change')}</button>
        </div>
      </div>
    </div>
  );
}
