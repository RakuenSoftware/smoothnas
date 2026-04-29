import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import NetworkTestChart from '../../charts/NetworkTestChart';

export default function NetworkTests() {
  const { t } = useI18n();
  const [testType, setTestType] = useState<'local' | 'external'>('local');
  const [mode, setMode] = useState<'download' | 'upload'>('download');
  const [host, setHost] = useState('');
  const [port, setPort] = useState(5201);
  const [duration, setDuration] = useState(15);
  const [streams, setStreams] = useState(1);
  const [autoServer, setAutoServer] = useState(true);
  const [externalServers, setExternalServers] = useState<any[]>([]);
  const [selectedServerId, setSelectedServerId] = useState('');
  const [serverFilter, setServerFilter] = useState('');
  const [loadingServers, setLoadingServers] = useState(false);
  const [serverError, setServerError] = useState('');
  const [running, setRunning] = useState(false);
  const [error, setError] = useState('');
  const [progress, setProgress] = useState('');
  const [result, setResult] = useState<any>(null);
  const [liveResult, setLiveResult] = useState<any>(null);
  const [history, setHistory] = useState<any[]>([]);
  const [selectedHistory, setSelectedHistory] = useState<any>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => () => { if (pollRef.current) clearInterval(pollRef.current); }, []);

  const filteredServers = serverFilter
    ? externalServers.filter(s => (s.label || '').toLowerCase().includes(serverFilter.toLowerCase()))
    : externalServers;

  const runDisabled = running || (testType === 'local' ? !host.trim() : !autoServer && !selectedServerId);

  function onTestTypeChange(type: 'local' | 'external') {
    setTestType(type);
    setError('');
    setResult(null);
    setLiveResult(null);
    if (type === 'external' && externalServers.length === 0 && !loadingServers) loadExternalServers();
  }

  function loadExternalServers() {
    setLoadingServers(true);
    setServerError('');
    api.getExternalSpeedtestServers().then(servers => {
      setExternalServers(servers);
      setLoadingServers(false);
    }).catch(e => {
      setServerError(extractError(e, t('benchmarks.net.error.loadServers')));
      setLoadingServers(false);
    });
  }

  function run() {
    setError('');
    setResult(null);
    setLiveResult(null);
    setProgress(testType === 'local' ? t('benchmarks.net.startingLocal') : t('benchmarks.net.startingExternal'));
    setRunning(true);
    const req: any = { type: testType, duration };
    if (testType === 'local') { req.host = host.trim(); req.port = port; req.streams = streams; req.mode = mode; }
    else if (!autoServer && selectedServerId) req.server_id = selectedServerId;
    api.runNetworkTest(req).then((res: any) => { pollJob(res.job_id); })
      .catch(e => { setError(extractError(e, t('benchmarks.net.failed'))); stopRunning(); });
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
          setError(job.error || t('benchmarks.net.failed'));
          stopRunning();
        }
      }).catch(() => {
        pending = false;
        if (pollRef.current) clearInterval(pollRef.current);
        setError(t('benchmarks.net.lostConn'));
        stopRunning();
      });
    }, 1000);
  }

  function stopRunning() { setRunning(false); setProgress(''); setLiveResult(null); }

  function fmt(n: number, decimals = 1): string {
    if (n == null || n === 0) return '—';
    return n.toFixed(decimals);
  }

  const displayResult = liveResult || result;

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('networkTests.title')}</h1>
        <p className="subtitle">{t('networkTests.subtitle')}</p>
      </div>

      <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
        <div className="tabs" style={{ marginBottom: 16 }}>
          <button className={`tab${testType === 'local' ? ' active' : ''}`} onClick={() => onTestTypeChange('local')}>{t('benchmarks.net.tabLocal')}</button>
          <button className={`tab${testType === 'external' ? ' active' : ''}`} onClick={() => onTestTypeChange('external')}>{t('benchmarks.net.tabExternal')}</button>
        </div>

        {testType === 'local' && (
          <div className="form-row">
            <label>{t('benchmarks.net.targetHost')} <input value={host} onChange={e => setHost(e.target.value)} placeholder="192.168.1.100" /></label>
            <label>{t('benchmarks.net.port')} <input type="number" value={port} onChange={e => setPort(parseInt(e.target.value))} /></label>
            <label>{t('benchmarks.io.duration')} <input type="number" value={duration} onChange={e => setDuration(parseInt(e.target.value))} min={5} max={120} /></label>
            <label>{t('benchmarks.net.streams')} <input type="number" value={streams} onChange={e => setStreams(parseInt(e.target.value))} min={1} max={32} /></label>
            <label>{t('benchmarks.io.mode')}
              <select value={mode} onChange={e => setMode(e.target.value as any)}>
                <option value="download">{t('benchmarks.net.download')}</option>
                <option value="upload">{t('benchmarks.net.upload')}</option>
              </select>
            </label>
          </div>
        )}

        {testType === 'external' && (
          <>
            <div style={{ display: 'flex', gap: 12, marginBottom: 12 }}>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                <input type="radio" checked={autoServer} onChange={() => setAutoServer(true)} /> {t('benchmarks.net.autoSelect')}
              </label>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                <input type="radio" checked={!autoServer} onChange={() => { setAutoServer(false); if (externalServers.length === 0) loadExternalServers(); }} /> {t('benchmarks.net.chooseServer')}
              </label>
            </div>
            {!autoServer && (
              <>
                <input value={serverFilter} onChange={e => setServerFilter(e.target.value)} placeholder={t('benchmarks.net.filterServers')} style={{ marginBottom: 8, padding: '6px 10px', border: '1px solid #ddd', borderRadius: 4, width: '100%', fontSize: 14 }} />
                {loadingServers ? <p>{t('benchmarks.net.loadingServers')}</p> : serverError ? <p style={{ color: '#f44336' }}>{serverError}</p> : (
                  <select value={selectedServerId} onChange={e => setSelectedServerId(e.target.value)} style={{ width: '100%', padding: '8px 10px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12 }}>
                    <option value="">{t('benchmarks.net.selectServer')}</option>
                    {filteredServers.map((s: any) => <option key={s.id} value={s.id}>{s.label}</option>)}
                  </select>
                )}
              </>
            )}
          </>
        )}

        {error && <div className="error-msg">{error}</div>}
        {progress && <div className="status-banner info">{progress}</div>}

        <button className="btn primary" onClick={run} disabled={runDisabled} style={{ marginTop: 8 }}>
          {running ? t('benchmarks.running') : t('benchmarks.net.run')}
        </button>
      </div>

      {displayResult && (
        <div style={{ background: '#fff', borderRadius: 8, padding: 20, marginBottom: 24, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
          <h3 style={{ margin: '0 0 12px' }}>{liveResult ? t('benchmarks.result.live') : t('benchmarks.result.title')}</h3>
          <div className="cards">
            {displayResult.download_mbps != null && (
              <div className="card"><div className="card-label">{t('benchmarks.net.download')}</div><div className="card-value">{fmt(displayResult.download_mbps)} <small>Mbps</small></div></div>
            )}
            {displayResult.upload_mbps != null && (
              <div className="card"><div className="card-label">{t('benchmarks.net.upload')}</div><div className="card-value">{fmt(displayResult.upload_mbps)} <small>Mbps</small></div></div>
            )}
            {displayResult.latency_ms != null && (
              <div className="card"><div className="card-label">{t('benchmarks.net.latency')}</div><div className="card-value">{fmt(displayResult.latency_ms)} <small>ms</small></div></div>
            )}
          </div>
          {displayResult.data_points?.length > 0 && (
            <NetworkTestChart points={displayResult.data_points} duration={duration} />
          )}
        </div>
      )}

      {history.length > 0 && (
        <div style={{ background: '#fff', borderRadius: 8, padding: 20, boxShadow: '0 1px 3px rgba(0,0,0,0.08)' }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
            <h3 style={{ margin: 0 }}>{t('benchmarks.history.title')}</h3>
            <button className="btn secondary" onClick={() => setHistory([])}>{t('benchmarks.history.clear')}</button>
          </div>
          <table className="data-table">
            <thead><tr><th>{t('benchmarks.net.download')}</th><th>{t('benchmarks.net.upload')}</th><th>{t('benchmarks.net.latency')}</th></tr></thead>
            <tbody>
              {history.map((r: any, i: number) => (
                <tr key={i} onClick={() => setSelectedHistory(r)} style={{ cursor: 'pointer' }}>
                  <td>{fmt(r.download_mbps)} Mbps</td>
                  <td>{fmt(r.upload_mbps)} Mbps</td>
                  <td>{fmt(r.latency_ms)} ms</td>
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
              <h3 style={{ margin: 0 }}>{t('benchmarks.modal.netTitle', { type: selectedHistory.type })}</h3>
              <button className="btn secondary" onClick={() => setSelectedHistory(null)}>{t('common.close')}</button>
            </div>
            <div className="cards" style={{ marginBottom: 16 }}>
              {selectedHistory.download_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.net.download')}</div><div className="card-value">{fmt(selectedHistory.download_mbps)} <small>Mbps</small></div></div>}
              {selectedHistory.upload_mbps != null && <div className="card"><div className="card-label">{t('benchmarks.net.upload')}</div><div className="card-value">{fmt(selectedHistory.upload_mbps)} <small>Mbps</small></div></div>}
              {selectedHistory.latency_ms != null && <div className="card"><div className="card-label">{t('benchmarks.net.latency')}</div><div className="card-value">{fmt(selectedHistory.latency_ms)} <small>ms</small></div></div>}
            </div>
            {selectedHistory.data_points?.length > 0
              ? <NetworkTestChart points={selectedHistory.data_points} duration={selectedHistory.duration_s} />
              : <p style={{ color: '#999', fontSize: 13 }}>{t('benchmarks.modal.noTimeSeries')}</p>}
          </div>
        </div>
      )}
    </div>
  );
}
