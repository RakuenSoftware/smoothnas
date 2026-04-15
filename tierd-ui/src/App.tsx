import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { AuthProvider, useAuth } from './contexts/AuthContext';
import { ToastProvider } from './contexts/ToastContext';
import { api } from './api/api';
import { PreloadProvider } from './contexts/PreloadContext';
import Toast from './components/Toast/Toast';
import Layout from './components/Layout/Layout';

import { LoginPage } from '@rakuensoftware/smoothgui';
import Dashboard from './pages/Dashboard/Dashboard';
import Disks from './pages/Disks/Disks';
import Smart from './pages/Smart/Smart';
import Arrays from './pages/Arrays/Arrays';
import Tiers from './pages/Tiers/Tiers';
import TieringInventory from './pages/TieringInventory/TieringInventory';

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

export default function App() {
  return (
    <BrowserRouter>
      <AuthProvider storagePrefix="sn" idleTimeoutMs={30 * 60 * 1000} onLogin={api.login} onLogout={api.logout}>
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
    </BrowserRouter>
  );
}
