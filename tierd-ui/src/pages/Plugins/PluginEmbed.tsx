import { useEffect, useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

// PluginEmbed renders a plugin's own UI inside an iframe served
// through the SmoothNAS nginx reverse-proxy at /plugins/<name>/.
//
// Trailing slash matters: `/plugins/foo` (no slash) is the React
// route this component owns; `/plugins/foo/` (with slash) is what
// nginx proxies to the container. The iframe's src always carries
// the trailing slash plus whatever embed.path the manifest declared
// (default `/`).

type Detail = {
  plugin: {
    name: string;
    version: string;
    state: string;
  };
  manifest: string;
};

type EmbedConfig = {
  path: string;
  auth: string;
};

// extractEmbed reads ui.embed from the manifest YAML the backend
// returned in the detail response. Pulled into a helper so the
// shape of "we don't have js-yaml in the bundle" parsing stays
// in one place — we use a tolerant regex rather than dragging in
// a YAML parser just to read two fields.
function extractEmbed(manifest: string): EmbedConfig | null {
  // Look for an embed block under ui:.
  const ui = manifest.match(/(^|\n)ui:\s*\n([\s\S]*?)(?=\n[a-zA-Z]|$)/);
  if (!ui) return null;
  const body = ui[2];
  const pathMatch = body.match(/embed:[\s\S]*?\n\s*path:\s*([^\n]+)/);
  const authMatch = body.match(/embed:[\s\S]*?\n\s*auth:\s*([^\n]+)/);
  if (!pathMatch && !authMatch) return null;
  return {
    path: (pathMatch?.[1] ?? '/').trim(),
    auth: (authMatch?.[1] ?? 'none').trim(),
  };
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

export default function PluginEmbed() {
  const { t } = useI18n();
  const { name } = useParams<{ name: string }>();
  const [loading, setLoading] = useState(true);
  const [detail, setDetail] = useState<Detail | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    if (!name) return;
    setLoading(true);
    api.getPlugin(name)
      .then(setDetail)
      .catch(e => setError(extractError(e, t('plugins.error.list'))))
      .finally(() => setLoading(false));
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  const embed = useMemo(
    () => detail ? extractEmbed(detail.manifest) : null,
    [detail],
  );
  const iframeSrc = useMemo(() => {
    if (!name || !embed) return '';
    // Normalise the embed path: ensure a single leading slash and
    // a trailing slash so nginx's location ^~ /plugins/<name>/ can
    // claim the request.
    const path = embed.path.startsWith('/') ? embed.path : `/${embed.path}`;
    return `/plugins/${name}${path}`;
  }, [name, embed]);

  if (loading) {
    return <div className="page"><Spinner loading={true} /></div>;
  }
  if (!name || !detail) {
    return (
      <div className="page">
        {error && <div className="alert error">{error}</div>}
        <Link to="/plugins">{t('plugins.detail.backToList')}</Link>
      </div>
    );
  }

  return (
    <div className="page plugin-embed-page">
      <div className="plugin-embed-header">
        <div>
          <h1>{detail.plugin.name}</h1>
          <div className="plugin-embed-meta">
            <span>{t('plugins.label.version')}: {detail.plugin.version}</span>
            <span>·</span>
            <span className={`state-badge ${stateClass(detail.plugin.state)}`}>
              {t(stateKey(detail.plugin.state))}
            </span>
          </div>
        </div>
        <div className="plugin-embed-actions">
          {embed && iframeSrc && (
            <a className="btn" href={iframeSrc} target="_blank" rel="noreferrer noopener">
              {t('plugins.embed.openInTab')}
            </a>
          )}
          <Link to={`/plugins/manage/${name}`} className="btn">
            {t('plugins.embed.manage')}
          </Link>
        </div>
      </div>

      {!embed ? (
        <div className="plugin-embed-error">
          <h2>{t('plugins.embed.noUI.title')}</h2>
          <p>{t('plugins.embed.noUI.body')}</p>
        </div>
      ) : detail.plugin.state !== 'running' ? (
        <div className="plugin-embed-error">
          <h2>{t('plugins.embed.notRunning.title')}</h2>
          <p>{t('plugins.embed.notRunning.body')}</p>
          <Link to={`/plugins/manage/${name}`} className="btn primary">
            {t('plugins.embed.manage')}
          </Link>
        </div>
      ) : (
        <iframe
          className="plugin-embed-frame"
          src={iframeSrc}
          title={detail.plugin.name}
          sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups"
          referrerPolicy="no-referrer"
        />
      )}
    </div>
  );
}
