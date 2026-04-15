import './charts.scss';

interface Props {
  points: any[];
  mode: string;
  duration: number;
}

const W = 580, H = 180, PL = 52, PT = 10, PR = 52, PB = 32;
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
    const label = val >= 1000 ? (val / 1000).toFixed(val >= 10000 ? 0 : 1) + 'k' : val.toFixed(0);
    return { y, label };
  });
}

function buildPath(points: any[], field: string, yMax: number, duration: number): string {
  if (!points.length || !duration || !yMax) return '';
  return points.map((p, i) => {
    const x = PL + (p.elapsed_s / duration) * plotW;
    const y = PT + plotH * (1 - Math.min(p[field] ?? 0, yMax) / yMax);
    return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
}

export default function BenchChart({ points, mode, duration }: Props) {
  const showRead = ['randrw', 'randread', 'read'].includes(mode);
  const showWrite = ['randrw', 'randwrite', 'write'].includes(mode);
  const mbpsMax = axisMax(points, ['read_mbps', 'write_mbps']);
  const iopsMax = axisMax(points, ['read_iops', 'write_iops']);
  const mbpsTicks = axisTicks(mbpsMax);
  const iopsTicks = axisTicks(iopsMax);
  const count = Math.min(6, duration);
  const xTicks = Array.from({ length: count + 1 }, (_, i) => {
    const t = Math.round(i * duration / count);
    return { x: PL + (t / duration) * plotW, label: t + 's' };
  });

  return (
    <div className="bench-chart">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="xMidYMid meet" className="chart-svg">
        {mbpsTicks.map(t => <line key={t.label} x1={PL} y1={t.y} x2={W - PR} y2={t.y} className="chart-grid" />)}
        {mbpsTicks.map(t => <text key={t.label} x={PL - 6} y={t.y + 4} className="chart-tick chart-tick-left" textAnchor="end">{t.label}</text>)}
        {iopsTicks.map(t => <text key={t.label} x={W - PR + 6} y={t.y + 4} className="chart-tick chart-tick-right" textAnchor="start">{t.label}</text>)}
        {xTicks.map(t => <text key={t.label} x={t.x} y={H - 6} className="chart-tick" textAnchor="middle">{t.label}</text>)}
        <text x={12} y={PT + plotH / 2} className="chart-axis-title chart-axis-left" transform={`rotate(-90,12,${PT + plotH / 2})`}>MB/s</text>
        <text x={W - 12} y={PT + plotH / 2} className="chart-axis-title chart-axis-right" transform={`rotate(90,${W - 12},${PT + plotH / 2})`}>IOPS</text>
        <rect x={PL} y={PT} width={plotW} height={plotH} fill="none" stroke="#e0e0e0" strokeWidth={1} />
        {showRead && <path d={buildPath(points, 'read_mbps', mbpsMax, duration)} fill="none" stroke="#1976d2" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />}
        {showWrite && <path d={buildPath(points, 'write_mbps', mbpsMax, duration)} fill="none" stroke="#ef6c00" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />}
        {showRead && <path d={buildPath(points, 'read_iops', iopsMax, duration)} fill="none" stroke="#1976d2" strokeWidth={1.5} strokeDasharray="4,3" strokeLinejoin="round" strokeLinecap="round" />}
        {showWrite && <path d={buildPath(points, 'write_iops', iopsMax, duration)} fill="none" stroke="#ef6c00" strokeWidth={1.5} strokeDasharray="4,3" strokeLinejoin="round" strokeLinecap="round" />}
      </svg>
      <div className="chart-legend">
        {showRead && <>
          <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#1976d2" strokeWidth="2" /></svg>Read MB/s</span>
          <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#1976d2" strokeWidth="1.5" strokeDasharray="4,3" /></svg>Read IOPS</span>
        </>}
        {showWrite && <>
          <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#ef6c00" strokeWidth="2" /></svg>Write MB/s</span>
          <span className="legend-item"><svg width="24" height="12" className="legend-icon"><line x1="0" y1="6" x2="24" y2="6" stroke="#ef6c00" strokeWidth="1.5" strokeDasharray="4,3" /></svg>Write IOPS</span>
        </>}
      </div>
    </div>
  );
}
