import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

// PluginRow mirrors the JSON shape the phase-06a backend returns
// from GET /api/plugins under the "plugins" key. Kept loose because
// the detail page reads more fields than this list view needs.
type PluginRow = {
  name: string;
  version: string;
  state: string;
  artifactType: string;
  imageRef?: string;
  distroSummary?: string;
  instanceCount: number;
  instanceConfigurable: boolean;
  resolvedProfiles: string[];
  installedAt: string;
  updatedAt: string;
};

// stateClass maps the aggregate plugin.state string to one of the
// CSS state-* badge classes from styles.scss. Falls back to the
// existing state-unmounted gray when the daemon hands back something
// we don't recognise (forward-compat).
function stateClass(state: string): string {
  switch (state) {
    case 'running':                       return 'state-healthy';
    case 'pulling':
    case 'creating':
    case 'starting':                      return 'state-provisioning';
    case 'degraded':                      return 'state-degraded';
    case 'failed':                        return 'state-error';
    case 'stopped':
    case 'installed':                     return 'state-unmounted';
  }
  return 'state-unmounted';
}

// stateKey is the i18n key for a plugin state. Unknown states fall
// back to plugins.state.unknown.
function stateKey(state: string): string {
  const known = new Set([
    'installed', 'pulling', 'creating', 'starting',
    'running', 'stopped', 'failed', 'degraded',
  ]);
  return known.has(state) ? `plugins.state.${state}` : 'plugins.state.unknown';
}

export default function Plugins() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const [loading, setLoading] = useState(true);
  const [plugins, setPlugins] = useState<PluginRow[]>([]);
  const [error, setError] = useState('');
  // busyName tracks per-card lifecycle action in flight so the
  // Start/Stop buttons can show a tiny spinner and disable
  // themselves without blocking other cards.
  const [busyName, setBusyName] = useState<string | null>(null);
  const [confirmUninstall, setConfirmUninstall] = useState<string | null>(null);

  useEffect(() => {
    refresh();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function refresh() {
    setLoading(true);
    setError('');
    api.listPlugins()
      .then(resp => {
        setPlugins(resp?.plugins ?? []);
        setLoading(false);
      })
      .catch((e: any) => {
        setError(extractError(e, t('plugins.error.list')));
        setLoading(false);
      });
  }

  function lifecycle(name: string, verb: 'start' | 'stop' | 'restart' | 'materialise') {
    setBusyName(name);
    setError('');
    const call =
      verb === 'start'       ? api.startPlugin :
      verb === 'stop'        ? api.stopPlugin :
      verb === 'restart'     ? api.restartPlugin :
                               api.materialisePlugin;
    call(name)
      .then(() => refresh())
      .catch((e: any) => setError(extractError(e, t('plugins.error.lifecycle'))))
      .finally(() => setBusyName(null));
  }

  function uninstall(name: string) {
    setConfirmUninstall(null);
    setBusyName(name);
    setError('');
    api.uninstallPlugin(name)
      .then(() => refresh())
      .catch((e: any) => setError(extractError(e, t('plugins.error.uninstall'))))
      .finally(() => setBusyName(null));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('plugins.title')}</h1>
        <p className="subtitle">{t('plugins.subtitle')}</p>
        <div className="page-header-actions">
          <button className="btn" onClick={refresh} disabled={loading}>
            {t('plugins.action.refresh')}
          </button>
          <button
            className="btn primary"
            onClick={() => navigate('/plugins/install')}
          >
            {t('plugins.action.install')}
          </button>
        </div>
      </div>

      {error && <div className="alert error">{error}</div>}

      {loading ? (
        <Spinner loading={true} />
      ) : plugins.length === 0 ? (
        <div className="empty-state">
          <h2>{t('plugins.empty.title')}</h2>
          <p>{t('plugins.empty.body')}</p>
        </div>
      ) : (
        <div className="plugin-grid">
          {plugins.map(p => (
            <PluginCard
              key={p.name}
              plugin={p}
              busy={busyName === p.name}
              onLifecycle={(verb) => lifecycle(p.name, verb)}
              onUninstall={() => setConfirmUninstall(p.name)}
              onConfigure={() => navigate(`/plugins/manage/${p.name}`)}
            />
          ))}
        </div>
      )}

      <ConfirmDialog
        visible={confirmUninstall !== null}
        title={t('plugins.confirm.uninstall.title')}
        message={t('plugins.confirm.uninstall.message')}
        confirmText={t('plugins.confirm.uninstall.confirm')}
        confirmClass="btn danger"
        onConfirm={() => confirmUninstall && uninstall(confirmUninstall)}
        onCancel={() => setConfirmUninstall(null)}
      />
    </div>
  );
}

