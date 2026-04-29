import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import BenchChart from '../../charts/BenchChart';
import NetworkTestChart from '../../charts/NetworkTestChart';

export default function Benchmarks() {
  const { t } = useI18n();
  const [category, setCategory] = useState<'io' | 'system' | 'network'>('io');

  // --- I/O benchmark state ---
  const [protocol, setProtocol] = useState('smb');
  const [path, setPath] = useState('');
  const [duration, setDuration] = useState(30);
  const [sizeMB, setSizeMB] = useState(256);
  const [blockSize, setBlockSize] = useState('4k');
  const [mode, setMode] = useState('randrw');
  const [remoteMode, setRemoteMode] = useState(false);
  const [remoteHost, setRemoteHost] = useState('');
  const [remoteShare, setRemoteShare] = useState('');
  const [remoteUser, setRemoteUser] = useState('');
  const [remotePass, setRemotePass] = useState('');
  const [mountOptions, setMountOptions] = useState('');
  const [shares, setShares] = useState<any[]>([]);
  const [nfsExports, setNfsExports] = useState<any[]>([]);
  const [iscsiTargets, setIscsiTargets] = useState<any[]>([]);
  const [localPaths, setLocalPaths] = useState<any[]>([]);
  const [loadingTargets, setLoadingTargets] = useState(false);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState('');
  const [progress, setProgress] = useState('');
  const [result, setResult] = useState<any>(null);
  const [liveResult, setLiveResult] = useState<any>(null);
  const [history, setHistory] = useState<any[]>([]);
  const [selectedHistory, setSelectedHistory] = useState<any>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // --- System benchmark state ---
  const [sysDuration, setSysDuration] = useState(10);
  const [sysRunning, setSysRunning] = useState(false);
  const [sysError, setSysError] = useState('');
  const [sysProgress, setSysProgress] = useState('');
  const [sysResult, setSysResult] = useState<any>(null);
  const [sysHistory, setSysHistory] = useState<any[]>([]);
  const sysPollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // --- Network test state ---
  const [netTestType, setNetTestType] = useState<'local' | 'external'>('local');
  const [netMode, setNetMode] = useState<'download' | 'upload'>('download');
  const [netHost, setNetHost] = useState('');
  const [netPort, setNetPort] = useState(5201);
  const [netDuration, setNetDuration] = useState(15);
  const [netStreams, setNetStreams] = useState(1);
  const [netAutoServer, setNetAutoServer] = useState(true);
  const [netExternalServers, setNetExternalServers] = useState<any[]>([]);
  const [netSelectedServerId, setNetSelectedServerId] = useState('');
  const [netServerFilter, setNetServerFilter] = useState('');
  const [netLoadingServers, setNetLoadingServers] = useState(false);
  const [netServerError, setNetServerError] = useState('');
  const [netRunning, setNetRunning] = useState(false);
  const [netError, setNetError] = useState('');
  const [netProgress, setNetProgress] = useState('');
  const [netResult, setNetResult] = useState<any>(null);
  const [netLiveResult, setNetLiveResult] = useState<any>(null);
  const [netHistory, setNetHistory] = useState<any[]>([]);
  const [netSelectedHistory, setNetSelectedHistory] = useState<any>(null);
  const netPollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const BLOCK_SIZES = ['4k', '8k', '16k', '32k', '64k', '128k', '512k', '1m'];
  const MODES = [
    { value: 'randrw', label: t('benchmarks.io.modeRandrw') },
    { value: 'randread', label: t('benchmarks.io.modeRandread') },
    { value: 'randwrite', label: t('benchmarks.io.modeRandwrite') },
    { value: 'read', label: t('benchmarks.io.modeRead') },
    { value: 'write', label: t('benchmarks.io.modeWrite') },
  ];

  useEffect(() => {
    loadTargets();
    return () => {
      if (pollRef.current) clearInterval(pollRef.current);
      if (sysPollRef.current) clearInterval(sysPollRef.current);
      if (netPollRef.current) clearInterval(netPollRef.current);
    };
  }, []);

  function loadTargets() {
    setLoadingTargets(true);
    let remaining = 4;
    const done = () => { if (--remaining === 0) setLoadingTargets(false); };
    api.getSmbShares().then(s => { setShares(s); done(); }).catch(done);
    api.getNfsExports().then(e => { setNfsExports(e); done(); }).catch(done);
    api.getFilesystemPaths().then(p => { setLocalPaths(p); done(); }).catch(done);
    api.getIscsiTargets().then(t => { setIscsiTargets(t); done(); }).catch(done);
  }

  const targets = (() => {
    if (protocol === 'smb') return shares.map((s: any) => ({ label: s.name, path: s.path }));
    if (protocol === 'nfs') return nfsExports.map((e: any) => ({ label: e.path, path: e.path }));
    if (protocol === 'iscsi') return iscsiTargets.map((t: any) => ({ label: t.iqn, path: t.block_device }));
    if (protocol === 'local') return localPaths.map((p: any) => ({ label: `${p.name} (${p.source})`, path: p.path }));
    return [];
  })();

  const runDisabled = running || (remoteMode ? !remoteHost || !remoteShare : !path);

  function onProtocolChange(p: string) {
    setProtocol(p);
    setPath('');
    setRemoteShare('');
    setResult(null);
    if (p === 'iscsi' || p === 'local') setRemoteMode(false);
  }

  function run() {
    setError('');
    setResult(null);
    setLiveResult(null);
    setProgress(t('benchmarks.io.starting'));
    setRunning(true);
    const req: any = { protocol, duration, size_mb: sizeMB, block_size: blockSize, mode };
    if (remoteMode) {
      req.remote = true;
      req.remote_host = remoteHost;
      req.remote_share = remoteShare;
      if (remoteUser) { req.remote_user = remoteUser; req.remote_pass = remotePass; }
      if (mountOptions) req.mount_options = mountOptions;
    } else {
      req.path = path;
    }
    api.runBenchmark(req).then((res: any) => { pollJob(res.job_id); })
      .catch((e: any) => { setError(e.message || t('benchmarks.io.failed')); stopRunning(); });
  }

  function pollJob(jobId: string) {
    if (pollRef.current) clearInterval(pollRef.current);
    let pending = false;
    pollRef.current = setInterval(() => {
      if (pending) return;
      pending = true;
      api.getJobStatus(jobId).then((job: any) => {
        pending = false;
        if (job.progress && job.progress !== progress) setProgress(job.progress);
        if (job.status === 'running' && job.result?.data_points?.length) setLiveResult(job.result);
        if (job.status === 'completed') {
          if (pollRef.current) clearInterval(pollRef.current);
          setResult(job.result);
          setHistory(prev => [job.result, ...prev].slice(0, 10));
          stopRunning();
        } else if (job.status === 'failed') {
          if (pollRef.current) clearInterval(pollRef.current);
          setError(job.error || t('benchmarks.io.failed'));
          stopRunning();
        }
      }).catch(() => {
        pending = false;
        if (pollRef.current) clearInterval(pollRef.current);
        setError(t('benchmarks.io.lostConn'));
        stopRunning();
      });
    }, 1000);
  }

  function stopRunning() { setRunning(false); setProgress(''); setLiveResult(null); }

  // --- System benchmark ---
  function runSystem() {
    setSysError('');
    setSysResult(null);
    setSysProgress(t('benchmarks.system.starting'));
    setSysRunning(true);
    api.runSystemBenchmark({ duration: sysDuration })
      .then((res: any) => { pollSysJob(res.job_id); })
      .catch((e: any) => { setSysError(e.message || t('benchmarks.system.failed')); setSysRunning(false); setSysProgress(''); });
  }

  function pollSysJob(jobId: string) {
    if (sysPollRef.current) clearInterval(sysPollRef.current);
    let pending = false;
    sysPollRef.current = setInterval(() => {
      if (pending) return;
      pending = true;
      api.getJobStatus(jobId).then((job: any) => {
        pending = false;
        if (job.progress) setSysProgress(job.progress);
        if (job.status === 'completed') {
          if (sysPollRef.current) clearInterval(sysPollRef.current);
          setSysResult(job.result);
          setSysHistory(prev => [job.result, ...prev].slice(0, 10));
          setSysRunning(false);
          setSysProgress('');
        } else if (job.status === 'failed') {
          if (sysPollRef.current) clearInterval(sysPollRef.current);
          setSysError(job.error || t('benchmarks.system.failed'));
          setSysRunning(false);
          setSysProgress('');
        }
      }).catch(() => {
        pending = false;
        if (sysPollRef.current) clearInterval(sysPollRef.current);
        setSysError(t('benchmarks.io.lostConn'));
        setSysRunning(false);
        setSysProgress('');
      });
    }, 1000);
  }

  // --- Network test ---
  const netFilteredServers = netServerFilter
    ? netExternalServers.filter(s => (s.label || '').toLowerCase().includes(netServerFilter.toLowerCase()))
    : netExternalServers;

  const netRunDisabled = netRunning || (netTestType === 'local' ? !netHost.trim() : !netAutoServer && !netSelectedServerId);

  function onNetTestTypeChange(type: 'local' | 'external') {
    setNetTestType(type);
    setNetError('');
    setNetResult(null);
    setNetLiveResult(null);
    if (type === 'external' && netExternalServers.length === 0 && !netLoadingServers) loadExternalServers();
  }

  function loadExternalServers() {
    setNetLoadingServers(true);
    setNetServerError('');
    api.getExternalSpeedtestServers().then(servers => {
      setNetExternalServers(servers);
      setNetLoadingServers(false);
    }).catch(e => {
      setNetServerError(extractError(e, t('benchmarks.net.error.loadServers')));
      setNetLoadingServers(false);
    });
  }

  function runNet() {
    setNetError('');
    setNetResult(null);
    setNetLiveResult(null);
    setNetProgress(netTestType === 'local' ? t('benchmarks.net.startingLocal') : t('benchmarks.net.startingExternal'));
    setNetRunning(true);
    const req: any = { type: netTestType, duration: netDuration };
    if (netTestType === 'local') { req.host = netHost.trim(); req.port = netPort; req.streams = netStreams; req.mode = netMode; }
    else if (!netAutoServer && netSelectedServerId) req.server_id = netSelectedServerId;
    api.runNetworkTest(req).then((res: any) => { pollNetJob(res.job_id); })
      .catch(e => { setNetError(extractError(e, t('benchmarks.net.failed'))); setNetRunning(false); setNetProgress(''); });
  }

  function pollNetJob(jobId: string) {
    if (netPollRef.current) clearInterval(netPollRef.current);
    let pending = false;
    netPollRef.current = setInterval(() => {
      if (pending) return;
      pending = true;
      api.getJobStatus(jobId).then((job: any) => {
        pending = false;
        if (job.progress && job.progress !== netProgress) setNetProgress(job.progress);
        if (job.status === 'running' && job.result?.data_points?.length) setNetLiveResult(job.result);
        if (job.status === 'completed') {
          if (netPollRef.current) clearInterval(netPollRef.current);
          setNetResult(job.result);
          setNetHistory(prev => [job.result, ...prev].slice(0, 10));
          setNetRunning(false);
          setNetProgress('');
          setNetLiveResult(null);
        } else if (job.status === 'failed') {
          if (netPollRef.current) clearInterval(netPollRef.current);
          setNetError(job.error || t('benchmarks.net.failed'));
          setNetRunning(false);
          setNetProgress('');
          setNetLiveResult(null);
        }
      }).catch(() => {
        pending = false;
        if (netPollRef.current) clearInterval(netPollRef.current);
        setNetError(t('benchmarks.net.lostConn'));
        setNetRunning(false);
        setNetProgress('');
        setNetLiveResult(null);
      });
    }, 1000);
  }

  function fmt(n: number, decimals = 1): string {
    if (n == null) return '\u2014';
    return n.toFixed(decimals);
  }

  function fmtInt(n: number): string {
    if (n == null || n === 0) return '\u2014';
    return Math.round(n).toLocaleString();
  }

  const displayResult = liveResult || result;
  const netDisplayResult = netLiveResult || netResult;

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('benchmarks.title')}</h1>
        <p className="subtitle">{t('benchmarks.subtitle')}</p>
      </div>

      <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
        <button className={`btn ${category === 'io' ? 'primary' : 'secondary'}`} onClick={() => setCategory('io')}>{t('benchmarks.tab.io')}</button>
        <button className={`btn ${category === 'system' ? 'primary' : 'secondary'}`} onClick={() => setCategory('system')}>{t('benchmarks.tab.system')}</button>
        <button className={`btn ${category === 'network' ? 'primary' : 'secondary'}`} onClick={() => setCategory('network')}>{t('benchmarks.tab.network')}</button>
      </div>

      {category === 'io' && (
        <>
      <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
        <div className="form-row">
          <label>{t('benchmarks.io.protocol')}
            <select value={protocol} onChange={e => onProtocolChange(e.target.value)}>
              <option value="smb">SMB</option>
              <option value="nfs">NFS</option>
              <option value="iscsi">iSCSI</option>
              <option value="local">{t('benchmarks.io.protocolLocal')}</option>
            </select>
          </label>
          <label>{t('benchmarks.io.mode')}
            <select value={mode} onChange={e => { setMode(e.target.value); setResult(null); }}>
              {MODES.map(m => <option key={m.value} value={m.value}>{m.label}</option>)}
            </select>
          </label>
          <label>{t('benchmarks.io.blockSize')}
            <select value={blockSize} onChange={e => setBlockSize(e.target.value)}>
              {BLOCK_SIZES.map(b => <option key={b} value={b}>{b}</option>)}
            </select>
          </label>
          <label>{t('benchmarks.io.duration')} <input type="number" value={duration} onChange={e => setDuration(parseInt(e.target.value))} min={5} max={300} /></label>
          <label>{t('benchmarks.io.fileSize')} <input type="number" value={sizeMB} onChange={e => setSizeMB(parseInt(e.target.value))} min={64} /></label>
        </div>

        {protocol !== 'iscsi' && protocol !== 'local' && (
          <div style={{ marginBottom: 12 }}>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', fontSize: 13 }}>
              <input type="checkbox" checked={remoteMode} onChange={e => setRemoteMode(e.target.checked)} />
              {t('benchmarks.io.remoteCheckbox')}
            </label>
          </div>
        )}

        {!remoteMode ? (
          <div className="form-row">
            <label>{t('benchmarks.io.targetPath')}
              <select value={path} onChange={e => setPath(e.target.value)}>
                <option value="">{t('benchmarks.io.targetPlaceholder')}</option>
                {targets.map((tgt: any) => <option key={tgt.path} value={tgt.path}>{tgt.label}</option>)}
              </select>
            </label>
          </div>
        ) : (
          <div className="form-row">
            <label>{t('benchmarks.io.remoteHost')} <input value={remoteHost} onChange={e => setRemoteHost(e.target.value)} /></label>
            <label>{t('benchmarks.io.share')} <input value={remoteShare} onChange={e => setRemoteShare(e.target.value)} /></label>
            <label>{t('benchmarks.io.userOpt')} <input value={remoteUser} onChange={e => setRemoteUser(e.target.value)} /></label>
            <label>{t('benchmarks.io.password')} <input type="password" value={remotePass} onChange={e => setRemotePass(e.target.value)} /></label>
            <label>{t('benchmarks.io.mountOptions')} <input value={mountOptions} onChange={e => setMountOptions(e.target.value)} /></label>
          </div>
        )}

        {error && <div className="error-msg">{error}</div>}
        {progress && <div className="status-banner info">{progress}</div>}

        <button className="btn primary" onClick={run} disabled={runDisabled} style={{ marginTop: 8 }}>
          {running ? t('benchmarks.running') : t('benchmarks.io.run')}
        </button>
      </div>

      {displayResult && (
        <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
          <h3 style={{ margin: '0 0 12px' }}>{liveResult ? t('benchmarks.result.live') : t('benchmarks.result.title')}</h3>
          <div className="cards">
            {displayResult.read_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.io.read')}</div><div className="card-value">{fmt(displayResult.read_mbps)} <small>MB/s</small></div><div className="card-detail">{fmt(displayResult.read_iops, 0)} IOPS</div></div>}
            {displayResult.write_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.io.write')}</div><div className="card-value">{fmt(displayResult.write_mbps)} <small>MB/s</small></div><div className="card-detail">{fmt(displayResult.write_iops, 0)} IOPS</div></div>}
            {displayResult.lat_avg_us != null && <div className="card"><div className="card-label">{t('benchmarks.io.latencyAvg')}</div><div className="card-value">{fmt(displayResult.lat_avg_us, 0)} <small>µs</small></div></div>}
          </div>
          {displayResult.data_points?.length > 0 && <BenchChart points={displayResult.data_points} mode={mode} duration={duration} />}
        </div>
      )}

      {history.length > 0 && (
        <div style={{ background: '#fff', borderRadius: 8, padding: 20, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
            <h3 style={{ margin: 0 }}>{t('benchmarks.history.title')}</h3>
            <button className="btn secondary" onClick={() => setHistory([])}>{t('benchmarks.history.clear')}</button>
          </div>
          <table className="data-table">
            <thead><tr><th>{t('benchmarks.col.readMbps')}</th><th>{t('benchmarks.col.writeMbps')}</th><th>{t('benchmarks.col.readIops')}</th><th>{t('benchmarks.col.writeIops')}</th></tr></thead>
            <tbody>
              {history.map((r: any, i: number) => (
                <tr key={i} onClick={() => setSelectedHistory(r)} style={{ cursor: 'pointer' }}>
                  <td>{fmt(r.read_mbps)}</td>
                  <td>{fmt(r.write_mbps)}</td>
                  <td>{fmt(r.read_iops, 0)}</td>
                  <td>{fmt(r.write_iops, 0)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {selectedHistory && (
        <div onClick={() => setSelectedHistory(null)} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.45)', display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000 }}>
          <div onClick={e => e.stopPropagation()} style={{ background: '#fff', borderRadius: 8, padding: 24, width: '90%', maxWidth: 680, boxShadow: '0 8px 32px rgba(0,0,0,0.2)' }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 16 }}>
              <h3 style={{ margin: 0 }}>{t('benchmarks.modal.ioTitle', { mode: selectedHistory.mode, block: selectedHistory.block_size })}</h3>
              <button className="btn secondary" onClick={() => setSelectedHistory(null)}>{t('common.close')}</button>
            </div>
            <div className="cards" style={{ marginBottom: 16 }}>
              {selectedHistory.read_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.io.read')}</div><div className="card-value">{fmt(selectedHistory.read_mbps)} <small>MB/s</small></div><div className="card-detail">{fmt(selectedHistory.read_iops, 0)} IOPS</div></div>}
              {selectedHistory.write_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.io.write')}</div><div className="card-value">{fmt(selectedHistory.write_mbps)} <small>MB/s</small></div><div className="card-detail">{fmt(selectedHistory.write_iops, 0)} IOPS</div></div>}
              {selectedHistory.lat_avg_us != null && <div className="card"><div className="card-label">{t('benchmarks.io.latencyAvg')}</div><div className="card-value">{fmt(selectedHistory.lat_avg_us, 0)} <small>us</small></div></div>}
            </div>
            {selectedHistory.data_points?.length > 0
              ? <BenchChart points={selectedHistory.data_points} mode={selectedHistory.mode} duration={selectedHistory.duration_s} />
              : <p style={{ color: '#999', fontSize: 13 }}>{t('benchmarks.modal.noTimeSeries')}</p>}
          </div>
        </div>
      )}
        </>
      )}

      {category === 'system' && (
        <>
          <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
            <p style={{ margin: '0 0 12px', color: '#666', fontSize: 13 }}>
              {t('benchmarks.system.intro')}
            </p>
            <div className="form-row">
              <label>{t('benchmarks.system.duration')}
                <input type="number" value={sysDuration} onChange={e => setSysDuration(parseInt(e.target.value))} min={5} max={120} />
              </label>
            </div>

            {sysError && <div className="error-msg">{sysError}</div>}
            {sysProgress && <div className="status-banner info">{sysProgress}</div>}

            <button className="btn primary" onClick={runSystem} disabled={sysRunning} style={{ marginTop: 8 }}>
              {sysRunning ? t('benchmarks.running') : t('benchmarks.system.run')}
            </button>
          </div>

          {sysResult && (
            <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
              <h3 style={{ margin: '0 0 16px' }}>{t('benchmarks.system.resultsTitle')}</h3>

              {sysResult.cpu_single_core && (
                <div style={{ marginBottom: 20 }}>
                  <h4 style={{ margin: '0 0 8px', fontSize: 14, color: '#555' }}>{t('benchmarks.system.cpuSingle')}</h4>
                  <p style={{ margin: '0 0 4px', fontSize: 12, color: '#999' }}>{t('benchmarks.system.cpuSingleDesc')}</p>
                  <div className="cards">
                    <div className="card"><div className="card-label">{t('benchmarks.system.eventsPerSec')}</div><div className="card-value">{fmtInt(sysResult.cpu_single_core.events_per_sec)}</div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.totalEvents')}</div><div className="card-value">{fmtInt(sysResult.cpu_single_core.total_events)}</div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.avgLat')}</div><div className="card-value">{fmt(sysResult.cpu_single_core.latency_avg_ms, 2)} <small>ms</small></div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.p95Lat')}</div><div className="card-value">{fmt(sysResult.cpu_single_core.latency_p95_ms, 2)} <small>ms</small></div></div>
                  </div>
                </div>
              )}

              {sysResult.cpu_multi_core && (
                <div style={{ marginBottom: 20 }}>
                  <h4 style={{ margin: '0 0 8px', fontSize: 14, color: '#555' }}>{t('benchmarks.system.cpuMulti', { threads: sysResult.cpu_multi_core.threads })}</h4>
                  <p style={{ margin: '0 0 4px', fontSize: 12, color: '#999' }}>{t('benchmarks.system.cpuMultiDesc')}</p>
                  <div className="cards">
                    <div className="card"><div className="card-label">{t('benchmarks.system.eventsPerSec')}</div><div className="card-value">{fmtInt(sysResult.cpu_multi_core.events_per_sec)}</div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.totalEvents')}</div><div className="card-value">{fmtInt(sysResult.cpu_multi_core.total_events)}</div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.avgLat')}</div><div className="card-value">{fmt(sysResult.cpu_multi_core.latency_avg_ms, 2)} <small>ms</small></div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.p95Lat')}</div><div className="card-value">{fmt(sysResult.cpu_multi_core.latency_p95_ms, 2)} <small>ms</small></div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.scaling')}</div><div className="card-value">{sysResult.cpu_single_core?.events_per_sec ? fmt(sysResult.cpu_multi_core.events_per_sec / sysResult.cpu_single_core.events_per_sec, 1) : '\u2014'}x</div></div>
                  </div>
                </div>
              )}

              {sysResult.memory && (
                <div>
                  <h4 style={{ margin: '0 0 8px', fontSize: 14, color: '#555' }}>{t('benchmarks.system.memBandwidth')}</h4>
                  <p style={{ margin: '0 0 4px', fontSize: 12, color: '#999' }}>{t('benchmarks.system.memBandwidthDesc')}</p>
                  <div className="cards">
                    <div className="card"><div className="card-label">{t('benchmarks.system.throughput')}</div><div className="card-value">{fmtInt(sysResult.memory.throughput_mbs)} <small>MiB/s</small></div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.opsPerSec')}</div><div className="card-value">{fmtInt(sysResult.memory.ops_per_sec)}</div></div>
                    <div className="card"><div className="card-label">{t('benchmarks.system.totalOps')}</div><div className="card-value">{fmtInt(sysResult.memory.total_ops)}</div></div>
                  </div>
                </div>
              )}
            </div>
          )}

          {sysHistory.length > 0 && (
            <div style={{ background: '#fff', borderRadius: 8, padding: 20, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
                <h3 style={{ margin: 0 }}>{t('benchmarks.history.title')}</h3>
                <button className="btn secondary" onClick={() => setSysHistory([])}>{t('benchmarks.history.clear')}</button>
              </div>
              <table className="data-table">
                <thead><tr><th>{t('benchmarks.col.cpuSingle')}</th><th>{t('benchmarks.col.cpuMulti')}</th><th>{t('benchmarks.system.scaling')}</th><th>{t('benchmarks.col.memMib')}</th></tr></thead>
                <tbody>
                  {sysHistory.map((r: any, i: number) => (
                    <tr key={i} onClick={() => setSysResult(r)} style={{ cursor: 'pointer' }}>
                      <td>{t('benchmarks.system.evPerSec', { value: fmtInt(r.cpu_single_core?.events_per_sec) })}</td>
                      <td>{t('benchmarks.system.evPerSec', { value: fmtInt(r.cpu_multi_core?.events_per_sec) })}</td>
                      <td>{r.cpu_single_core?.events_per_sec ? fmt(r.cpu_multi_core?.events_per_sec / r.cpu_single_core?.events_per_sec, 1) : '\u2014'}x</td>
                      <td>{fmtInt(r.memory?.throughput_mbs)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}

      {category === 'network' && (
        <>
          <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
            <div className="tabs" style={{ marginBottom: 16 }}>
              <button className={`tab${netTestType === 'local' ? ' active' : ''}`} onClick={() => onNetTestTypeChange('local')}>{t('benchmarks.net.tabLocal')}</button>
              <button className={`tab${netTestType === 'external' ? ' active' : ''}`} onClick={() => onNetTestTypeChange('external')}>{t('benchmarks.net.tabExternal')}</button>
            </div>

            {netTestType === 'local' && (
              <div className="form-row">
                <label>{t('benchmarks.net.targetHost')} <input value={netHost} onChange={e => setNetHost(e.target.value)} placeholder="192.168.1.100" /></label>
                <label>{t('benchmarks.net.port')} <input type="number" value={netPort} onChange={e => setNetPort(parseInt(e.target.value))} /></label>
                <label>{t('benchmarks.io.duration')} <input type="number" value={netDuration} onChange={e => setNetDuration(parseInt(e.target.value))} min={5} max={120} /></label>
                <label>{t('benchmarks.net.streams')} <input type="number" value={netStreams} onChange={e => setNetStreams(parseInt(e.target.value))} min={1} max={32} /></label>
                <label>{t('benchmarks.io.mode')}
                  <select value={netMode} onChange={e => setNetMode(e.target.value as any)}>
                    <option value="download">{t('benchmarks.net.download')}</option>
                    <option value="upload">{t('benchmarks.net.upload')}</option>
                  </select>
                </label>
              </div>
            )}

            {netTestType === 'external' && (
              <>
                <div style={{ display: 'flex', gap: 12, marginBottom: 12 }}>
                  <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                    <input type="radio" checked={netAutoServer} onChange={() => setNetAutoServer(true)} /> {t('benchmarks.net.autoSelect')}
                  </label>
                  <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                    <input type="radio" checked={!netAutoServer} onChange={() => { setNetAutoServer(false); if (netExternalServers.length === 0) loadExternalServers(); }} /> {t('benchmarks.net.chooseServer')}
                  </label>
                </div>
                {!netAutoServer && (
                  <>
                    <input value={netServerFilter} onChange={e => setNetServerFilter(e.target.value)} placeholder={t('benchmarks.net.filterServers')} style={{ marginBottom: 8, padding: '6px 10px', border: '1px solid #ddd', borderRadius: 4, width: '100%', fontSize: 14 }} />
                    {netLoadingServers ? <p>{t('benchmarks.net.loadingServers')}</p> : netServerError ? <p style={{ color: '#f44336' }}>{netServerError}</p> : (
                      <select value={netSelectedServerId} onChange={e => setNetSelectedServerId(e.target.value)} style={{ width: '100%', padding: '8px 10px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12 }}>
                        <option value="">{t('benchmarks.net.selectServer')}</option>
                        {netFilteredServers.map((s: any) => <option key={s.id} value={s.id}>{s.label}</option>)}
                      </select>
                    )}
                  </>
                )}
              </>
            )}

            {netError && <div className="error-msg">{netError}</div>}
            {netProgress && <div className="status-banner info">{netProgress}</div>}

            <button className="btn primary" onClick={runNet} disabled={netRunDisabled} style={{ marginTop: 8 }}>
              {netRunning ? t('benchmarks.running') : t('benchmarks.net.run')}
            </button>
          </div>

          {netDisplayResult && (
            <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
              <h3 style={{ margin: '0 0 12px' }}>{netLiveResult ? t('benchmarks.result.live') : t('benchmarks.result.title')}</h3>
              <div className="cards">
                {netDisplayResult.download_mbps != null && (
                  <div className="card"><div className="card-label">{t('benchmarks.net.download')}</div><div className="card-value">{fmt(netDisplayResult.download_mbps)} <small>Mbps</small></div></div>
                )}
                {netDisplayResult.upload_mbps != null && (
                  <div className="card"><div className="card-label">{t('benchmarks.net.upload')}</div><div className="card-value">{fmt(netDisplayResult.upload_mbps)} <small>Mbps</small></div></div>
                )}
                {netDisplayResult.latency_ms != null && (
                  <div className="card"><div className="card-label">{t('benchmarks.net.latency')}</div><div className="card-value">{fmt(netDisplayResult.latency_ms)} <small>ms</small></div></div>
                )}
              </div>
              {netDisplayResult.data_points?.length > 0 && (
                <NetworkTestChart points={netDisplayResult.data_points} duration={netDuration} />
              )}
            </div>
          )}

          {netHistory.length > 0 && (
            <div style={{ background: '#fff', borderRadius: 8, padding: 20, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
                <h3 style={{ margin: 0 }}>{t('benchmarks.history.title')}</h3>
                <button className="btn secondary" onClick={() => setNetHistory([])}>{t('benchmarks.history.clear')}</button>
              </div>
              <table className="data-table">
                <thead><tr><th>{t('benchmarks.net.download')}</th><th>{t('benchmarks.net.upload')}</th><th>{t('benchmarks.net.latency')}</th></tr></thead>
                <tbody>
                  {netHistory.map((r: any, i: number) => (
                    <tr key={i} onClick={() => setNetSelectedHistory(r)} style={{ cursor: 'pointer' }}>
                      <td>{fmt(r.download_mbps)} Mbps</td>
                      <td>{fmt(r.upload_mbps)} Mbps</td>
                      <td>{fmt(r.latency_ms)} ms</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {netSelectedHistory && (
            <div onClick={() => setNetSelectedHistory(null)} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.45)', display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000 }}>
              <div onClick={e => e.stopPropagation()} style={{ background: '#fff', borderRadius: 8, padding: 24, width: '90%', maxWidth: 680, boxShadow: '0 8px 32px rgba(0,0,0,0.2)' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 16 }}>
                  <h3 style={{ margin: 0 }}>{t('benchmarks.modal.netTitle', { type: netSelectedHistory.type })}</h3>
                  <button className="btn secondary" onClick={() => setNetSelectedHistory(null)}>{t('common.close')}</button>
                </div>
                <div className="cards" style={{ marginBottom: 16 }}>
                  {netSelectedHistory.download_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.net.download')}</div><div className="card-value">{fmt(netSelectedHistory.download_mbps)} <small>Mbps</small></div></div>}
                  {netSelectedHistory.upload_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.net.upload')}</div><div className="card-value">{fmt(netSelectedHistory.upload_mbps)} <small>Mbps</small></div></div>}
                  {netSelectedHistory.latency_ms != null && <div className="card"><div className="card-label">{t('benchmarks.net.latency')}</div><div className="card-value">{fmt(netSelectedHistory.latency_ms)} <small>ms</small></div></div>}
                </div>
                {netSelectedHistory.data_points?.length > 0
                  ? <NetworkTestChart points={netSelectedHistory.data_points} duration={netSelectedHistory.duration_s} />
                  : <p style={{ color: '#999', fontSize: 13 }}>{t('benchmarks.modal.noTimeSeries')}</p>}
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}
