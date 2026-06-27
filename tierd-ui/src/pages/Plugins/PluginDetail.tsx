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
  services: any[];
  instances: any[];
  volumes: any[];
  ports: any[];
  config: any[];
  containerRefs: any[];
  manifest: string;
};

function normalizeDetail(raw: any): Detail {
  return {
    plugin: raw?.plugin ?? {},
    services: Array.isArray(raw?.services) ? raw.services : [],
    instances: Array.isArray(raw?.instances) ? raw.instances : [],
    volumes: Array.isArray(raw?.volumes) ? raw.volumes : [],
    ports: Array.isArray(raw?.ports) ? raw.ports : [],
    config: Array.isArray(raw?.config) ? raw.config : [],
    containerRefs: Array.isArray(raw?.containerRefs) ? raw.containerRefs : [],
    manifest: raw?.manifest ?? '',
  };
}

type GPUInfo = {
  id: string;
  vendor: string;
  label: string;
  devicePath: string;
};

type Tab = 'overview' | 'models' | 'logs' | 'config' | 'instances' | 'profiles' | 'danger';

// instancesTabVisible decides whether to render the Instances tab on
// the detail header. Hidden for plugins that are single-instance and
// not configurable — i.e. the parent doc's "small plugin" case where
// scaling makes no sense and the tab would add visual noise.
function instancesTabVisible(plugin: any): boolean {
  return plugin?.instanceCount > 1 || !!plugin?.instanceConfigurable;
}

function modelsTabVisible(detail: Detail): boolean {
  return !!modelVolume(detail) && detail.config.some(c => configKey(c) === 'MODEL_PATH');
}

function modelVolume(detail: Detail): any | null {
  return (detail.volumes ?? []).find(v => v.BindPath === '/models' || v.Name === 'models') ?? null;
}

function configKey(row: any): string {
  return row?.key ?? row?.Key ?? '';
}

function configValue(row: any): string {
  return row?.value ?? row?.Value ?? '';
}

function refName(row: any): string {
  return row?.name ?? row?.Name ?? '';
}

function refService(row: any): string {
  return row?.service ?? row?.Service ?? '';
}

function refImage(row: any): string {
  return row?.imageRef ?? row?.ImageRef ?? '';
}

function refResolved(row: any): string {
  return row?.resolvedRef ?? row?.ResolvedRef ?? '';
}

