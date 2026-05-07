import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

// Detail mirrors the JSON shape POST /api/plugins/<name> returns.
// Loose typing — the backend's PluginRecord adds fields over time;
// the detail page only renders a subset.
type Detail = {
  plugin: any;
  instances: any[];
  volumes: any[];
  ports: any[];
  config: { plugin_name: string; key: string; value: string }[];
  manifest: string;
};

type Tab = 'overview' | 'logs' | 'config' | 'profiles' | 'danger';

const tabs: Tab[] = ['overview', 'logs', 'config', 'profiles', 'danger'];

function stateClass(state: string): string {
  switch (state) {
    case 'running':                       return 'state-healthy';
    case 'pulling':
    case 'creating':
    case 'starting':                      return 'state-provisioning';
    case 'degraded':                      return 'state-degraded';
    case 'failed':                        return 'state-error';
  }
  return 'state-unmounted';
}

function stateKey(state: string): string {
  const known = new Set(['installed', 'pulling', 'creating', 'starting', 'running', 'stopped', 'failed', 'degraded']);
  return known.has(state) ? `plugins.state.${state}` : 'plugins.state.unknown';
}

export default function PluginDetail() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const { name } = useParams<{ name: string }>();

  const [tab, setTab] = useState<Tab>('overview');
  const [detail, setDetail] = useState<Detail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [confirmUninstall, setConfirmUninstall] = useState(false);

  useEffect(() => {
    if (!name) return;
    refresh();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  function refresh() {
    if (!name) return;
    setLoading(true);
    setError('');
    api.getPlugin(name)
      .then(setDetail)
      .catch(e => setError(extractError(e, t('plugins.error.list'))))
      .finally(() => setLoading(false));
  }

  function uninstall() {
    if (!name) return;
    setConfirmUninstall(false);
    api.uninstallPlugin(name)
      .then(() => navigate('/plugins'))
      .catch(e => setError(extractError(e, t('plugins.error.uninstall'))));
  }

  if (loading) {
    return <div className="page"><Spinner loading={true} /></div>;
  }
  if (!detail || !name) {
    return (
      <div className="page">
        <h1>{t('plugins.detail.notFound')}</h1>
        {error && <div className="alert error">{error}</div>}
        <Link to="/plugins">{t('plugins.detail.backToList')}</Link>
      </div>
    );
  }

  return (
    <div className="page">
      <div className="plugin-detail-header">
        <div>
          <h1>{detail.plugin.name}</h1>
          <div className="plugin-detail-meta">
            <span>{t('plugins.label.version')}: {detail.plugin.version}</span>
            <span>·</span>
            <span>{detail.plugin.artifactType}</span>
            <span>·</span>
            <span className={`state-badge ${stateClass(detail.plugin.state)}`}>
              {t(stateKey(detail.plugin.state))}
            </span>
          </div>
        </div>
        <Link to="/plugins" className="btn">
          {t('plugins.detail.backToList')}
        </Link>
      </div>

      {error && <div className="alert error">{error}</div>}

      <nav className="plugin-detail-tabs">
        {tabs.map(name => (
          <button
            key={name}
            className={`${tab === name ? 'active' : ''}${name === 'danger' ? ' danger' : ''}`}
            onClick={() => setTab(name)}
          >
            {t(`plugins.detail.tab.${name}`)}
          </button>
        ))}
      </nav>

      <div className="plugin-detail-pane">
        {tab === 'overview' && <OverviewTab detail={detail} />}
        {tab === 'logs' && <LogsTab name={name} state={detail.plugin.state} />}
        {tab === 'config' && (
          <ConfigTab
            name={name}
            initial={Object.fromEntries(detail.config.map(c => [c.key, c.value]))}
            manifest={detail.manifest}
            onSaved={refresh}
          />
        )}
        {tab === 'profiles' && <ProfilesTab profiles={detail.plugin.resolvedProfiles ?? []} />}
        {tab === 'danger' && (
          <DangerTab onUninstall={() => setConfirmUninstall(true)} />
        )}
      </div>

      <ConfirmDialog
        visible={confirmUninstall}
        title={t('plugins.confirm.uninstall.title')}
        message={t('plugins.detail.danger.uninstall.body')}
        confirmText={t('plugins.confirm.uninstall.confirm')}
        confirmClass="btn danger"
        onConfirm={uninstall}
        onCancel={() => setConfirmUninstall(false)}
      />
    </div>
  );
}

function OverviewTab({ detail }: { detail: Detail }) {
  const { t } = useI18n();
  const p = detail.plugin;
  return (
    <>
      <h2>{t('plugins.detail.overview.heading')}</h2>
      <dl className="plugin-kv-list">
        <dt>{t('plugins.detail.overview.state')}</dt>
        <dd>
          <span className={`state-badge ${stateClass(p.state)}`}>
            {t(stateKey(p.state))}
          </span>
        </dd>
        <dt>{t('plugins.detail.overview.artifact')}</dt>
        <dd>{p.artifactType}</dd>
        {p.imageRef && (
          <>
            <dt>{t('plugins.detail.overview.image')}</dt>
            <dd className="mono">{p.imageRef}</dd>
          </>
        )}
        {p.distroSummary && (
          <>
            <dt>{t('plugins.detail.overview.distro')}</dt>
            <dd>{p.distroSummary}</dd>
          </>
        )}
        <dt>{t('plugins.detail.overview.instances')}</dt>
        <dd>
          {p.instanceCount}
          {p.instanceConfigurable && ` (${t('plugins.detail.overview.instanceConfigurable')})`}
        </dd>
        <dt>{t('plugins.detail.overview.installed')}</dt>
        <dd>{p.installedAt}</dd>
        <dt>{t('plugins.detail.overview.updated')}</dt>
        <dd>{p.updatedAt}</dd>
      </dl>

      <div className="plugin-detail-section">
        <h3>{t('plugins.detail.overview.volumes')}</h3>
        {detail.volumes.length === 0 ? (
          <p>{t('plugins.label.none')}</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>{t('plugins.detail.col.name')}</th>
                <th>{t('plugins.detail.col.mode')}</th>
                <th>{t('plugins.detail.col.tier')}</th>
                <th>{t('plugins.detail.col.bind')}</th>
                <th>{t('plugins.detail.col.hostPaths')}</th>
              </tr>
            </thead>
            <tbody>
              {detail.volumes.map(v => (
                <tr key={v.Name}>
                  <td className="mono">{v.Name}</td>
                  <td>{v.Mode}{v.Slot ? ` (${v.Slot})` : ''}</td>
                  <td className="mono">{v.TierPool || '—'}</td>
                  <td className="mono">{v.BindPath}</td>
                  <td className="mono">
                    {Object.entries(v.Paths || {}).map(([inst, p]) => (
                      <div key={inst}>{inst}: {String(p) || '—'}</div>
                    ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="plugin-detail-section">
        <h3>{t('plugins.detail.overview.ports')}</h3>
        {detail.ports.length === 0 ? (
          <p>{t('plugins.label.none')}</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>{t('plugins.detail.col.name')}</th>
                <th>{t('plugins.detail.col.port')}</th>
                <th>{t('plugins.detail.col.protocol')}</th>
                <th>{t('plugins.detail.col.expose')}</th>
              </tr>
            </thead>
            <tbody>
              {detail.ports.map(p => (
                <tr key={p.Name}>
                  <td className="mono">{p.Name}</td>
                  <td>{p.ContainerPort}</td>
                  <td>{p.Protocol}</td>
                  <td>{p.Expose ? t('plugins.detail.col.yes') : t('plugins.detail.col.no')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {detail.instances.length > 0 && (
        <div className="plugin-detail-section">
          <h3>{t('plugins.detail.overview.instances')}</h3>
          <table>
            <thead>
              <tr>
                <th>{t('plugins.detail.col.instance')}</th>
                <th>{t('plugins.detail.col.state')}</th>
                <th>{t('plugins.detail.col.container')}</th>
                <th>{t('plugins.detail.col.bridge')}</th>
              </tr>
            </thead>
            <tbody>
              {detail.instances.map(i => (
                <tr key={i.Instance}>
                  <td>{i.Instance}</td>
                  <td>
                    <span className={`state-badge ${stateClass(i.State)}`}>
                      {t(stateKey(i.State))}
                    </span>
                  </td>
                  <td className="mono">{i.ContainerID ? i.ContainerID.slice(0, 12) : '—'}</td>
                  <td className="mono">{i.BridgeIP || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function LogsTab({ name, state }: { name: string; state: string }) {
  const { t } = useI18n();
  // Local buffer of accumulated SSE lines. The render uses join('\n')
  // so the <pre> stays manageable for moderate log volumes — for
  // very-long-running sessions a virtualised list would be wiser, but
  // 480px tall fixed view is fine for v1.
  const [lines, setLines] = useState<string[]>([]);
  const [paused, setPaused] = useState(false);
  const [streamState, setStreamState] = useState<'connecting' | 'connected' | 'disconnected'>('connecting');
  const viewRef = useRef<HTMLPreElement>(null);
  const pausedRef = useRef(false);
  const bufferRef = useRef<string[]>([]);

  // Keep a ref-mirror of paused so the EventSource handler reads the
  // current value without re-subscribing each toggle.
  useEffect(() => { pausedRef.current = paused; }, [paused]);

  useEffect(() => {
    if (state !== 'running') {
      setStreamState('disconnected');
      return;
    }
    setLines([]);
    bufferRef.current = [];
    setStreamState('connecting');
    const url = `/api/plugins/${name}/logs`;
    const es = new EventSource(url, { withCredentials: true });

    es.onopen = () => setStreamState('connected');
    es.onerror = () => setStreamState('disconnected');
    es.onmessage = (ev) => {
      // The backend converts the daemon's multiplexed log stream into
      // SSE by escaping embedded newlines as "\ndata: "; EventSource
      // re-joins with "\n" so each message can already span lines.
      const msg = ev.data as string;
      if (pausedRef.current) {
        bufferRef.current.push(msg);
        return;
      }
      setLines(prev => {
        const next = prev.concat(msg);
        return next.length > 5000 ? next.slice(-5000) : next;
      });
    };
    return () => es.close();
  }, [name, state]);

  // Auto-scroll to tail when not paused and the operator hasn't
  // scrolled up. Detection: if scrollTop is within 80px of the
  // bottom we treat it as "tracking the tail".
  useEffect(() => {
    if (paused || !viewRef.current) return;
    const el = viewRef.current;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (atBottom) el.scrollTop = el.scrollHeight;
  }, [lines, paused]);

  function togglePause() {
    if (paused) {
      // Resume: drain the buffered messages into visible state.
      const drained = bufferRef.current;
      bufferRef.current = [];
      setLines(prev => {
        const next = prev.concat(drained);
        return next.length > 5000 ? next.slice(-5000) : next;
      });
    }
    setPaused(p => !p);
  }

  function clearBuffer() {
    setLines([]);
    bufferRef.current = [];
  }

  function download() {
    const blob = new Blob([lines.join('\n')], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${name}.log`;
    a.click();
    URL.revokeObjectURL(url);
  }

  if (state !== 'running') {
    return (
      <>
        <h2>{t('plugins.detail.logs.heading')}</h2>
        <p className="subtitle">{t('plugins.detail.logs.subtitle')}</p>
        <p>{t('plugins.detail.logs.notRunning')}</p>
      </>
    );
  }

  const stateLabel =
    streamState === 'connected'   ? t('plugins.detail.logs.connected') :
    streamState === 'connecting'  ? t('plugins.detail.logs.connecting') :
                                    t('plugins.detail.logs.disconnected');

  return (
    <>
      <h2>{t('plugins.detail.logs.heading')}</h2>
      <p className="subtitle">{t('plugins.detail.logs.subtitle')}</p>
      <div className="plugin-log-controls">
        <span className={`stream-state ${streamState}`}>● {stateLabel}</span>
        <button className="btn" onClick={togglePause}>
          {paused ? t('plugins.detail.logs.action.resume') : t('plugins.detail.logs.action.pause')}
        </button>
        <button className="btn" onClick={clearBuffer}>
          {t('plugins.detail.logs.action.clear')}
        </button>
        <button className="btn" onClick={download} disabled={lines.length === 0}>
          {t('plugins.detail.logs.action.download')}
        </button>
      </div>
      <pre ref={viewRef} className="plugin-log-view">
        {lines.join('\n')}
      </pre>
    </>
  );
}

function ConfigTab({
  name, initial, manifest, onSaved,
}: {
  name: string;
  initial: Record<string, string>;
  manifest: string;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  // Pull the manifest's config[] field metadata so the form knows
  // which keys are secret. Re-parse via the backend rather than
  // re-export the type — keeps validation truth in one place.
  const [fields, setFields] = useState<{ key: string; description?: string; secret?: boolean }[]>([]);
  const [values, setValues] = useState<Record<string, string>>({ ...initial });
  const [busy, setBusy] = useState(false);
  const [restartBusy, setRestartBusy] = useState(false);
  const [error, setError] = useState('');
  const [restartNeeded, setRestartNeeded] = useState(false);

  useEffect(() => {
    if (!manifest) return;
    api.parsePlugin({ manifest })
      .then(resp => {
        const parsed = resp.manifest as { config?: { key: string; description?: string; secret?: boolean }[] };
        setFields(parsed.config ?? []);
      })
      .catch(() => { /* leave fields empty; the keys still render */ });
  }, [manifest]);

  function save() {
    setBusy(true);
    setError('');
    api.updatePluginConfig(name, values)
      .then(() => {
        setRestartNeeded(true);
        onSaved();
      })
      .catch(e => setError(extractError(e, t('plugins.detail.config.error.save'))))
      .finally(() => setBusy(false));
  }

  function restart() {
    setRestartBusy(true);
    setError('');
    api.restartPlugin(name)
      .then(() => setRestartNeeded(false))
      .catch(e => setError(extractError(e, t('plugins.detail.config.error.restart'))))
      .finally(() => setRestartBusy(false));
  }

  // Render a row per (manifest field) so secret-ness is honoured. If
  // a key is in initial but not the manifest (operator added one
  // out-of-band), show it as a plain text field.
  const renderedKeys = new Set(fields.map(f => f.key));
  const orphans = Object.keys(values).filter(k => !renderedKeys.has(k));

  return (
    <>
      <h2>{t('plugins.detail.config.heading')}</h2>
      <p className="subtitle">{t('plugins.detail.config.description')}</p>

      {restartNeeded && (
        <div className="wizard-success">{t('plugins.detail.config.restartNeeded')}</div>
      )}
      {error && <div className="wizard-error"><pre>{error}</pre></div>}

      {fields.length === 0 && orphans.length === 0 ? (
        <p>{t('plugins.install.config.noFields')}</p>
      ) : (
        <div className="plugin-config-form">
          {fields.map(f => (
            <div key={f.key} className="config-field">
              <label htmlFor={`cfg-${f.key}`}>{f.key}</label>
              <input
                id={`cfg-${f.key}`}
                type={f.secret ? 'password' : 'text'}
                value={values[f.key] ?? ''}
                onChange={e => setValues({ ...values, [f.key]: e.target.value })}
              />
              {f.description && <span className="help">{f.description}</span>}
            </div>
          ))}
          {orphans.map(k => (
            <div key={k} className="config-field">
              <label htmlFor={`cfg-${k}`}>{k}</label>
              <input
                id={`cfg-${k}`}
                type="text"
                value={values[k]}
                onChange={e => setValues({ ...values, [k]: e.target.value })}
              />
            </div>
          ))}

          <div className="config-actions">
            <button className="btn primary" onClick={save} disabled={busy}>
              {t('plugins.detail.config.action.save')}
            </button>
            <button
              className="btn"
              onClick={restart}
              disabled={restartBusy || !restartNeeded}
            >
              {t('plugins.detail.config.action.restart')}
            </button>
          </div>
        </div>
      )}
    </>
  );
}

function ProfilesTab({ profiles }: { profiles: string[] }) {
  const { t } = useI18n();
  if (profiles.length === 0) {
    return (
      <>
        <h2>{t('plugins.detail.profiles.heading')}</h2>
        <p>{t('plugins.detail.profiles.empty')}</p>
      </>
    );
  }
  return (
    <>
      <h2>{t('plugins.detail.profiles.heading')}</h2>
      <p className="subtitle">{t('plugins.detail.profiles.description')}</p>
      <span className="chip-row">
        {profiles.map(name => (
          <span key={name} className="chip">{name}</span>
        ))}
      </span>
    </>
  );
}

function DangerTab({ onUninstall }: { onUninstall: () => void }) {
  const { t } = useI18n();
  return (
    <>
      <h2>{t('plugins.detail.danger.heading')}</h2>
      <div className="danger-card">
        <h3>{t('plugins.detail.danger.uninstall.heading')}</h3>
        <p>{t('plugins.detail.danger.uninstall.body')}</p>
        <button className="btn danger" onClick={onUninstall}>
          {t('plugins.action.uninstall')}
        </button>
      </div>
    </>
  );
}
