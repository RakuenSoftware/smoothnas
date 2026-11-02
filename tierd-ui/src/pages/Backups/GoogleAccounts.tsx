import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export interface GoogleAccount { id: number; label: string; email: string; jobs: number }
export default function GoogleAccounts({ accounts, refresh }: { accounts: GoogleAccount[]; refresh: () => void }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [credentials, setCredentials] = useState({ label: '', client_id: '', client_secret: '' });
  const [session, setSession] = useState<{ session_id: string; user_code: string; verification_url: string } | null>(null);
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [disconnect, setDisconnect] = useState<GoogleAccount | null>(null);
  useEffect(() => {
    if (!session) return;
    const interval = setInterval(async () => {
      try {
        const result = await api.getGoogleBackupSession(session.session_id);
        setStatus(result.status);
        if (result.status === 'connected') { setSession(null); setOpen(false); refresh(); }
        else if (result.status !== 'pending') { setSession(null); setError(t('backups.google.signInFailed')); }
      } catch (e: any) { setSession(null); setError(e.message); }
    }, 2000);
    return () => clearInterval(interval);
  }, [session, refresh, t]);
  async function connect() {
    setBusy(true); setError('');
    try { setSession(await api.startGoogleBackupSession(credentials)); setCredentials({ label: '', client_id: '', client_secret: '' }); setStatus('pending'); }
    catch (e: any) { setError(e.message); }
    finally { setBusy(false); }
  }
  async function cancel() {
    if (session) { try { await api.cancelGoogleBackupSession(session.session_id); } catch (e: any) { setError(e.message); return; } }
    setSession(null); setOpen(false); setCredentials({ label: '', client_id: '', client_secret: '' });
  }
  return <div className="card" style={{ marginBottom: 16 }}>
    <h3>{t('backups.google.accounts')}</h3>
    <p>{t('backups.google.setup')} <a href="https://console.cloud.google.com/apis/credentials" target="_blank" rel="noreferrer">{t('backups.google.console')}</a></p>
    {accounts.map(a => <div key={a.id} style={{ display: 'flex', gap: 12, alignItems: 'center', marginBottom: 8 }}>
      <span>{a.label} {a.email && `(${a.email})`} · {t('backups.google.jobs', { count: a.jobs })}</span>
      <button className="btn danger" disabled={a.jobs > 0} onClick={() => setDisconnect(a)}>{t('backups.google.disconnect')}</button>
    </div>)}
    {!open && <button className="btn secondary" onClick={() => { setOpen(true); setError(''); }}>{t('backups.google.connect')}</button>}
    {open && <>
      <p>{t('backups.google.production')}</p><p>{t('backups.google.sameProject')}</p>
      {!session ? <div className="form-row">
        <label>{t('backups.google.label')}<input value={credentials.label} onChange={e => setCredentials(c => ({ ...c, label: e.target.value }))} /></label>
        <label>{t('backups.google.clientId')}<input value={credentials.client_id} onChange={e => setCredentials(c => ({ ...c, client_id: e.target.value }))} /></label>
        <label>{t('backups.google.clientSecret')}<input type="password" autoComplete="new-password" value={credentials.client_secret} onChange={e => setCredentials(c => ({ ...c, client_secret: e.target.value }))} /></label>
        <button className="btn primary" onClick={connect} disabled={busy}>{t('backups.google.connect')}</button>
      </div> : <p><a href={session.verification_url} target="_blank" rel="noreferrer">{t('backups.google.approve')}</a> <strong>{session.user_code}</strong> · {t(`backups.google.status.${status}`)}</p>}
      <button className="btn secondary" onClick={cancel}>{t('common.cancel')}</button>
    </>}
    {error && <div className="error-msg">{error}</div>}
    <ConfirmDialog visible={!!disconnect} title={t('backups.google.disconnect')} message={t('backups.google.disconnectConfirm')} onCancel={() => setDisconnect(null)} onConfirm={async () => {
      if (!disconnect) return;
      try { await api.deleteGoogleBackupAccount(disconnect.id); setDisconnect(null); refresh(); } catch (e: any) { setError(e.message); setDisconnect(null); }
    }} />
  </div>;
}