// PluginCard renders a single plugin's read-only summary plus the
// lifecycle action buttons. Open / Configure are disabled in 6b
// (the iframe page lands in phase 7, the config form in 6d) but
// shown so the operator can see the surface that's coming.
function PluginCard({
  plugin,
  busy,
  onLifecycle,
  onUninstall,
  onConfigure,
}: {
  plugin: PluginRow;
  busy: boolean;
  onLifecycle: (verb: 'start' | 'stop' | 'restart' | 'materialise') => void;
  onUninstall: () => void;
  onConfigure: () => void;
}) {
  const { t } = useI18n();
  const isRunning = plugin.state === 'running';
  const isStopped = plugin.state === 'stopped' || plugin.state === 'installed' || plugin.state === 'failed';
  const needsMaterialise = plugin.state === 'installed';

  return (
    <div className="plugin-card">
      <div className="plugin-card-head">
        <div>
          <h3>{plugin.name}</h3>
          <div className="plugin-card-meta">
            <span>{t('plugins.label.version')}: {plugin.version}</span>
            <span>·</span>
            <span>{plugin.artifactType}</span>
            {plugin.instanceCount > 1 && (
              <>
                <span>·</span>
                <span>{plugin.instanceCount} {t('plugins.label.instances').toLowerCase()}</span>
              </>
            )}
          </div>
        </div>
        <span className={`state-badge ${stateClass(plugin.state)}`}>
          {t(stateKey(plugin.state))}
        </span>
      </div>

      <dl className="plugin-card-fields">
        <dt>{t('plugins.label.profiles')}</dt>
        <dd>
          {plugin.resolvedProfiles && plugin.resolvedProfiles.length > 0 ? (
            <span className="chip-row">
              {plugin.resolvedProfiles.map(name => (
                <span key={name} className="chip">{name}</span>
              ))}
            </span>
          ) : (
            <span className="muted">{t('plugins.label.none')}</span>
          )}
        </dd>
        {plugin.imageRef && (
          <>
            <dt>{t('plugins.label.image')}</dt>
            <dd className="mono truncate">{plugin.imageRef}</dd>
          </>
        )}
        {plugin.distroSummary && (
          <>
            <dt>{t('plugins.label.distro')}</dt>
            <dd>{plugin.distroSummary}</dd>
          </>
        )}
        <dt>{t('plugins.label.installed')}</dt>
        <dd>{plugin.installedAt}</dd>
      </dl>

      <div className="plugin-card-actions">
        {needsMaterialise ? (
          <button
            className="btn primary"
            disabled={busy}
            onClick={() => onLifecycle('materialise')}
          >
            {t('plugins.action.materialise')}
          </button>
        ) : isStopped ? (
          <button
            className="btn primary"
            disabled={busy}
            onClick={() => onLifecycle('start')}
          >
            {t('plugins.action.start')}
          </button>
        ) : (
          <button
            className="btn"
            disabled={busy}
            onClick={() => onLifecycle('stop')}
          >
            {t('plugins.action.stop')}
          </button>
        )}
        {isRunning && (
          <button
            className="btn"
            disabled={busy}
            onClick={() => onLifecycle('restart')}
          >
            {t('plugins.action.restart')}
          </button>
        )}
        <button
          className="btn"
          onClick={onConfigure}
        >
          {t('plugins.action.configure')}
        </button>
        <button
          className="btn"
          disabled
          title={t('plugins.placeholder.openSoon')}
        >
          {t('plugins.action.open')}
        </button>
        <button
          className="btn danger"
          disabled={busy}
          onClick={onUninstall}
        >
          {t('plugins.action.uninstall')}
        </button>
      </div>
    </div>
  );
}
