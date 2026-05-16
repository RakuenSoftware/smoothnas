import { createContext, useContext, useState, useEffect, useCallback, ReactNode } from 'react';
import { api } from '../api/api';

type PreloadKey = 'disks' | 'arrays' | 'nonRaidArrays' | 'filesystemArrays' | 'pools' | 'datasets' | 'zvols'
  | 'snapshots' | 'protocols' | 'health' | 'alarmHistory' | 'updateChannel';

interface PreloadData {
  disks: any[];
  arrays: any[];
  nonRaidArrays: any[];
  filesystemArrays: any[];
  pools: any[];
  datasets: any[];
  zvols: any[];
  snapshots: any[];
  protocols: any[];
  health: any | null;
  alarmHistory: any[];
  updateChannel: any | null;
}

interface PreloadContextValue extends PreloadData {
  invalidate: (key: PreloadKey) => void;
  invalidateMany: (...keys: PreloadKey[]) => void;
}

const PreloadContext = createContext<PreloadContextValue>(null!);

const fetchers: Record<PreloadKey, () => Promise<any>> = {
  disks: () => api.getDisks(),
  arrays: () => api.getArrays(),
  nonRaidArrays: () => api.getNonRaidArrays(),
  filesystemArrays: () => api.getFilesystemArrays(),
  pools: () => api.getPools(),
  datasets: () => api.getDatasets(),
  zvols: () => api.getZvols(),
  snapshots: () => api.getSnapshots(),
  protocols: () => api.getProtocols(),
  health: () => api.getHealth(),
  alarmHistory: () => api.getAlarmHistory({ limit: '20' }),
  updateChannel: () => api.getUpdateChannel(),
};

const defaults: PreloadData = {
  disks: [], arrays: [], nonRaidArrays: [], filesystemArrays: [], pools: [], datasets: [], zvols: [],
  snapshots: [], protocols: [], health: null, alarmHistory: [], updateChannel: null,
};

export function PreloadProvider({ children }: { children: ReactNode }) {
  const [data, setData] = useState<PreloadData>(defaults);

  function fetchKey(key: PreloadKey) {
    fetchers[key]().then(result => {
      setData(prev => ({ ...prev, [key]: result }));
    }).catch(() => {});
  }

  useEffect(() => {
    const keys = Object.keys(fetchers) as PreloadKey[];
    keys.forEach(fetchKey);
  }, []);

  const invalidate = useCallback((key: PreloadKey) => {
    fetchKey(key);
  }, []);

  const invalidateMany = useCallback((...keys: PreloadKey[]) => {
    keys.forEach(fetchKey);
  }, []);

  return (
    <PreloadContext.Provider value={{ ...data, invalidate, invalidateMany }}>
      {children}
    </PreloadContext.Provider>
  );
}

export function usePreload(): PreloadContextValue {
  return useContext(PreloadContext);
}
