import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
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

function formatBytes(n: number): string {
  if (!n || n < 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(n >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function Dashboard() {
  const { health, disks, arrays, pools, datasets, protocols, alarmHistory, invalidateMany } = usePreload();
  const [hardware, setHardware] = useState<any>(null);
  const [loadedHardware, setLoadedHardware] = useState(false);
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

  useEffect(() => {
    refreshHardware();
    hwTimerRef.current = setInterval(refreshHardware, 5000);
    return () => { if (hwTimerRef.current) clearInterval(hwTimerRef.current); };
  }, []);


  function refresh() {
    invalidateMany('health', 'disks', 'arrays', 'pools', 'datasets', 'protocols', 'alarmHistory');
    refreshHardware();
  }

  const storageDisks = disks.filter((d: any) => d.assignment !== 'os');
  const enabledProtocols = protocols.filter((p: any) => p.enabled).map((p: any) => p.name.toUpperCase());

  return (
    <div className="page">
      <div className="page-header">
        <h1>Dashboard</h1>
        <p className="subtitle">System overview, health status, and alerts</p>
        <button className="refresh-btn" onClick={refresh}>Refresh</button>
      </div>

      <div className="cards">
        <div className="card">
          <div className="card-label">Service</div>
          {health ? (
            <>
              <div className={`card-value${health.status === 'ok' ? ' healthy' : ''}`}>{health.status || 'unknown'}</div>
              <div className="card-detail">v{health.version} / up {health.uptime}</div>
            </>
          ) : <Spinner loading />}
        </div>
        <div className="card">
          <div className="card-label">Disks</div>
          {disks.length > 0 || true ? (
            <>
              <div className="card-value">{storageDisks.length}</div>
              <div className="card-detail">{disks.length} total ({disks.length - storageDisks.length} OS)</div>
            </>
          ) : <Spinner loading />}
        </div>
        <div className="card">
          <div className="card-label">mdadm Arrays</div>
          <div className="card-value">{arrays.length}</div>
          <div className="card-detail"><Link to="/arrays">Manage</Link></div>
        </div>
        <div className="card">
          <div className="card-label">ZFS Pools</div>
          <div className="card-value">{pools.length}</div>
          <div className="card-detail"><Link to="/pools">Manage</Link></div>
        </div>
        <div className="card">
          <div className="card-label">Datasets</div>
          <div className="card-value">{datasets.length}</div>
          <div className="card-detail">ZFS datasets</div>
        </div>
        <div className="card">
          <div className="card-label">Sharing</div>
          <div className="card-value">{enabledProtocols.length} active</div>
          <div className="card-detail">{enabledProtocols.join(', ') || 'None'}</div>
        </div>
      </div>

      <div className="section">
        <h2>System Hardware</h2>
        {!loadedHardware ? <Spinner loading /> : hardware && (
          <div className="cards">
            <div className="card">
              <div className="card-label">CPU</div>
              <div className="card-value">{hardware.cpu.usage_pct.toFixed(1)}%</div>
              <div className="card-detail">
                {hardware.cpu.cores} cores{hardware.cpu.model ? ` · ${hardware.cpu.model}` : ''}
              </div>
              <Sparkline data={cpuHistory.current} max={100} color="#2563eb" />
            </div>
            <div className="card">
              <div className="card-label">Memory</div>
              <div className="card-value">{hardware.mem.used_pct.toFixed(1)}%</div>
              <div className="card-detail">
                {formatBytes(hardware.mem.used_bytes)} / {formatBytes(hardware.mem.total_bytes)} used
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
        <h2>Recent Alerts</h2>
        {alarmHistory.length > 0 ? (
          <table className="data-table">
            <thead>
              <tr>
                <th>Time</th><th>Device</th><th>Attribute</th><th>Severity</th><th>Value</th>
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
        ) : <p>No recent alerts.</p>}
      </div>
    </div>
  );
}
