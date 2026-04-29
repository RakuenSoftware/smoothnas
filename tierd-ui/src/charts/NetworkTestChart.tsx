import { useI18n } from '@rakuensoftware/smoothgui';
import './charts.scss';

interface Props {
  points: any[];
  duration: number;
}

const W = 580, H = 180, PL = 52, PT = 10, PR = 68, PB = 32;
const plotW = W - PL - PR;
const plotH = H - PT - PB;

function axisMax(points: any[], fields: string[]): number {
  if (!points.length) return 100;
  let max = 0;
  for (const p of points) for (const f of fields) if ((p[f] ?? 0) > max) max = p[f];
  if (max <= 0) return 100;
  const mag = Math.pow(10, Math.floor(Math.log10(max)));
  return Math.ceil(max / mag) * mag;
}

function axisTicks(max: number): { y: number; label: string }[] {
  return Array.from({ length: 5 }, (_, i) => {
    const val = max * i / 4;
    const y = PT + plotH * (1 - i / 4);
    const label = val >= 1000 ? `${(val / 1000).toFixed(val >= 10000 ? 0 : 1)}k` : val.toFixed(0);
    return { y, label };
  });
}

function buildPath(points: any[], field: string, yMax: number, duration: number): string {
  if (!points.length || !duration || !yMax) return '';
  return points.map((p, i) => {
    const x = PL + (Math.min(p.elapsed_s ?? 0, duration) / duration) * plotW;
    const y = PT + plotH * (1 - Math.min(p[field] ?? 0, yMax) / yMax);
    return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
}

export default function NetworkTestChart({ points, duration }: Props) {
  const { t } = useI18n();
  const dur = Math.max(duration, 1);
  const showDownload = points.some(p => (p.download_mbps ?? 0) > 0);
  const showUpload = points.some(p => (p.upload_mbps ?? 0) > 0);
  const showLatency = points.some(p => (p.latency_ms ?? 0) > 0);
  const throughputMax = axisMax(points, ['download_mbps', 'upload_mbps']);
  const latencyMax = axisMax(points, ['latency_ms']);
  const throughputTicks = axisTicks(throughputMax);
  const latencyTicks = axisTicks(latencyMax);
  const count = Math.min(6, dur);
  const xTicks = Array.from({ length: count + 1 }, (_, i) => {
    const t = Math.round(i * dur / count);
    return { x: PL + (t / dur) * plotW, label: `${t}s` };
  });

  return (
    <div className="bench-chart">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="xMidYMid meet" className="chart-svg">
        {throughputTicks.map(t => <line key={t.label} x1={PL} y1={t.y} x2={W - PR} y2={t.y} className="chart-grid" />)}
        {throughputTicks.map(t => <text key={t.label} x={PL - 6} y={t.y + 4} className="chart-tick chart-tick-left" textAnchor="end">{t.label}</text>)}
        {showLatency && latencyTicks.map(t => <text key={t.label} x={W - PR + 6} y={t.y + 4} className="chart-tick chart-tick-right" textAnchor="start">{t.label}</text>)}
        {xTicks.map(t => <text key={t.label} x={t.x} y={H - 6} className="chart-tick" textAnchor="middle">{t.label}</text>)}
        <text x={12} y={PT + plotH / 2} className="chart-axis-title chart-axis-left" transform={`rotate(-90,12,${PT + plotH / 2})`}>Mbps</text>
        {showLatency && <text x={W - 12} y={PT + plotH / 2} className="chart-axis-title chart-axis-right" transform={`rotate(90,${W - 12},${PT + plotH / 2})`}>{t('benchmarks.chart.latencyAxis')}</text>}
        <rect x={PL} y={PT} width={plotW} height={plotH} fill="none" stroke="#e0e0e0" strokeWidth={1} />
        {showDownload && <path d={buildPath(points, 'download_mbps', throughputMax, dur)} fill="none" stroke="#1976d2" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />}
        {showUpload && <path d={buildPath(points, 'upload_mbps', throughputMax, dur)} fill="none" stroke="#ef6c00" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />}
        {showLatency && <path d={buildPath(points, 'latency_ms', latencyMax, dur)} fill="none" stroke="#546e7a" strokeWidth={1.5} strokeDasharray="4,3" strokeLinejoin="round" strokeLinecap="round" />}
      </svg>
      <div className="chart-legend">
        {showDownload && <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#1976d2" strokeWidth="2" /></svg>{t('benchmarks.chart.downloadMbps')}</span>}
        {showUpload && <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#ef6c00" strokeWidth="2" /></svg>{t('benchmarks.chart.uploadMbps')}</span>}
        {showLatency && <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#546e7a" strokeWidth="1.5" strokeDasharray="4,3" /></svg>{t('benchmarks.net.latency')}</span>}
      </div>
    </div>
  );
}
