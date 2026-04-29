import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { AuthProvider, useAuth } from './contexts/AuthContext';
import { ToastProvider } from './contexts/ToastContext';
import { api } from './api/api';
import { PreloadProvider } from './contexts/PreloadContext';
import Toast from './components/Toast/Toast';
import Layout from './components/Layout/Layout';

import { I18nProvider, LoginPage } from '@rakuensoftware/smoothgui';
import {
  FALLBACK_LANGUAGE,
  SUPPORTED_LANGUAGES,
  fetchSystemLocale,
  hasUserOverride,
  persistLanguage,
  resolveInitialLanguage,
  smoothnasTranslations,
  type LanguageCode,
} from './i18n';
import { useEffect, useRef, useState } from 'react';
import Dashboard from './pages/Dashboard/Dashboard';
import Disks from './pages/Disks/Disks';
import Smart from './pages/Smart/Smart';
import Arrays from './pages/Arrays/Arrays';
import Tiers from './pages/Tiers/Tiers';
import TieringInventory from './pages/TieringInventory/TieringInventory';
import Volumes from './pages/Volumes/Volumes';
import SmoothfsPools from './pages/SmoothfsPools/SmoothfsPools';

import Sharing from './pages/Sharing/Sharing';
import Network from './pages/Network/Network';
import Users from './pages/Users/Users';
import Settings from './pages/Settings/Settings';
import Terminal from './pages/Terminal/Terminal';
import Benchmarks from './pages/Benchmarks/Benchmarks';
import Updates from './pages/Updates/Updates';
import Backups from './pages/Backups/Backups';

function ProtectedLayout() {
  const { loggedIn } = useAuth();
  if (!loggedIn) return <Navigate to="/login" replace />;
  return (
    <PreloadProvider>
      <Layout />
    </PreloadProvider>
  );
}

// LanguageSync bridges per-user language persistence with the
// I18nProvider. After login it fetches the user's saved preference
// from `GET /api/users/me/language` and applies it so the same
// account sees the same language across browsers. We only apply
// the server value if the user hasn't made an explicit choice in
// THIS browser session (URL `?lang=` / a fresh LanguagePicker
// click, both tracked by `hasUserOverride()`) — that way the
// picker click on this device wins over a stale server value
// until the picker write-back flows through.
function LanguageSync({ currentLanguage, applyLanguage }: {
  currentLanguage: LanguageCode;
  applyLanguage: (next: LanguageCode) => void;
}) {
  const { loggedIn } = useAuth();
  const lastFetchedFor = useRef<boolean>(false);

  useEffect(() => {
    if (!loggedIn) {
      lastFetchedFor.current = false;
      return;
    }
    if (lastFetchedFor.current) return;
    lastFetchedFor.current = true;
    let cancelled = false;
    api.getMyLanguage().then((res: { language: string }) => {
      if (cancelled) return;
      const lang = (res?.language || '').split('-')[0];
      const supported = new Set<string>(SUPPORTED_LANGUAGES.map(l => l.code));
      if (lang && supported.has(lang) && lang !== currentLanguage && !hasUserOverride()) {
        applyLanguage(lang as LanguageCode);
      }
    }).catch(() => {});
    return () => { cancelled = true; };
  }, [loggedIn, currentLanguage, applyLanguage]);

  return null;
}

export default function App() {
  // i18n bootstrap. Two-stage:
  //
  //   1. Synchronous: ?lang= -> localStorage -> navigator -> en.
  //      This lands a language at first paint so the app never
  //      flashes English.
  //
  //   2. Asynchronous: GET /api/locale (the installer's choice,
  //      written to /etc/smoothnas/locale during firstboot). If
  //      the operator has not made an explicit pick (no ?lang and
  //      no LanguagePicker history in localStorage) we overlay
  //      the system default once it arrives — so installing in
  //      Dutch makes the web GUI default to Dutch even when the
  //      browser's Accept-Language is English.
  const [language, setLanguage] = useState<LanguageCode>(resolveInitialLanguage);
  useEffect(() => {
    if (hasUserOverride()) return;
    let cancelled = false;
    fetchSystemLocale().then((sys) => {
      if (cancelled || !sys) return;
      setLanguage(sys);
    });
    return () => { cancelled = true; };
  }, []);
  return (
    <BrowserRouter>
      <I18nProvider
        language={language}
        defaultLanguage={FALLBACK_LANGUAGE}
        translations={smoothnasTranslations}
        onLanguageChange={(next) => {
          setLanguage(next as LanguageCode);
          persistLanguage(next as LanguageCode);
          // Best-effort write to the server so the same user sees
          // the same language across browsers. Silently ignores
          // failures (401 if not logged in, 500 if backend down).
          api.setMyLanguage(next as LanguageCode).catch(() => {});
        }}
      >
      <AuthProvider storagePrefix="sn" idleTimeoutMs={30 * 60 * 1000} onLogin={api.login} onLogout={api.logout}>
        <LanguageSync currentLanguage={language} applyLanguage={setLanguage} />
        <ToastProvider>
          <Routes>
            <Route path="/login" element={<LoginPage appName="SmoothNAS" subtitle="Storage Management" />} />
            <Route element={<ProtectedLayout />}>
              <Route path="/" element={<Navigate to="/dashboard" replace />} />
              <Route path="/dashboard" element={<Dashboard />} />
              <Route path="/disks" element={<Disks />} />
              <Route path="/smart" element={<Smart />} />
              <Route path="/arrays" element={<Arrays />} />
              <Route path="/tiers" element={<Tiers />} />
              <Route path="/tiering" element={<TieringInventory />} />
              <Route path="/volumes" element={<Volumes />} />
              <Route path="/volumes/:id" element={<Volumes />} />
              <Route path="/smoothfs-pools" element={<SmoothfsPools />} />

              <Route path="/pools" element={<Navigate to="/arrays" replace />} />
              <Route path="/sharing" element={<Sharing />} />
              <Route path="/backups" element={<Backups />} />
              <Route path="/benchmarks" element={<Benchmarks />} />
              <Route path="/network-tests" element={<Navigate to="/benchmarks" replace />} />
              <Route path="/network" element={<Network />} />
              <Route path="/users" element={<Users />} />
              <Route path="/terminal" element={<Terminal />} />
              <Route path="/updates" element={<Updates />} />
              <Route path="/settings" element={<Settings />} />
              <Route path="*" element={<Navigate to="/dashboard" replace />} />
            </Route>
          </Routes>
          <Toast />
        </ToastProvider>
      </AuthProvider>
      </I18nProvider>
    </BrowserRouter>
  );
}
