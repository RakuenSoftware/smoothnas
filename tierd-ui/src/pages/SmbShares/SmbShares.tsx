import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function SmbShares() {
  const { t } = useI18n();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [shares, setShares] = useState<any[]>([]);
  const [smbConfig, setSmbConfig] = useState<any>(null);
  const [enabled, setEnabled] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [updatingConfig, setUpdatingConfig] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [newShare, setNewShare] = useState({ name: '', path: '', read_only: false, guest_ok: false });
  const [paths, setPaths] = useState<any[]>([]);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { loadData(); loadPaths(); }, []);

  function loadData() {
    setLoading(true);
    Promise.all([api.getSmbShares(), api.getSmbConfig()])
      .then(([s, cfg]) => { setShares(s); setSmbConfig(cfg); setLoading(false); })
      .catch(e => { setError(extractError(e, t('smb.error.load'))); setLoading(false); });
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
    }).catch(e => { setError(extractError(e, t('smb.error.toggle'))); setToggling(false); });
  }

  function setCompatibilityMode(enabled: boolean) {
    setUpdatingConfig(true);
    setError('');
    api.updateSmbConfig({ compatibility_mode: enabled })
      .then(cfg => {
        setSmbConfig(cfg);
        setUpdatingConfig(false);
      })
      .catch(e => {
        setError(extractError(e, t('smb.error.updateMode')));
        setUpdatingConfig(false);
      });
  }

  function create() {
    api.createSmbShare(newShare).then(() => {
      setShowCreate(false);
      setNewShare({ name: '', path: '', read_only: false, guest_ok: false });
      loadData();
    }).catch(e => setError(extractError(e, t('smb.error.create'))));
  }

  function deleteShare(name: string) {
    if (!confirm(t('smb.confirm.delete', { name }))) return;
    api.deleteSmbShare(name).then(loadData).catch(e => setError(extractError(e, t('smb.error.delete'))));
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <span>{t('smb.protocolLabel')} <span className={`badge ${enabled ? 'active' : 'inactive'}`}>{enabled ? t('iscsi.protocol.enabled') : t('iscsi.protocol.disabled')}</span></span>
        <button className="btn secondary" onClick={toggleProtocol} disabled={toggling}>
          {enabled ? t('iscsi.button.disable') : t('iscsi.button.enable')}
        </button>
        {smbConfig && (
          <label
            title={t('smb.compatibilityMode.tooltip')}
            style={{ display: 'flex', alignItems: 'center', gap: 6, marginLeft: 8 }}
          >
            <input
              type="checkbox"
              checked={!!smbConfig.compatibility_mode}
              disabled={updatingConfig}
              onChange={e => setCompatibilityMode(e.target.checked)}
            />
            {t('smb.compatibilityMode.label')}
          </label>
        )}
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('smb.button.addShare')}</button>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('smb.create.title')}</h3>
          <div className="form-row">
            <label>{t('smb.field.shareName')} <input value={newShare.name} onChange={e => setNewShare(p => ({ ...p, name: e.target.value }))} /></label>
            <label>{t('smb.field.path')}
              <select value={newShare.path} onChange={e => setNewShare(p => ({ ...p, path: e.target.value }))}>
                <option value="">{t('smb.field.pathPlaceholder')}</option>
                {paths.map((p: any) => <option key={p.path} value={p.path}>{p.name} ({p.path})</option>)}
              </select>
            </label>
          </div>
          <div className="form-row">
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newShare.read_only} onChange={e => setNewShare(p => ({ ...p, read_only: e.target.checked }))} />
              {t('smb.field.readOnly')}
            </label>
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newShare.guest_ok} onChange={e => setNewShare(p => ({ ...p, guest_ok: e.target.checked }))} />
              {t('smb.field.guestOk')}
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create} disabled={!newShare.name.trim() || !newShare.path}>{t('arrays.button.create')}</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        shares.length === 0 ? (
          <div className="empty-state"><p>{t('smb.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>{t('datasets.col.name')}</th><th>{t('iscsi.col.path')}</th><th>{t('smb.col.readOnly')}</th><th>{t('smb.col.guestOk')}</th><th>{t('arrays.col.actions')}</th></tr></thead>
            <tbody>
              {shares.map((s: any) => (
                <tr key={s.name}>
                  <td>{s.name}</td>
                  <td><code>{s.path}</code></td>
                  <td>{s.read_only ? t('common.yes') : t('common.no')}</td>
                  <td>{s.guest_ok ? t('common.yes') : t('common.no')}</td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteShare(s.name)}>{t('common.delete')}</button>
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
