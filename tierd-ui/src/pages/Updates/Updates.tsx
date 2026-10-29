import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

// Updates are plain apt: unattended-upgrades installs Debian security fixes and
// everything published to the Smooth* archive as soon as it appears. This page
// shows what is available, lets the operator check or install now (the run
// goes on in its own systemd unit, so it survives tierd restarting when the
// smoothnas package upgrades itself), shows the run's output live, and shows
// when updates are failing. Some updates only take effect after a restart,
// which the operator starts from here.
interface RebootStatus {
  required: boolean;
  reasons: string[] | null;
}

interface UpdateStatus {
  running: boolean;
  available: { name: string; installed: string; available: string }[];
  last_run: {
    action: string;
    started_at: string;
    finished_at?: string;
    exit_code?: number;
    errors: string[];
  } | null;
  reboot: RebootStatus;
}

const POLL_MS = 2000;

export default function Updates() {
  const { t } = useI18n();
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [restarting, setRestarting] = useState(false);
  const [log, setLog] = useState('');
  const [showLog, setShowLog] = useState(false);
  const offsetRef = useRef(0);
  const logRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    loadData();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // While a run is in progress, follow its log and status. Requests fail
  // briefly when the run upgrades and restarts tierd; keep polling through it.
  useEffect(() => {
    if (!status?.running) return;
    setShowLog(true);
    const id = setInterval(() => { pollLog(); refreshStatus(); }, POLL_MS);
    return () => clearInterval(id);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status?.running]);

  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [log]);

  function refreshStatus() {
    return api.getUpdateStatus()
      .then((s: UpdateStatus) => { setStatus(s); setError(''); })
      .catch(() => {});
  }

  function loadData() {
    setLoading(true);
    setError('');
    offsetRef.current = 0;
    setLog('');
    pollLog();
    api.getUpdateStatus()
      .then((s: UpdateStatus) => setStatus(s))
      .catch(e => setError(extractError(e, t('updates.error.status'))))
      .finally(() => setLoading(false));
  }

  function pollLog() {
    api.getUpdateLog(offsetRef.current)
      .then((r: { text: string; next_offset: number }) => {
        if (r.next_offset < offsetRef.current) {
          setLog(r.text);
        } else if (r.text) {
          setLog(prev => prev + r.text);
        }
        offsetRef.current = r.next_offset;
      })
      .catch(() => {});
  }

  function start(action: 'check' | 'install') {
    setError('');
    const call = action === 'check' ? api.checkUpdates() : api.installUpdates();
    call
      .then(() => {
        offsetRef.current = 0;
        setLog('');
        setShowLog(true);
        setStatus(s => s ? { ...s, running: true } : s);
      })
      .catch(e => setError(extractError(e, t('updates.error.start'))));
  }

  function restart() {
    if (!window.confirm(t('updates.confirm.restart'))) return;
    setError('');
    api.reboot()
      .then(() => setRestarting(true))
      .catch(e => setError(extractError(e, t('updates.error.restart'))));
  }

  const last = status?.last_run;
  const lastFailed = !!last && !status?.running && last.exit_code !== undefined && last.exit_code !== 0;
  const available = status?.available ?? [];

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('updates.title')}</h1>
        <p className="subtitle">{t('updates.subtitle')}</p>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      <div className="section">
        <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{t('updates.label.intro')}</p>
        {error && <div className="error-msg">{error}</div>}
        <Spinner loading={loading} />

        {status && (
          <>
            {status.running ? (
              <div style={{ background: '#e0f2fe', borderRadius: 8, padding: 16, marginBottom: 12 }}>
                <strong>{t(last?.action === 'check' ? 'updates.label.checking' : 'updates.label.installing')}</strong>
              </div>
            ) : lastFailed ? (
              <div className="error-msg" style={{ marginBottom: 12 }}>
                <strong>{t(last!.action === 'check' ? 'updates.label.checkFailed' : 'updates.label.installFailed', { time: last!.finished_at || last!.started_at })}</strong>
                {last!.errors.length > 0 && (
                  <ul style={{ margin: '8px 0 0' }}>
                    {last!.errors.map((e, i) => <li key={i} style={{ fontFamily: 'monospace', fontSize: 12 }}>{e}</li>)}
                  </ul>
                )}
              </div>
            ) : available.length === 0 ? (
              <p style={{ color: '#16a34a', marginBottom: 12 }}>
                {t('updates.label.upToDate')}
                {last?.finished_at && <span style={{ color: '#666' }}> {t('updates.label.lastChecked', { time: last.finished_at })}</span>}
              </p>
            ) : null}

            {available.length > 0 && !status.running && (
              <div style={{ marginBottom: 12 }}>
                <p style={{ margin: '0 0 8px' }}><strong>{t('updates.label.available', { count: available.length })}</strong></p>
                <table className="data-table" style={{ width: '100%' }}>
                  <thead>
                    <tr>
                      <th>{t('updates.col.package')}</th>
                      <th>{t('updates.col.installed')}</th>
                      <th>{t('updates.col.available')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {available.map(p => (
                      <tr key={p.name}><td>{p.name}</td><td>{p.installed}</td><td>{p.available}</td></tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 16 }}>
              <button className="btn secondary" onClick={() => start('check')} disabled={status.running}>
                {t('updates.button.check')}
              </button>
              <button className="btn primary" onClick={() => start('install')} disabled={status.running}>
                {t('updates.button.install')}
              </button>
              {log && !status.running && (
                <button className="btn secondary" onClick={() => setShowLog(v => !v)}>
                  {t(showLog ? 'updates.button.hideLog' : 'updates.button.showLog')}
                </button>
              )}
            </div>

            {showLog && log && (
              <pre ref={logRef} style={{ maxHeight: 360, overflow: 'auto', background: '#111', color: '#ddd', padding: 12, borderRadius: 6, fontSize: 12, whiteSpace: 'pre-wrap', marginBottom: 16 }}>
                {log}
              </pre>
            )}
          </>
        )}

        {restarting ? (
          <div className="success">{t('updates.label.restarting')}</div>
        ) : status?.reboot.required ? (
          <div style={{ background: '#fef3c7', borderRadius: 8, padding: 16, marginBottom: 12 }}>
            <p style={{ margin: '0 0 8px' }}><strong>{t('updates.label.restartNeeded')}</strong></p>
            <ul style={{ margin: 0 }}>
              {(status.reboot.reasons ?? []).map(r => <li key={r}>{r}</li>)}
            </ul>
          </div>
        ) : status && (
          <p style={{ color: '#16a34a', marginBottom: 12 }}>{t('updates.label.noRestart')}</p>
        )}
        <button className={`btn ${status?.reboot.required ? 'primary' : 'secondary'}`} onClick={restart} disabled={restarting || status?.running}>
          {t('updates.button.restart')}
        </button>
      </div>
    </div>
  );
}
