import { useState } from 'react';
import { Outlet, useNavigate } from 'react-router-dom';
import {
  AppShell,
  NavItem,
  ConfirmDialog,
  AlertsButton,
  UserDropdown,
  UserMenuEntry,
  useI18n,
} from '@rakuensoftware/smoothgui';
import { useAuth } from '../../contexts/AuthContext';
import { api } from '../../api/api';
import LanguagePicker from '../LanguagePicker/LanguagePicker';

// Phase 2 of the SmoothNAS i18n proposal: chrome conversion.
// Sidebar nav labels + sections, top-bar user menu, and the reboot
// / shutdown confirm dialogs go through t(). The header gets a
// LanguagePicker that's hidden until a second language ships.

// buildNavItems renders the nav list using the active locale. Kept
// out of module scope so each render picks up the latest t() — the
// AppShell's NavItem labels are strings, so we recompute on every
// render rather than memoising. The list is small (≤ 16 entries);
// recomputation is free.
function buildNavItems(t: ReturnType<typeof useI18n>['t']): NavItem[] {
  const sec = {
    overview: t('nav.section.overview', undefined, 'Overview'),
    hardware: t('nav.section.hardware', undefined, 'Hardware'),
    storage: t('nav.section.storage', undefined, 'Storage'),
    sharing: t('nav.section.sharing', undefined, 'Sharing'),
    system: t('nav.section.system', undefined, 'System'),
  };
  return [
    { label: t('nav.dashboard'),       icon: '■',  route: '/dashboard',      section: sec.overview },
    { label: t('nav.disks'),           icon: '⊙',  route: '/disks',          section: sec.hardware },
    { label: t('nav.smart'),           icon: '⚠',  route: '/smart',          section: sec.hardware },
    { label: t('nav.arrays'),          icon: '⊞',  route: '/arrays',         section: sec.storage },
    { label: t('nav.tiers'),           icon: '★',  route: '/tiers',          section: sec.storage },
    { label: t('nav.tiering'),         icon: '⇅',  route: '/tiering',        section: sec.storage },
    { label: t('nav.volumes'),         icon: '▤',  route: '/volumes',        section: sec.storage },
    { label: t('nav.smoothfsPools'),   icon: '▣',  route: '/smoothfs-pools', section: sec.storage },

    { label: t('nav.sharing'),         icon: '⇤',  route: '/sharing',        section: sec.sharing },
    { label: t('nav.backups'),         icon: '⬡',  route: '/backups',        section: sec.sharing },
    { label: t('nav.benchmarks'),      icon: '⏱',  route: '/benchmarks',     section: sec.system },
    { label: t('nav.network'),         icon: '🌐', route: '/network',        section: sec.system },
    { label: t('nav.users'),           icon: '👤', route: '/users',          section: sec.system },
    { label: t('nav.terminal'),        icon: '⎊',  route: '/terminal',       section: sec.system },
    { label: t('nav.updates'),         icon: '⬆',  route: '/updates',        section: sec.system },
    { label: t('nav.settings'),        icon: '⚙',  route: '/settings',       section: sec.system },
  ];
}

function TopBar() {
  const { username, logout } = useAuth();
  const { t } = useI18n();
  const navigate = useNavigate();
  const [showRebootConfirm, setShowRebootConfirm] = useState(false);
  const [showShutdownConfirm, setShowShutdownConfirm] = useState(false);

  const userMenuItems: UserMenuEntry[] = [
    { label: t('topbar.user.accountSettings'), onClick: () => navigate('/settings') },
    { divider: true },
    { label: t('topbar.user.reboot'), onClick: () => setShowRebootConfirm(true) },
    { label: t('topbar.user.shutdown'), onClick: () => setShowShutdownConfirm(true) },
    { divider: true },
    { label: t('topbar.user.logout'), onClick: logout, variant: 'danger' },
  ];

  return (
    <>
      <LanguagePicker />

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
        title={t('topbar.reboot.title')}
        message={t('topbar.reboot.message')}
        confirmText={t('topbar.reboot.confirm')}
        confirmClass="btn warning"
        onConfirm={() => { setShowRebootConfirm(false); api.reboot().catch(() => {}); }}
        onCancel={() => setShowRebootConfirm(false)}
      />
      <ConfirmDialog
        visible={showShutdownConfirm}
        title={t('topbar.shutdown.title')}
        message={t('topbar.shutdown.message')}
        confirmText={t('topbar.shutdown.confirm')}
        confirmClass="btn danger"
        onConfirm={() => { setShowShutdownConfirm(false); api.shutdown().catch(() => {}); }}
        onCancel={() => setShowShutdownConfirm(false)}
      />
    </>
  );
}

export default function Layout() {
  const { t } = useI18n();
  return (
    <AppShell
      appName="SmoothNAS"
      appNameShort="SN"
      navItems={buildNavItems(t)}
      topBarContent={<TopBar />}
    >
      <Outlet />
    </AppShell>
  );
}
