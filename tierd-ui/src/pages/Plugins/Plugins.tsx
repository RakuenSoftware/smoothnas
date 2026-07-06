import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';
import { pluginCatalogRepositories } from '../../data/pluginCatalog';

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
  containerRefs?: PluginContainerRef[];
  containerUpdateAvailable?: boolean;
  installedAt: string;
  updatedAt: string;
};

type PluginContainerRef = {
  service: string;
  name: string;
  imageRef: string;
  resolvedRef?: string;
  digest?: string;
  updatedAt: string;
};

type PluginUpdate = {
  version: string;
  manifestYaml: string;
  releaseTag: string;
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
  const [updates, setUpdates] = useState<Record<string, PluginUpdate>>({});
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
        const rows = resp?.plugins ?? [];
        setPlugins(rows);
        setLoading(false);
        loadAvailableUpdates(rows);
      })
      .catch((e: any) => {
        setError(extractError(e, t('plugins.error.list')));
        setLoading(false);
      });
  }

  function loadAvailableUpdates(rows: PluginRow[]) {
    setUpdates({});
    if (rows.length === 0) return;
    const installed = new Map(rows.map(p => [p.name, p]));
    Promise.all(pluginCatalogRepositories.map(repo =>
      api.getPluginCatalogLatest(repo.repo).catch(() => null)
    )).then(releases => {
      const next: Record<string, PluginUpdate> = {};
      for (const release of releases) {
        if (!release) continue;
        for (const item of release.manifests ?? []) {
          const name = item.manifest?.metadata?.name;
          const version = item.manifest?.metadata?.version;
          const current = name ? installed.get(name) : undefined;
          if (!current || !version || !isVersionGreater(version, current.version)) continue;
          const existing = next[name];
          if (!existing || isVersionGreater(version, existing.version)) {
            next[name] = {
              version,
              manifestYaml: item.manifestYaml,
              releaseTag: release.tagName,
            };
          }
        }
      }
      setUpdates(next);
    });
  }

  function lifecycle(name: string, verb: 'start' | 'stop' | 'restart' | 'materialise' | 'refresh-containers') {
    setBusyName(name);
    setError('');
    const call =
      verb === 'start'       ? api.startPlugin :
      verb === 'stop'        ? api.stopPlugin :
      verb === 'restart'     ? api.restartPlugin :
      verb === 'refresh-containers' ? api.refreshPluginContainers :
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

  function updatePlugin(name: string, manifest: string) {
    setBusyName(name);
    setError('');
    api.updatePlugin(name, { manifest })
      .then(() => refresh())
      .catch((e: any) => setError(extractError(e, t('plugins.error.update'))))
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
              update={updates[p.name]}
              busy={busyName === p.name}
              onLifecycle={(verb) => lifecycle(p.name, verb)}
              onUpdate={(manifest) => updatePlugin(p.name, manifest)}
              onUninstall={() => setConfirmUninstall(p.name)}
              onConfigure={() => navigate(`/plugins/manage/${p.name}`)}
              onOpen={() => navigate(`/plugins/${p.name}`)}
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
  update,
  busy,
  onLifecycle,
  onUpdate,
  onUninstall,
  onConfigure,
  onOpen,
}: {
  plugin: PluginRow;
  update?: PluginUpdate;
  busy: boolean;
  onLifecycle: (verb: 'start' | 'stop' | 'restart' | 'materialise' | 'refresh-containers') => void;
  onUpdate: (manifest: string) => void;
  onUninstall: () => void;
  onConfigure: () => void;
  onOpen: () => void;
}) {
  const { t } = useI18n();
  const isRunning = plugin.state === 'running';
  const isStopped = plugin.state === 'stopped' || plugin.state === 'installed' || plugin.state === 'failed';
  const needsMaterialise = plugin.state === 'installed';
  const containerRefs = plugin.containerRefs ?? [];
  const visibleContainerRefs = containerRefs.slice(0, 6);
  const remainingContainerRefs = containerRefs.length - visibleContainerRefs.length;
  // Show the container's version. A compose plugin has no meaningful manifest
  // version (0.0.0), so fall back to the distinct image tag(s) of its containers.
  const imageVersions = Array.from(
    new Set(containerRefs.map(r => imageTag(r.resolvedRef || r.imageRef)).filter(Boolean)),
  );
  const displayVersion =
    plugin.version && plugin.version !== '0.0.0' ? plugin.version : imageVersions.join(', ');

  return (
    <div className="plugin-card">
      <div className="plugin-card-head">
        <div>
          <h3>{plugin.name}</h3>
          <div className="plugin-card-meta">
            {displayVersion && (
              <span>{t('plugins.label.version')}: {displayVersion}</span>
            )}
            {update && (
              <>
                <span>·</span>
                <span>{t('plugins.label.updateAvailable')}: {update.version}</span>
              </>
            )}
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
        {containerRefs.length > 0 && (
          <>
            <dt>{t('plugins.label.containers')}</dt>
            <dd>
              <div className="plugin-card-ref-list">
                {visibleContainerRefs.map(ref => {
                  const version = containerRefVersion(ref);
                  return (
                    <div
                      key={`${ref.service}/${ref.name}`}
                      className="mono truncate"
                      title={`${containerRefName(ref)}: ${version}`}
                    >
                      <span className="muted">{containerRefName(ref)}:</span> {shortContainerRef(version)}
                    </div>
                  );
                })}
                {remainingContainerRefs > 0 && (
                  <div className="muted">
                    {t('plugins.label.moreContainers').replace('{count}', String(remainingContainerRefs))}
                  </div>
                )}
              </div>
            </dd>
          </>
        )}
        <dt>{t('plugins.label.installed')}</dt>
        <dd>{plugin.installedAt}</dd>
      </dl>

      <div className="plugin-card-actions">
        {update && (
          <button
            className="btn primary"
            disabled={busy}
            onClick={() => onUpdate(update.manifestYaml)}
            title={update.releaseTag}
          >
            {t('plugins.action.update')}
          </button>
        )}
        {plugin.containerUpdateAvailable && (
          <button
            className="btn"
            disabled={busy || needsMaterialise}
            onClick={() => onLifecycle('refresh-containers')}
          >
            {t('plugins.action.refreshContainers')}
          </button>
        )}
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
          onClick={onOpen}
          disabled={!isRunning}
          title={isRunning ? '' : t('plugins.embed.notRunning.title')}
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

function isVersionGreater(candidate: string, current: string): boolean {
  return compareSemver(candidate, current) > 0;
}

function containerRefName(ref: PluginContainerRef): string {
  return [ref.service, ref.name].filter(Boolean).join('/');
}

function containerRefVersion(ref: PluginContainerRef): string {
  return ref.resolvedRef || ref.imageRef || '';
}

function shortContainerRef(ref: string): string {
  return ref.replace(/sha256:([0-9a-f]{12})[0-9a-f]+/i, 'sha256:$1...');
}

// imageTag extracts the container's version from an image ref: the tag (after
// the final ':' that isn't part of a registry host:port), or a short digest for
// a digest-pinned ref. An untagged image is implicitly "latest".
function imageTag(ref: string): string {
  if (!ref) return '';
  const at = ref.indexOf('@');
  if (at >= 0) return shortContainerRef(ref.slice(at + 1));
  const slash = ref.lastIndexOf('/');
  const colon = ref.lastIndexOf(':');
  if (colon > slash) return ref.slice(colon + 1);
  return 'latest';
}

function compareSemver(a: string, b: string): number {
  const pa = parseSemver(a);
  const pb = parseSemver(b);
  for (let i = 0; i < 3; i++) {
    if (pa.nums[i] !== pb.nums[i]) return pa.nums[i] - pb.nums[i];
  }
  if (pa.suffix === pb.suffix) return 0;
  if (!pa.suffix) return 1;
  if (!pb.suffix) return -1;
  return pa.suffix.localeCompare(pb.suffix);
}

function parseSemver(v: string): { nums: number[]; suffix: string } {
  const match = v.match(/^(\d+)\.(\d+)\.(\d+)(.*)$/);
  if (!match) return { nums: [0, 0, 0], suffix: v };
  return {
    nums: [Number(match[1]), Number(match[2]), Number(match[3])],
    suffix: match[4] ?? '',
  };
}
