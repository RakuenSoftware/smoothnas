import { useState } from 'react';
import { Outlet, useNavigate } from 'react-router-dom';
import { AppShell, NavItem, ConfirmDialog, AlertsButton, UserDropdown, UserMenuEntry } from '@rakuensoftware/smoothgui';
import { useAuth } from '../../contexts/AuthContext';
import { api } from '../../api/api';

const NAV_ITEMS: NavItem[] = [
  { label: 'Dashboard', icon: '■', route: '/dashboard', section: 'Overview' },
  { label: 'Disks', icon: '⊙', route: '/disks', section: 'Hardware' },
  { label: 'SMART', icon: '⚠', route: '/smart', section: 'Hardware' },
  { label: 'Arrays', icon: '⊞', route: '/arrays', section: 'Storage' },
  { label: 'Tiers', icon: '★', route: '/tiers', section: 'Storage' },
  { label: 'Tiering', icon: '⇅', route: '/tiering', section: 'Storage' },

  { label: 'Sharing', icon: '⇤', route: '/sharing', section: 'Sharing' },
  { label: 'Backups', icon: '⬡', route: '/backups', section: 'Sharing' },
  { label: 'Benchmarks', icon: '⏱', route: '/benchmarks', section: 'System' },
  { label: 'Network', icon: '🌐', route: '/network', section: 'System' },
  { label: 'Users', icon: '👤', route: '/users', section: 'System' },
  { label: 'Terminal', icon: '⎊', route: '/terminal', section: 'System' },
  { label: 'Updates', icon: '⬆', route: '/updates', section: 'System' },
  { label: 'Settings', icon: '⚙', route: '/settings', section: 'System' },
];

function TopBar() {
  const { username, logout } = useAuth();
  const navigate = useNavigate();
  const [showRebootConfirm, setShowRebootConfirm] = useState(false);
  const [showShutdownConfirm, setShowShutdownConfirm] = useState(false);

  const userMenuItems: UserMenuEntry[] = [
    { label: 'Account Settings', onClick: () => navigate('/settings') },
    { divider: true },
    { label: 'Reboot', onClick: () => setShowRebootConfirm(true) },
    { label: 'Shutdown', onClick: () => setShowShutdownConfirm(true) },
    { divider: true },
    { label: 'Logout', onClick: logout, variant: 'danger' },
  ];

  return (
    <>
      <AlertsButton
        getAlertCount={() => api.getAlertCount()}
        getAlerts={() => api.getSystemAlerts()}
        clearAlert={(id) => api.clearAlert(id)}
      />

      <UserDropdown
        username={username || ''}
        menuItems={userMenuItems}
      />

      <ConfirmDialog
        visible={showRebootConfirm}
        title="Reboot System"
        message="Are you sure you want to reboot the system? All active connections will be interrupted."
        confirmText="Reboot"
        confirmClass="btn warning"
        onConfirm={() => { setShowRebootConfirm(false); api.reboot().catch(() => {}); }}
        onCancel={() => setShowRebootConfirm(false)}
      />
      <ConfirmDialog
        visible={showShutdownConfirm}
        title="Shut Down System"
        message="Are you sure you want to shut down the system? The server will be powered off and must be manually restarted."
        confirmText="Shut Down"
        confirmClass="btn danger"
        onConfirm={() => { setShowShutdownConfirm(false); api.shutdown().catch(() => {}); }}
        onCancel={() => setShowShutdownConfirm(false)}
      />
    </>
  );
}

export default function Layout() {
  return (
    <AppShell
      appName="SmoothNAS"
      appNameShort="SN"
      navItems={NAV_ITEMS}
      topBarContent={<TopBar />}
    >
      <Outlet />
    </AppShell>
  );
}