function configMap(rows: any[]): Record<string, string> {
  return Object.fromEntries((rows ?? []).map(c => [configKey(c), configValue(c)]).filter(([k]) => !!k));
}

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
      .then(resp => setDetail(normalizeDetail(resp)))
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
        {(['overview', 'models', 'logs', 'config', 'instances', 'profiles', 'danger'] as Tab[])
          .filter(n => n !== 'models' || modelsTabVisible(detail))
          .filter(n => n !== 'instances' || instancesTabVisible(detail.plugin))
          .map(n => (
            <button
              key={n}
              className={`${tab === n ? 'active' : ''}${n === 'danger' ? ' danger' : ''}`}
              onClick={() => setTab(n)}
            >
              {t(`plugins.detail.tab.${n}`)}
            </button>
          ))}
      </nav>

      <div className="plugin-detail-pane">
        {tab === 'overview' && <OverviewTab detail={detail} name={name!} onChanged={refresh} />}
        {tab === 'models' && <ModelsTab name={name} detail={detail} onInstalled={refresh} />}
        {tab === 'logs' && <LogsTab name={name} state={detail.plugin.state} />}
        {tab === 'config' && (
          <ConfigTab
            name={name}
            initial={configMap(detail.config)}
            manifest={detail.manifest}
            onSaved={refresh}
          />
        )}
        {tab === 'instances' && (
          <InstancesTab
            name={name}
            initial={{
              count: detail.plugin.instanceCount,
              configurable: !!detail.plugin.instanceConfigurable,
              instances: detail.instances,
            }}
            onScaled={refresh}
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

function OverviewTab({ detail, name, onChanged }: { detail: Detail; name: string; onChanged: () => void }) {
  const { t } = useI18n();
  const p = detail.plugin;
  const currentPin: string =
    (detail.services ?? []).map((s: any) => s.pinnedImage).find((v: any) => !!v) ?? '';
  const [image, setImage] = useState(currentPin);
  const [saving, setSaving] = useState(false);
  const [pinMsg, setPinMsg] = useState('');
  const savePin = (val: string) => {
    setSaving(true);
    setPinMsg(t('plugins.detail.image.applying'));
    api
      .setPluginImage(name, val)
      .then(res => {
        setPinMsg(t(res.applied ? 'plugins.detail.image.applied' : 'plugins.detail.image.saved'));
        onChanged();
      })
      .catch(e => setPinMsg(extractError(e, t('plugins.detail.image.error'))))
      .finally(() => setSaving(false));
  };
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
        {detail.containerRefs.length > 0 && (
          <>
            <dt>{t('plugins.detail.overview.containerRefs')}</dt>
            <dd>
              <div className="plugin-ref-list">
                {detail.containerRefs.map(ref => (
                  <div key={`${refService(ref)}/${refName(ref)}`} className="mono truncate">
                    {[refService(ref), refName(ref)].filter(Boolean).join('/')}: {refResolved(ref) || refImage(ref)}
                  </div>
                ))}
              </div>
            </dd>
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
        <h3>{t('plugins.detail.image.heading')}</h3>
        <p className="muted">{t('plugins.detail.image.help')}</p>
        <div className="plugin-image-override">
          <input
            type="text"
            className="mono"
            value={image}
            placeholder={t('plugins.detail.image.placeholder')}
            onChange={e => setImage(e.target.value)}
          />
          <button className="btn" disabled={saving} onClick={() => savePin(image.trim())}>
            {t('plugins.detail.image.apply')}
          </button>
          {currentPin && (
            <button
              className="btn secondary"
              disabled={saving}
              onClick={() => {
                setImage('');
                savePin('');
              }}
            >
              {t('plugins.detail.image.clear')}
            </button>
          )}
        </div>
        {pinMsg && <p className="muted">{pinMsg}</p>}
      </div>

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

type JobStatus = {
  id: string;
  status: 'running' | 'completed' | 'failed';
  progress?: string;
  error?: string;
  result?: any;
};

function ModelsTab({
  name, detail, onInstalled,
}: {
  name: string;
  detail: Detail;
  onInstalled: () => void;
}) {
  const { t } = useI18n();
  const currentConfig = configMap(detail.config);
  const volume = modelVolume(detail);
  const [url, setUrl] = useState('');
  const [filename, setFilename] = useState('');
  const [sha256, setSha256] = useState('');
  const [temperature, setTemperature] = useState(currentConfig.LLAMA_ARG_TEMP || '0.8');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [jobId, setJobId] = useState('');
  const [job, setJob] = useState<JobStatus | null>(null);

  useEffect(() => {
    if (!jobId) return;
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | null = null;
    const poll = () => {
      api.getJobStatus(jobId)
        .then((next: JobStatus) => {
          if (cancelled) return;
          setJob(next);
          if (next.status === 'completed') {
            onInstalled();
            if (timer) clearInterval(timer);
          }
          if (next.status === 'failed' && timer) clearInterval(timer);
        })
        .catch(e => {
          if (!cancelled) setError(extractError(e, t('plugins.detail.models.error.job')));
          if (timer) clearInterval(timer);
        });
    };
    poll();
    timer = setInterval(poll, 2000);
    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
    };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobId]);

  function installModel() {
    if (!url.trim()) {
      setError(t('plugins.detail.models.error.url'));
      return;
    }
    let parsedTemperature: number | undefined;
    if (temperature.trim()) {
      parsedTemperature = Number(temperature);
      if (!Number.isFinite(parsedTemperature) || parsedTemperature < 0 || parsedTemperature > 2) {
        setError(t('plugins.detail.models.error.temperature'));
        return;
      }
    }
    setBusy(true);
    setError('');
    setJob(null);
    api.installPluginModel(name, {
      url: url.trim(),
      ...(filename.trim() ? { filename: filename.trim() } : {}),
      ...(sha256.trim() ? { sha256: sha256.trim() } : {}),
      ...(parsedTemperature !== undefined ? { temperature: parsedTemperature } : {}),
      start: true,
    })
      .then(resp => setJobId(resp.jobId))
      .catch(e => setError(extractError(e, t('plugins.detail.models.error.install'))))
      .finally(() => setBusy(false));
  }

  const running = job?.status === 'running';
  const currentModel = currentConfig.MODEL_PATH || t('plugins.label.none');
  const currentTemperature = currentConfig.LLAMA_ARG_TEMP || '0.8';
  const hostPath = volume?.Paths?.[1] ?? Object.values(volume?.Paths ?? {})[0] ?? '';

  return (
    <>
      <h2>{t('plugins.detail.models.heading')}</h2>
      <p className="subtitle">{t('plugins.detail.models.description')}</p>

      <dl className="plugin-kv-list">
        <dt>{t('plugins.detail.models.current')}</dt>
        <dd className="mono">{currentModel}</dd>
        <dt>{t('plugins.detail.models.temperature.current')}</dt>
        <dd className="mono">{currentTemperature}</dd>
        {hostPath && (
          <>
            <dt>{t('plugins.detail.models.volume')}</dt>
            <dd className="mono">{String(hostPath)}</dd>
          </>
        )}
      </dl>

      <div className="plugin-model-form">
        <label htmlFor="model-url">{t('plugins.detail.models.url')}</label>
        <input
          id="model-url"
          type="url"
          value={url}
          onChange={e => setUrl(e.target.value)}
          placeholder="https://..."
          disabled={busy || running}
        />

        <label htmlFor="model-filename">{t('plugins.detail.models.filename')}</label>
        <input
          id="model-filename"
          type="text"
          value={filename}
          onChange={e => setFilename(e.target.value)}
          placeholder={t('plugins.detail.models.filename.placeholder')}
          disabled={busy || running}
        />

        <label htmlFor="model-sha">{t('plugins.detail.models.sha256')}</label>
        <input
          id="model-sha"
          type="text"
          value={sha256}
          onChange={e => setSha256(e.target.value)}
          placeholder="sha256:..."
          disabled={busy || running}
        />

        <label htmlFor="model-temperature">{t('plugins.detail.models.temperature')}</label>
        <input
          id="model-temperature"
          type="number"
          min="0"
          max="2"
          step="0.01"
          value={temperature}
          onChange={e => setTemperature(e.target.value)}
          disabled={busy || running}
        />

        <div className="plugin-model-actions">
          <button className="btn primary" onClick={installModel} disabled={busy || running}>
            {busy || running
              ? t('plugins.detail.models.action.installing')
              : t('plugins.detail.models.action.install')}
          </button>
        </div>
      </div>

      {error && <div className="alert error"><pre>{error}</pre></div>}
      {job && (
        <div className={`plugin-model-job ${job.status}`}>
          <strong>{t(`plugins.detail.models.job.${job.status}`)}</strong>
          {job.progress && <span>{job.progress}</span>}
          {job.error && <pre>{job.error}</pre>}
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
  const [fields, setFields] = useState<ConfigFieldMeta[]>([]);
  const [values, setValues] = useState<Record<string, string>>({ ...initial });
  const [gpus, setGpus] = useState<GPUInfo[]>([]);
  const [busy, setBusy] = useState(false);
  const [restartBusy, setRestartBusy] = useState(false);
  const [error, setError] = useState('');
  const [restartNeeded, setRestartNeeded] = useState(false);

  useEffect(() => {
    if (!manifest) return;
    api.parsePlugin({ manifest })
      .then(resp => {
        const parsed = resp.manifest as { config?: ConfigFieldMeta[] };
        setFields(parsed.config ?? []);
      })
      .catch(() => { /* leave fields empty; the keys still render */ });
    api.getPluginGPUs().then((resp: any) => {
      setGpus(normalizeGPUs(resp?.gpus));
    }).catch(() => { /* GPU fields keep the automatic/current option */ });
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
              <label htmlFor={`cfg-${f.key}`}>{f.label || f.key}</label>
              <PluginConfigInput
                id={`cfg-${f.key}`}
                field={f}
                value={values[f.key] ?? ''}
                gpus={gpus}
                onChange={value => setValues({ ...values, [f.key]: value })}
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

type ConfigFieldMeta = {
  key: string;
  type?: string;
  label?: string;
  description?: string;
  secret?: boolean;
  gpuVendor?: string;
  options?: { value: string; label?: string }[];
  min?: string;
  max?: string;
  step?: string;
  unit?: string;
};

function PluginConfigInput({
  id,
  field,
  value,
  gpus,
  onChange,
}: {
  id: string;
  field: ConfigFieldMeta;
  value: string;
  gpus: GPUInfo[];
  onChange: (value: string) => void;
}) {
  const { t } = useI18n();
  if (field.type === 'select') {
    return (
      <select
        id={id}
        value={value}
        onChange={e => onChange(e.target.value)}
      >
        {(field.options ?? []).map(opt => (
          <option key={opt.value} value={opt.value}>{opt.label || opt.value}</option>
        ))}
      </select>
    );
  }
  if (field.type === 'boolean') {
    return (
      <input
        id={id}
        type="checkbox"
        checked={value === 'true' || value === '1'}
        onChange={e => onChange(e.target.checked ? 'true' : 'false')}
      />
    );
  }
  if (field.type === 'gpu') {
    const options = gpuOptions(field, value, gpus);
    return (
      <select
        id={id}
        value={value}
        onChange={e => onChange(e.target.value)}
      >
        <option value="">{t('plugins.install.config.gpuAutomatic')}</option>
        {options.map(gpu => (
          <option key={gpu.devicePath} value={gpu.devicePath}>
            {gpu.label} ({gpu.devicePath})
          </option>
        ))}
      </select>
    );
  }
  return (
    <div className="config-input-with-unit">
      <input
        id={id}
        type={field.secret ? 'password' : field.type === 'number' ? 'number' : 'text'}
        value={value}
        onChange={e => onChange(e.target.value)}
        min={field.min}
        max={field.max}
        step={field.step}
      />
      {field.unit && <span>{field.unit}</span>}
    </div>
  );
}

function gpuOptions(field: ConfigFieldMeta, value: string, gpus: GPUInfo[]): GPUInfo[] {
  const vendor = (field.gpuVendor ?? '').toLowerCase();
  const out = gpus.filter(gpu => gpuMatchesField(field, gpu.vendor));
  if (value && !out.some(gpu => gpu.devicePath === value)) {
    out.push({
      id: value,
      vendor: field.gpuVendor ?? '',
      label: value,
      devicePath: value,
    });
  }
  return out;
}

function normalizeGPUs(rows: any): GPUInfo[] {
  return (Array.isArray(rows) ? rows : []).map((g: any) => ({
    id: g.id ?? g.devicePath,
    vendor: g.vendor ?? '',
    label: g.label ?? g.name ?? g.devicePath,
    devicePath: g.devicePath ?? '',
  })).filter((g: GPUInfo) => !!g.devicePath);
}

function gpuMatchesField(field: { gpuVendor?: string }, gpuVendor: string): boolean {
  const vendor = (field.gpuVendor ?? '').toLowerCase();
  const gpu = gpuVendor.toLowerCase();
  return !vendor || gpu === vendor || (vendor === 'amd' && gpu === 'intel');
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

// InstancesTab renders the per-instance state table and (for
// configurable plugins) the scale slider. Slider commits go through a
// confirm dialog that enumerates which instance numbers will be
// removed for scale-down operations — operators have asked for that
// because uninstalling instance N also blows its workspace dir.
//
// The component re-fetches its own data (instead of relying on the
// parent's detail payload) on mount and after every successful scale
// so the table reflects the live state without a parent-level
// roundtrip.
function InstancesTab({
  name,
  initial,
  onScaled,
}: {
  name: string;
  initial: { count: number; configurable: boolean; instances: any[] };
  onScaled: () => void;
}) {
  const { t } = useI18n();
  const [count, setCount] = useState(initial.count);
  const [configurable, setConfigurable] = useState(initial.configurable);
  const [instances, setInstances] = useState(initial.instances);
  const [target, setTarget] = useState(initial.count);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [pendingScale, setPendingScale] = useState<{ from: number; to: number; removed: number[] } | null>(null);

  function refreshInstances() {
    api.listPluginInstances(name)
      .then(d => {
        setCount(d.count);
        setConfigurable(d.configurable);
        setInstances(d.instances ?? []);
        setTarget(d.count);
      })
      .catch(e => setError(extractError(e, t('plugins.detail.instances.error.list'))));
  }

  function requestScale(to: number) {
    if (to === count) return;
    if (to < count) {
      const removed: number[] = [];
      for (let i = count; i > to; i--) removed.push(i);
      setPendingScale({ from: count, to, removed });
      return;
    }
    // Scale-up needs no confirmation — no destructive side effects.
    commitScale(to);
  }

  function commitScale(to: number) {
    setBusy(true);
    setError('');
    setPendingScale(null);
    api.scalePluginInstances(name, to)
      .then(() => {
        refreshInstances();
        onScaled();
      })
      .catch(e => {
        setError(extractError(e, t('plugins.detail.instances.error.scale')));
        // Reset slider to the (still-current) count on failure.
        setTarget(count);
      })
      .finally(() => setBusy(false));
  }

  // Min slider is 2 for a multi-instance plugin (we don't allow
  // crossing the 1↔N boundary; the backend rejects with
  // plugins.scale.boundary). Single-instance configurable plugins
  // would have min 1, but those are uncommon and the slider gets a
  // clearer story by clamping to ≥2 visibly.
  const min = count > 1 ? 2 : 1;
  const max = Math.max(min, 16); // arbitrary v1 cap

  return (
    <>
      <h2>{t('plugins.detail.instances.heading')}</h2>
      <p className="subtitle">{t('plugins.detail.instances.description')}</p>

      {error && <div className="alert error"><pre>{error}</pre></div>}

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
          {instances.map(i => (
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

      {configurable && (
        <div className="plugin-detail-section">
          <h3>{t('plugins.detail.instances.scale.heading')}</h3>
          <p className="subtitle">{t('plugins.detail.instances.scale.description')}</p>
          <div className="instance-scale-controls">
            <input
              type="range"
              min={min}
              max={max}
              step={1}
              value={target}
              onChange={e => setTarget(parseInt(e.target.value, 10))}
              disabled={busy}
            />
            <span className="instance-scale-value">
              {t('plugins.detail.instances.scale.target', { count: target })}
            </span>
            <button
              className="btn primary"
              onClick={() => requestScale(target)}
              disabled={busy || target === count}
            >
              {target > count
                ? t('plugins.detail.instances.scale.action.up')
                : target < count
                  ? t('plugins.detail.instances.scale.action.down')
                  : t('plugins.detail.instances.scale.action.same')}
            </button>
          </div>
        </div>
      )}

      <ConfirmDialog
        visible={!!pendingScale}
        title={t('plugins.detail.instances.confirm.title')}
        message={
          pendingScale
            ? t('plugins.detail.instances.confirm.body', {
                from: pendingScale.from,
                to: pendingScale.to,
                removed: pendingScale.removed.join(', '),
              })
            : ''
        }
        confirmText={t('plugins.detail.instances.confirm.confirm')}
        confirmClass="btn danger"
        onConfirm={() => pendingScale && commitScale(pendingScale.to)}
        onCancel={() => { setPendingScale(null); setTarget(count); }}
      />
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
