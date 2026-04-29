import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import { useToast } from '../../contexts/ToastContext';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Users() {
  const { t } = useI18n();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [users, setUsers] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [newUser, setNewUser] = useState({ username: '', password: '', confirmPassword: '' });
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoading(true);
    api.getUsers().then(u => { setUsers(u); setLoading(false); })
      .catch(e => { toast.error(extractError(e, t('users.error.load'))); setLoading(false); });
  }

  function createUser() {
    const { username, password, confirmPassword } = newUser;
    if (!username.trim()) { toast.warning(t('users.validate.usernameRequired')); return; }
    if (password.length < 8) { toast.warning(t('users.validate.passwordLen')); return; }
    if (password !== confirmPassword) { toast.warning(t('users.validate.passwordMismatch')); return; }
    api.createUser(username.trim(), password).then(() => {
      setShowCreate(false);
      setNewUser({ username: '', password: '', confirmPassword: '' });
      toast.success(t('users.toast.created'));
      loadData();
    }).catch(e => toast.error(extractError(e, t('users.error.create'))));
  }

  function deleteUser(username: string) {
    setConfirmTitle(t('users.confirm.deleteTitle'));
    setConfirmMessage(t('users.confirm.deleteMessage', { username }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteUser(username).then(() => {
        toast.success(t('users.toast.deleted'));
        loadData();
      }).catch(e => toast.error(extractError(e, t('users.error.delete'))));
    };
    setConfirmVisible(true);
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('users.title')}</h1>
        <p className="subtitle">{t('users.subtitle')}</p>
        <div className="header-actions">
          <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('users.button.create')}</button>
          <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
        </div>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('users.button.create')}</h3>
          <div className="form-row">
            <label>{t('users.field.username')} <input value={newUser.username} onChange={e => setNewUser(p => ({ ...p, username: e.target.value }))} /></label>
            <label>{t('users.field.password')} <input type="password" value={newUser.password} onChange={e => setNewUser(p => ({ ...p, password: e.target.value }))} /></label>
            <label>{t('users.field.confirmPassword')} <input type="password" value={newUser.confirmPassword} onChange={e => setNewUser(p => ({ ...p, confirmPassword: e.target.value }))} /></label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={createUser}>{t('arrays.button.create')}</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        users.length === 0 ? (
          <div className="empty-state"><p>{t('users.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>{t('users.col.username')}</th><th>{t('users.col.uid')}</th><th>{t('users.col.home')}</th><th>{t('arrays.col.actions')}</th></tr></thead>
            <tbody>
              {users.map((u: any) => (
                <tr key={u.username}>
                  <td><strong>{u.username}</strong></td>
                  <td>{u.uid || '—'}</td>
                  <td><code>{u.home || '—'}</code></td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteUser(u.username)}>{t('common.delete')}</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )
      )}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText={t('common.delete')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
