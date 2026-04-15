import { apiFetch as _apiFetch, apiFetchForm as _apiFetchForm } from '@rakuensoftware/smoothgui';

const BASE = '/api';
const apiFetch = <T = any>(method: string, path: string, body?: unknown) => _apiFetch<T>(method, BASE + path, body);
const apiFetchForm = <T = any>(method: string, path: string, form: FormData) => _apiFetchForm<T>(method, BASE + path, form);

export const api = {
  // --- Auth ---
  login: (username: string, password: string) => apiFetch('POST', '/auth/login', { username, password }),
  logout: () => apiFetch('POST', '/auth/logout', {}),
  changePassword: (currentPassword: string, newPassword: string) =>
    apiFetch('PUT', '/auth/password', { current_password: currentPassword, new_password: newPassword }),

  // --- Users ---
  getUsers: () => apiFetch<any[]>('GET', '/users'),
  createUser: (username: string, password: string) => apiFetch('POST', '/users', { username, password }),
  deleteUser: (username: string) => apiFetch('DELETE', `/users/${username}`),

  // --- Health ---
  getHealth: () => apiFetch('GET', '/health'),

  // --- Disks ---
  getDisks: () => apiFetch<any[]>('GET', '/disks'),
  getDiskSmart: (name: string) => apiFetch('GET', `/disks/${name}/smart`),
  getDiskSmartHistory: (name: string, params?: Record<string, string>) => {
    const qs = params ? '?' + new URLSearchParams(params).toString() : '';
    return apiFetch<any[]>('GET', `/disks/${name}/smart/history${qs}`);
  },
  startSmartTest: (name: string, type: string) => apiFetch('POST', `/disks/${name}/smart/test`, { type }),
  getSmartTests: (name: string) => apiFetch<any[]>('GET', `/disks/${name}/smart/test`),
  identifyDisk: (name: string) => apiFetch('POST', `/disks/${name}/identify`, {}),
  wipeDisk: (name: string) => apiFetch('POST', `/disks/${name}/wipe`, {}),

  // --- SMART Alarms ---
  getAlarmRules: () => apiFetch<any[]>('GET', '/smart/alarms'),
  createAlarmRule: (rule: any) => apiFetch('POST', '/smart/alarms', rule),
  updateAlarmRule: (id: number, rule: any) => apiFetch('PUT', `/smart/alarms/${id}`, rule),
  deleteAlarmRule: (id: number) => apiFetch('DELETE', `/smart/alarms/${id}`),
  getAlarmHistory: (params?: Record<string, string>) => {
    const qs = params ? '?' + new URLSearchParams(params).toString() : '';
    return apiFetch<any[]>('GET', `/smart/alarms/history${qs}`);
  },

  // --- Jobs ---
  getJobStatus: (jobId: string) => apiFetch('GET', `/jobs/${jobId}`),
  listJobsByTag: (tag: string) => apiFetch<any[]>('GET', `/jobs?tag=${encodeURIComponent(tag)}`),

  // --- mdadm Arrays ---
  getArrays: () => apiFetch<any[]>('GET', '/arrays'),
  createArray: (data: any) => apiFetch('POST', '/arrays', data),
  getArray: (name: string) => apiFetch('GET', `/arrays/${name}`),
  deleteArray: (name: string) => apiFetch('DELETE', `/arrays/${name}`),
  addDiskToArray: (name: string, disk: string) => apiFetch('POST', `/arrays/${name}/disks`, { disk }),
  removeDiskFromArray: (name: string, disk: string) => apiFetch('DELETE', `/arrays/${name}/disks/${disk}`),
  replaceDiskInArray: (name: string, oldDisk: string, newDisk: string) =>
    apiFetch('POST', `/arrays/${name}/disks/${oldDisk}/replace`, { new_disk: newDisk }),
  scrubArray: (name: string) => apiFetch('POST', `/arrays/${name}/scrub`, {}),

  // --- Tiers ---
  getTiers: () => apiFetch<any[]>('GET', '/tiers'),
  createTier: (name: string, tiers?: { name: string; rank: number }[]) =>
    apiFetch('POST', '/tiers', { name, ...(tiers ? { tiers } : {}) }),
  deleteTier: (name: string) => apiFetch('DELETE', `/tiers/${name}`, { confirm_pool_name: name }),
  assignTierArray: (poolName: string, slotName: string, arrayId: number) =>
    apiFetch('PUT', `/tiers/${poolName}/tiers/${slotName}`, { array_id: arrayId }),

  // --- Tiers (heat engine) ---
  getTier: (name: string) => apiFetch<any>('GET', `/tiers/${name}`),
  addTierLevel: (poolName: string, data: any) => apiFetch('POST', `/tiers/${poolName}/levels`, data),
  updateTierLevel: (poolName: string, levelName: string, data: any) => apiFetch('PUT', `/tiers/${poolName}/levels/${levelName}`, data),
  deleteTierLevel: (poolName: string, levelName: string) => apiFetch('DELETE', `/tiers/${poolName}/levels/${levelName}`),



  // --- ZFS Pools ---
  getPools: () => apiFetch<any[]>('GET', '/pools'),
  createPool: (data: any) => apiFetch('POST', '/pools', data),
  getPool: (name: string) => apiFetch('GET', `/pools/${name}`),
  deletePool: (name: string) => apiFetch('DELETE', `/pools/${name}`),
  addVdev: (pool: string, data: any) => apiFetch('POST', `/pools/${pool}/vdevs`, data),
  addSlog: (pool: string, disks: string[]) => apiFetch('POST', `/pools/${pool}/slog`, { disks }),
  removeSlog: (pool: string, disks: string[]) => apiFetch('DELETE', `/pools/${pool}/slog`, { disks }),
  addL2arc: (pool: string, disks: string[]) => apiFetch('POST', `/pools/${pool}/l2arc`, { disks }),
  removeL2arc: (pool: string, disks: string[]) => apiFetch('DELETE', `/pools/${pool}/l2arc`, { disks }),
  scrubPool: (pool: string) => apiFetch('POST', `/pools/${pool}/scrub`, {}),
  replacePoolDisk: (pool: string, oldDisk: string, newDisk: string) =>
    apiFetch('POST', `/pools/${pool}/disks/${oldDisk}/replace`, { new_disk: newDisk }),

  // --- Datasets ---
  getDatasets: (pool?: string) => {
    const qs = pool ? `?pool=${encodeURIComponent(pool)}` : '';
    return apiFetch<any[]>('GET', `/datasets${qs}`);
  },
  createDataset: (data: any) => apiFetch('POST', '/datasets', data),
  updateDataset: (id: string, props: any) => apiFetch('PUT', `/datasets/${id}`, props),
  deleteDataset: (id: string) => apiFetch('DELETE', `/datasets/${id}`),
  mountDataset: (id: string) => apiFetch('POST', `/datasets/${id}/mount`, {}),
  unmountDataset: (id: string) => apiFetch('POST', `/datasets/${id}/unmount`, {}),

  // --- Zvols ---
  getZvols: (pool?: string) => {
    const qs = pool ? `?pool=${encodeURIComponent(pool)}` : '';
    return apiFetch<any[]>('GET', `/zvols${qs}`);
  },
  createZvol: (data: any) => apiFetch('POST', '/zvols', data),
  deleteZvol: (id: string) => apiFetch('DELETE', `/zvols/${id}`),
  resizeZvol: (id: string, size: string) => apiFetch('PUT', `/zvols/${id}/resize`, { size }),

  // --- Snapshots ---
  getSnapshots: (dataset?: string) => {
    const qs = dataset ? `?dataset=${encodeURIComponent(dataset)}` : '';
    return apiFetch<any[]>('GET', `/snapshots${qs}`);
  },
  createSnapshot: (dataset: string, name: string) => apiFetch('POST', '/snapshots', { dataset, name }),
  deleteSnapshot: (id: string) => apiFetch('DELETE', `/snapshots/${id}`),
  rollbackSnapshot: (id: string) => apiFetch('POST', `/snapshots/${id}/rollback`, {}),
  cloneSnapshot: (id: string, newDataset: string) =>
    apiFetch('POST', `/snapshots/${id}/clone`, { new_dataset: newDataset }),

  // --- Unified Tiering ---
  getTieringDomains: () => apiFetch<any[]>('GET', '/tiering/domains'),
  getTieringTargets: () => apiFetch<any[]>('GET', '/tiering/targets'),
  getTieringMovements: () => apiFetch<any[]>('GET', '/tiering/movements'),
  cancelTieringMovement: (id: string) => apiFetch('DELETE', `/tiering/movements/${id}`),
  getTieringDegraded: () => apiFetch<any[]>('GET', '/tiering/degraded'),
  getTieringNamespaces: () => apiFetch<any[]>('GET', '/tiering/namespaces'),
  listNamespaceFiles: (nsID: string, prefix?: string, limit?: number) => {
    const p = new URLSearchParams();
    if (prefix) p.set('prefix', prefix);
    if (limit) p.set('limit', String(limit));
    const qs = p.toString() ? '?' + p.toString() : '';
    return apiFetch<Array<{
      path: string;
      size: number;
      inode: number;
      tier_rank: number;
      pin_state: string;
    }>>('GET', `/tiering/namespaces/${nsID}/files${qs}`);
  },
  pinObjectByPath: (nsID: string, path: string, state: 'pinned-hot' | 'pinned-cold' | 'none') =>
    state === 'none'
      ? apiFetch('DELETE', `/tiering/namespaces/${nsID}/objects/${encodeURIComponent(path)}/pin`)
      : apiFetch('PUT', `/tiering/namespaces/${nsID}/objects/${encodeURIComponent(path)}/pin`, { pin_state: state }),

  // --- Protocols ---
  getProtocols: () => apiFetch<any[]>('GET', '/protocols'),
  toggleProtocol: (name: string, enabled: boolean) => apiFetch('PUT', `/protocols/${name}`, { enabled }),

  // --- SMB ---
  getSmbShares: () => apiFetch<any[]>('GET', '/smb/shares'),
  createSmbShare: (share: any) => apiFetch('POST', '/smb/shares', share),
  deleteSmbShare: (name: string) => apiFetch('DELETE', `/smb/shares/${name}`),

  // --- NFS ---
  getNfsExports: () => apiFetch<any[]>('GET', '/nfs/exports'),
  createNfsExport: (exp: any) => apiFetch('POST', '/nfs/exports', exp),
  deleteNfsExport: (id: string) => apiFetch('DELETE', `/nfs/exports/${id}`),

  // --- iSCSI ---
  getIscsiTargets: () => apiFetch<any[]>('GET', '/iscsi/targets'),
  createIscsiTarget: (data: any) => apiFetch('POST', '/iscsi/targets', data),
  deleteIscsiTarget: (iqn: string) => apiFetch('DELETE', `/iscsi/targets/${iqn}`),

  // --- Filesystem ---
  getFilesystemPaths: () => apiFetch<any[]>('GET', '/filesystem/paths'),

  // --- Network ---
  getInterfaces: () => apiFetch<any[]>('GET', '/network/interfaces'),
  configureInterface: (name: string, config: any) => apiFetch('PUT', `/network/interfaces/${name}`, config),
  identifyInterface: (name: string) => apiFetch('POST', `/network/interfaces/${name}/identify`, {}),
  getBonds: () => apiFetch<any[]>('GET', '/network/bonds'),
  createBond: (bond: any) => apiFetch('POST', '/network/bonds', bond),
  getVlans: () => apiFetch<any[]>('GET', '/network/vlans'),
  createVlan: (vlan: any) => apiFetch('POST', '/network/vlans', vlan),
  getDns: () => apiFetch('GET', '/network/dns'),
  setDns: (dns: any) => apiFetch('PUT', '/network/dns', dns),
  getHostname: () => apiFetch('GET', '/network/hostname'),
  setHostname: (hostname: string) => apiFetch('PUT', '/network/hostname', { hostname }),
  getRoutes: () => apiFetch<any[]>('GET', '/network/routes'),
  addRoute: (route: any) => apiFetch('POST', '/network/routes', route),
  deleteRoute: (id: string) => apiFetch('DELETE', `/network/routes/${id}`),
  getPendingChange: () => apiFetch('GET', '/network/pending'),
  confirmChange: () => apiFetch('POST', '/network/pending/confirm', {}),
  revertChange: () => apiFetch('POST', '/network/pending/revert', {}),

  // --- Network Tests ---
  getExternalSpeedtestServers: () => apiFetch<any[]>('GET', '/network-tests/external/servers'),
  runNetworkTest: (req: any) => apiFetch('POST', '/network-tests/run', req),

  // --- System ---
  getSystemStatus: () => apiFetch('GET', '/system/status'),
  getSystemHardware: () => apiFetch('GET', '/system/hardware'),
  getSystemAlerts: () => apiFetch<any[]>('GET', '/system/alerts'),
  getAlertCount: () => apiFetch('GET', '/system/alerts/count'),
  clearAlert: (id: string) => apiFetch('DELETE', `/system/alerts/${id}`),
  logAlert: (message: string, severity: 'warning' | 'critical' = 'warning', source = 'gui', device = '') =>
    apiFetch('POST', '/system/alerts', { message, severity, source, device }),
  getTuning: () => apiFetch('GET', '/system/tuning'),
  setTuning: (tuning: any) => apiFetch('PUT', '/system/tuning', tuning),

  // --- Updates ---
  checkUpdate: () => apiFetch('GET', '/system/update/check'),
  applyUpdate: () => apiFetch('POST', '/system/update/apply', {}),
  getUpdateProgress: () => apiFetch('GET', '/system/update/progress'),
  getUpdateChannel: () => apiFetch('GET', '/system/update/channel'),
  setUpdateChannel: (channel: string) => apiFetch('PUT', '/system/update/channel', { channel }),
  uploadUpdate: (form: FormData) => apiFetchForm('POST', '/system/update/upload', form),

  // --- Debian Packages ---
  getDebianStatus: () => apiFetch('GET', '/system/debian/status'),
  applyDebianPackages: () => apiFetch('POST', '/system/debian/apply', {}),
  getDebianProgress: () => apiFetch('GET', '/system/debian/progress'),

  // --- Backup ---
  getBackupConfigs: () => apiFetch<any[]>('GET', '/backup/configs'),
  createBackupConfig: (cfg: any) => apiFetch('POST', '/backup/configs', cfg),
  deleteBackupConfig: (id: number) => apiFetch('DELETE', `/backup/configs/${id}`),
  runBackup: (id: number) => apiFetch('POST', `/backup/configs/${id}/run`, {}),
  getBackupRun: (runId: number) => apiFetch<any>('GET', `/backup/runs/${runId}`),
  cancelBackupRun: (runId: number) => apiFetch('POST', `/backup/runs/${runId}/cancel`, {}),
  listBackupRuns: (params?: { config_id?: number; active?: boolean }) => {
    const qs = params ? '?' + new URLSearchParams(
      Object.fromEntries(
        Object.entries({ config_id: params.config_id?.toString(), active: params.active?.toString() })
          .filter(([, v]) => v !== undefined) as [string, string][]
      )
    ).toString() : '';
    return apiFetch<any[]>('GET', `/backup/runs${qs}`);
  },

  // --- Power ---
  reboot: () => apiFetch('POST', '/system/reboot', {}),
  shutdown: () => apiFetch('POST', '/system/shutdown', {}),

  // --- Benchmark ---
  runBenchmark: (req: any) => apiFetch('POST', '/benchmark/run', req),
  runSystemBenchmark: (req: any) => apiFetch('POST', '/benchmark/system', req),

  // --- Tiering (unified control plane) ---
  listTieringNamespaces: () => apiFetch<any[]>('GET', '/tiering/namespaces'),
  getTieringNamespace: (id: string) => apiFetch<any>('GET', `/tiering/namespaces/${id}`),
  createTieringNamespaceSnapshot: (id: string) => apiFetch<any>('POST', `/tiering/namespaces/${id}/snapshot`),
  listTieringNamespaceSnapshots: (id: string) => apiFetch<any[]>('GET', `/tiering/namespaces/${id}/snapshots`),
  getTieringNamespaceSnapshot: (id: string, snapID: string) => apiFetch<any>('GET', `/tiering/namespaces/${id}/snapshots/${snapID}`),
  deleteTieringNamespaceSnapshot: (id: string, snapID: string) => apiFetch<void>('DELETE', `/tiering/namespaces/${id}/snapshots/${snapID}`),
};
