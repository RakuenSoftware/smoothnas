import { useEffect, useRef, useState } from 'react';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import { useToast } from '../../contexts/ToastContext';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Users() {
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [users, setUsers] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [newUser, setNewUser] = useState({ username: '', password: '', confirmPassword: '' });
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);

  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoading(true);
    api.getUsers().then(u => { setUsers(u); setLoading(false); })
      .catch(e => { toast.error(extractError(e, 'Failed to load users')); setLoading(false); });
  }

  function createUser() {
    const { username, password, confirmPassword } = newUser;
    if (!username.trim()) { toast.warning('Username is required'); return; }
    if (password.length < 8) { toast.warning('Password must be at least 8 characters'); return; }
    if (password !== confirmPassword) { toast.warning('Passwords do not match'); return; }
    api.createUser(username.trim(), password).then(() => {
      setShowCreate(false);
      setNewUser({ username: '', password: '', confirmPassword: '' });
      toast.success('User created');
      loadData();
    }).catch(e => toast.error(extractError(e, 'Failed to create user')));
  }

  function deleteUser(username: string) {
    setConfirmTitle('Delete User');
    setConfirmMessage(`This will permanently delete the user "${username}" and remove their home directory. This cannot be undone.`);
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteUser(username).then(() => {
        toast.success('User deleted');
        loadData();
      }).catch(e => toast.error(extractError(e, 'Failed to delete user')));
    };
    setConfirmVisible(true);
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Users</h1>
        <p className="subtitle">Local user account management</p>
        <div className="header-actions">
          <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>Create User</button>
          <button className="refresh-btn" onClick={loadData}>Refresh</button>
        </div>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>Create User</h3>
          <div className="form-row">
            <label>Username <input value={newUser.username} onChange={e => setNewUser(p => ({ ...p, username: e.target.value }))} /></label>
            <label>Password <input type="password" value={newUser.password} onChange={e => setNewUser(p => ({ ...p, password: e.target.value }))} /></label>
            <label>Confirm Password <input type="password" value={newUser.confirmPassword} onChange={e => setNewUser(p => ({ ...p, confirmPassword: e.target.value }))} /></label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn primary" onClick={createUser}>Create</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        users.length === 0 ? (
          <div className="empty-state"><p>No users found.</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>Username</th><th>UID</th><th>Home</th><th>Actions</th></tr></thead>
            <tbody>
              {users.map((u: any) => (
                <tr key={u.username}>
                  <td><strong>{u.username}</strong></td>
                  <td>{u.uid || '—'}</td>
                  <td><code>{u.home || '—'}</code></td>
                  <td className="action-cell">
                    <button className="btn danger" onClick={() => deleteUser(u.username)}>Delete</button>
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
        confirmText="Delete"
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
