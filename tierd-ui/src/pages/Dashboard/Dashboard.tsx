import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { api } from '../../api/api';
import Spinner from '../../components/Spinner/Spinner';

const HISTORY_MAX = 60; // 60 × 5 s = 5 minutes

function Sparkline({ data, max: forcedMax, color = '#2563eb' }: { data: number[]; max?: number; color?: string }) {
  if (data.length < 2) return <svg width={120} height={36} />;
  const w = 120, h = 36;
  const peak = forcedMax ?? Math.max(...data, 1);
  const pts = data.map((v, i) => {
    const x = (i / (data.length - 1)) * w;
    const y = h - (v / peak) * (h - 2) - 1;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
  return (
    <svg width={w} height={h} style={{ display: 'block', marginTop: 4 }}>
      <polyline points={pts} fill="none" stroke={color} strokeWidth={1.5} strokeLinejoin="round" />
    </svg>
  );
}

// formatBytes auto-scales sizes to the nearest binary unit. The
// unit suffixes (B / KiB / MiB / …) are IEC abbreviations and stay
// in English across locales — translating them would break the
// data they label.
function formatBytes(n: number): string {
  if (!n || n < 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(n >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function Dashboard() {
  const { t } = useI18n();
  const { health, disks, arrays, pools, datasets, protocols, alarmHistory, invalidateMany } = usePreload();
  const [hardware, setHardware] = useState<any>(null);
  const [loadedHardware, setLoadedHardware] = useState(false);
  const [tieringSummary, setTieringSummary] = useState({
    loaded: false,
    activeMigrations: 0,
    migrationBacklog: 0,
    nearSpillover: 0,
    fullThreshold: 0,
    busiestTier: '',
  });
  const prevNICs = useRef(new Map<string, { rx: number; tx: number; ts: number }>());
  const nicRates = useRef(new Map<string, { rxBps: number; txBps: number }>());
  const hwTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const [, forceUpdate] = useState(0);

  // Rolling history for graphs
  const cpuHistory = useRef<number[]>([]);
  const memHistory = useRef<number[]>([]);
  const nicHistory = useRef(new Map<string, { rx: number[]; tx: number[] }>());

  function refreshHardware() {
    api.getSystemHardware().then((hw: any) => {
      const now = Date.now();
      for (const nic of hw?.nics || []) {
        const prev = prevNICs.current.get(nic.name);
        if (prev) {
          const dt = (now - prev.ts) / 1000;
          if (dt > 0) {
            nicRates.current.set(nic.name, {
              rxBps: Math.max(0, (nic.rx_bytes - prev.rx) / dt),
              txBps: Math.max(0, (nic.tx_bytes - prev.tx) / dt),
            });
          }
        }
        prevNICs.current.set(nic.name, { rx: nic.rx_bytes, tx: nic.tx_bytes, ts: now });
      }
      // Append to history buffers.
      const push = (arr: number[], val: number) => {
        arr.push(val);
        if (arr.length > HISTORY_MAX) arr.shift();
      };
      push(cpuHistory.current, hw?.cpu?.usage_pct ?? 0);
      push(memHistory.current, hw?.mem?.used_pct ?? 0);
      for (const nic of hw?.nics || []) {
        const rate = nicRates.current.get(nic.name) || { rxBps: 0, txBps: 0 };
        if (!nicHistory.current.has(nic.name)) nicHistory.current.set(nic.name, { rx: [], tx: [] });
        const h = nicHistory.current.get(nic.name)!;
        push(h.rx, rate.rxBps);
        push(h.tx, rate.txBps);
      }

      setHardware(hw);
      setLoadedHardware(true);
      forceUpdate(n => n + 1);
    }).catch(() => setLoadedHardware(true));
  }

  function refreshTieringSummary() {
    Promise.all([
      api.getTieringMovements().catch(() => []),
      api.getTiers().catch(() => []),
    ]).then(([movements, tiers]: [any[], any[]]) => {
      const activeMigrations = movements.filter((j: any) => j.state === 'running').length;
      const migrationBacklog = movements.filter((j: any) => j.state === 'pending').length;
      const levels = tiers.flatMap((pool: any) =>
        (pool.tiers || []).map((level: any) => {
          const capacity = Number(level.capacity_bytes || 0);
          const used = Number(level.used_bytes || 0);
          const fill = capacity > 0 ? (used / capacity) * 100 : 0;
          return {
            pool: pool.name,
            name: level.name,
            fill,
            target: Number(level.target_fill_pct || 0),
            full: Number(level.full_threshold_pct || 0),
          };
        })
      );
      const near = levels.filter((level: any) =>
        level.full > 0 && level.fill >= Math.max(level.target, level.full - 5)
      );
      const full = levels.filter((level: any) => level.full > 0 && level.fill >= level.full);
      const busiest = levels
        .filter((level: any) => level.fill > 0)
        .sort((a: any, b: any) => b.fill - a.fill)[0];
      setTieringSummary({
        loaded: true,
        activeMigrations,
        migrationBacklog,
        nearSpillover: near.length,
        fullThreshold: full.length,
        busiestTier: busiest
          ? `${busiest.pool}/${busiest.name} ${busiest.fill.toFixed(1)}%`
          : t('dashboard.tiering.noUsage'),
      });
    }).catch(() => {
      setTieringSummary(prev => ({ ...prev, loaded: true }));
    });
  }

  useEffect(() => {
    refreshHardware();
    refreshTieringSummary();
    hwTimerRef.current = setInterval(refreshHardware, 5000);
    return () => { if (hwTimerRef.current) clearInterval(hwTimerRef.current); };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);


  function refresh() {
    invalidateMany('health', 'disks', 'arrays', 'pools', 'datasets', 'protocols', 'alarmHistory');
    refreshHardware();
    refreshTieringSummary();
  }

  const storageDisks = disks.filter((d: any) => d.assignment !== 'os');
  const enabledProtocols = protocols.filter((p: any) => p.enabled).map((p: any) => p.name.toUpperCase());
  const healthIssues = (health?.checks || []).filter((c: any) => c.status && c.status !== 'ok');

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('dashboard.title')}</h1>
        <p className="subtitle">{t('dashboard.subtitle')}</p>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      <div className="cards">
        <div className="card">
          <div className="card-label">{t('dashboard.card.service')}</div>
          {health ? (
            <>
              <div
                className={`card-value${health.status === 'ok' ? ' healthy' : ''}`}
                style={health.status === 'critical' ? { color: '#b91c1c' } : health.status === 'warning' ? { color: '#a16207' } : undefined}
              >
                {health.status || t('common.unknown')}
              </div>
              <div className="card-detail">
                {healthIssues.length > 0
                  ? t('dashboard.service.issues', { count: healthIssues.length })
                  : t('dashboard.service.versionUptime', { version: health.version, uptime: health.uptime })}
              </div>
            </>
          ) : <Spinner loading />}
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.disks')}</div>
          <div className="card-value">{storageDisks.length}</div>
          <div className="card-detail">
            {t('dashboard.disks.detail', { total: disks.length, os: disks.length - storageDisks.length })}
          </div>
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.mdadmArrays')}</div>
          <div className="card-value">{arrays.length}</div>
          <div className="card-detail"><Link to="/arrays">{t('dashboard.link.manage')}</Link></div>
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.zfsPools')}</div>
          <div className="card-value">{pools.length}</div>
          <div className="card-detail"><Link to="/pools">{t('dashboard.link.manage')}</Link></div>
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.activeMigrations')}</div>
          {tieringSummary.loaded ? (
            <>
              <div className="card-value">{tieringSummary.activeMigrations}</div>
              <div className="card-detail"><Link to="/tiering">{t('dashboard.link.tieringInventory')}</Link></div>
            </>
          ) : <Spinner loading />}
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.migrationBacklog')}</div>
          {tieringSummary.loaded ? (
            <>
              <div className="card-value">{tieringSummary.migrationBacklog}</div>
              <div className="card-detail">{t('dashboard.backlog.detail')}</div>
            </>
          ) : <Spinner loading />}
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.nearSpillover')}</div>
          {tieringSummary.loaded ? (
            <>
              <div
                className="card-value"
                style={tieringSummary.fullThreshold > 0 ? { color: '#b91c1c' } : tieringSummary.nearSpillover > 0 ? { color: '#a16207' } : undefined}
              >
                {tieringSummary.nearSpillover}
              </div>
              <div className="card-detail">{tieringSummary.busiestTier}</div>
            </>
          ) : <Spinner loading />}
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.datasets')}</div>
          <div className="card-value">{datasets.length}</div>
          <div className="card-detail">{t('dashboard.datasets.detail')}</div>
        </div>
        <div className="card">
          <div className="card-label">{t('dashboard.card.sharing')}</div>
          <div className="card-value">
            {t('dashboard.sharing.activeCount', { count: enabledProtocols.length })}
          </div>
          <div className="card-detail">{enabledProtocols.join(', ') || t('common.none')}</div>
        </div>
      </div>

      <div className="section">
        <h2>{t('dashboard.section.systemHardware')}</h2>
        {!loadedHardware ? <Spinner loading /> : hardware && (
          <div className="cards">
            <div className="card">
              <div className="card-label">{t('dashboard.card.cpu')}</div>
              <div className="card-value">{hardware.cpu.usage_pct.toFixed(1)}%</div>
              <div className="card-detail">
                {hardware.cpu.model
                  ? t('dashboard.cpu.detail', { cores: hardware.cpu.cores, model: hardware.cpu.model })
                  : t('dashboard.cpu.detailNoModel', { cores: hardware.cpu.cores })}
              </div>
              <Sparkline data={cpuHistory.current} max={100} color="#2563eb" />
            </div>
            <div className="card">
              <div className="card-label">{t('dashboard.card.memory')}</div>
              <div className="card-value">{hardware.mem.used_pct.toFixed(1)}%</div>
              <div className="card-detail">
                {t('dashboard.memory.detail', {
                  used: formatBytes(hardware.mem.used_bytes),
                  total: formatBytes(hardware.mem.total_bytes),
                })}
              </div>
              <Sparkline data={memHistory.current} max={100} color="#16a34a" />
            </div>
            {(hardware.nics || []).map((nic: any) => {
              const rate = nicRates.current.get(nic.name) || { rxBps: 0, txBps: 0 };
              const nicH = nicHistory.current.get(nic.name) || { rx: [], tx: [] };
              const nicPeak = Math.max(...nicH.rx, ...nicH.tx, 1);
              return (
                <div className="card" key={nic.name}>
                  <div className="card-label">{nic.name}</div>
                  <div className={`card-value${nic.link === 'up' ? ' healthy' : ''}`}>{nic.link}</div>
                  <div className="card-detail">
                    {nic.speed_mbps > 0 ? `${nic.speed_mbps} Mb/s · ` : ''}
                    ↓ {formatBytes(rate.rxBps)}/s ↑ {formatBytes(rate.txBps)}/s
                  </div>
                  <svg width={120} height={36} style={{ display: 'block', marginTop: 4 }}>
                    <polyline points={nicH.rx.map((v, i) => `${((i / (nicH.rx.length - 1 || 1)) * 120).toFixed(1)},${(36 - (v / nicPeak) * 34 - 1).toFixed(1)}`).join(' ')}
                      fill="none" stroke="#2563eb" strokeWidth={1.5} strokeLinejoin="round" />
                    <polyline points={nicH.tx.map((v, i) => `${((i / (nicH.tx.length - 1 || 1)) * 120).toFixed(1)},${(36 - (v / nicPeak) * 34 - 1).toFixed(1)}`).join(' ')}
                      fill="none" stroke="#dc2626" strokeWidth={1.5} strokeLinejoin="round" />
                  </svg>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <div className="section">
        <h2>{t('dashboard.section.recentAlerts')}</h2>
        {alarmHistory.length > 0 ? (
          <table className="data-table">
            <thead>
              <tr>
                <th>{t('dashboard.alerts.time')}</th>
                <th>{t('dashboard.alerts.device')}</th>
                <th>{t('dashboard.alerts.attribute')}</th>
                <th>{t('dashboard.alerts.severity')}</th>
                <th>{t('dashboard.alerts.value')}</th>
              </tr>
            </thead>
            <tbody>
              {alarmHistory.map((event: any, i: number) => (
                <tr key={i} className={event.severity === 'critical' ? 'critical' : event.severity === 'warning' ? 'warning' : ''}>
                  <td>{event.timestamp}</td>
                  <td>{event.device_path}</td>
                  <td>{event.attr_name}</td>
                  <td>{event.severity}</td>
                  <td>{event.value}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <p>{t('dashboard.alerts.empty')}</p>}
      </div>
    </div>
  );
}
