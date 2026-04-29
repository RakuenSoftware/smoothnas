import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function NfsExports() {
  const { t } = useI18n();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [exports, setExports] = useState<any[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [updatingExport, setUpdatingExport] = useState<number | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [newExport, setNewExport] = useState({ path: '', networks: '', sync: false, root_squash: true, read_only: false });
  const [paths, setPaths] = useState<any[]>([]);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { loadData(); loadPaths(); }, []);

  function loadData() {
    setLoading(true);
    api.getNfsExports().then(e => { setExports(e); setLoading(false); })
      .catch(e => { setError(extractError(e, t('nfs.error.load'))); setLoading(false); });
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
    }).catch(e => { setError(extractError(e, t('nfs.error.toggle'))); setToggling(false); });
  }

  function create() {
    const data = { ...newExport, networks: newExport.networks.split(',').map((n: string) => n.trim()).filter(Boolean) };
    api.createNfsExport(data).then(() => {
      setShowCreate(false);
      setNewExport({ path: '', networks: '', sync: false, root_squash: true, read_only: false });
      loadData();
    }).catch(e => setError(extractError(e, t('nfs.error.create'))));
  }

  function setExportSync(id: number, sync: boolean) {
    setUpdatingExport(id);
    api.updateNfsExport(id, { sync }).then(loadData)
      .catch(e => setError(extractError(e, t('nfs.error.update'))))
      .finally(() => setUpdatingExport(null));
  }

  function formatNetworks(networks: unknown) {
    if (Array.isArray(networks)) return networks.join(', ');
    if (typeof networks === 'string') return networks;
    return '';
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <span>{t('nfs.protocolLabel')} <span className={`badge ${enabled ? 'active' : 'inactive'}`}>{enabled ? t('iscsi.protocol.enabled') : t('iscsi.protocol.disabled')}</span></span>
        <button className="btn secondary" onClick={toggleProtocol} disabled={toggling}>
          {enabled ? t('iscsi.button.disable') : t('iscsi.button.enable')}
        </button>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('nfs.button.addExport')}</button>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('nfs.create.title')}</h3>
          <div className="form-row">
            <label>{t('smb.field.path')}
              <select value={newExport.path} onChange={e => setNewExport(p => ({ ...p, path: e.target.value }))}>
                <option value="">{t('smb.field.pathPlaceholder')}</option>
                {paths.map((p: any) => <option key={p.path} value={p.path}>{p.name} ({p.path})</option>)}
              </select>
            </label>
            <label>{t('nfs.field.allowedNetworks')}
              <input value={newExport.networks} onChange={e => setNewExport(p => ({ ...p, networks: e.target.value }))} placeholder="10.0.0.0/24, 192.168.1.0/24" />
            </label>
          </div>
          <div className="form-row">
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newExport.sync} onChange={e => setNewExport(p => ({ ...p, sync: e.target.checked }))} />
              {t('nfs.field.syncWrites')}
            </label>
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newExport.root_squash} onChange={e => setNewExport(p => ({ ...p, root_squash: e.target.checked }))} />
              {t('nfs.field.rootSquash')}
            </label>
            <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={newExport.read_only} onChange={e => setNewExport(p => ({ ...p, read_only: e.target.checked }))} />
              {t('smb.field.readOnly')}
            </label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create} disabled={!newExport.path}>{t('arrays.button.create')}</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        exports.length === 0 ? (
          <div className="empty-state"><p>{t('nfs.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>{t('iscsi.col.path')}</th><th>{t('nfs.col.networks')}</th><th>{t('nfs.col.writeMode')}</th><th>{t('nfs.col.rootSquash')}</th></tr></thead>
            <tbody>
              {exports.map((e: any, i: number) => (
                <tr key={i}>
                  <td><code>{e.path}</code></td>
                  <td>{formatNetworks(e.networks)}</td>
                  <td>
                    <label style={{ flexDirection: 'row', alignItems: 'center', gap: 8, margin: 0 }}>
                      <input
                        type="checkbox"
                        checked={!!e.sync}
                        disabled={updatingExport === e.id}
                        onChange={ev => setExportSync(e.id, ev.target.checked)}
                      />
                      {e.sync ? t('nfs.writeMode.sync') : t('nfs.writeMode.async')}
                    </label>
                  </td>
                  <td>{e.root_squash ? t('common.yes') : t('common.no')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )
      )}
    </div>
  );
}
