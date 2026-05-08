import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import { pluginCatalog, type CatalogEntry } from '../../data/pluginCatalog';

// ParsedManifest matches the JSON shape POST /api/plugins/parse
// returns. Loose typing — the backend validates and the wizard
// only reads a small subset.
type ParsedManifest = {
  apiVersion: string;
  kind: string;
  metadata: { name: string; version: string; description?: string };
  artifact: {
    type: string;
    image?: string;
    digest?: string;
    distro?: string;
    release?: string;
    arch?: string;
    packages?: string[];
    setup?: string[];
  };
  container: { command?: string[]; restartPolicy?: string };
  instances?: { count?: number; configurable?: boolean };
  volumes?: ManifestVolume[];
  ports?: { name: string; port: number; protocol: string; expose: boolean }[];
  ui?: { embed?: { path?: string; auth?: string } };
  profiles?: string[];
  config?: ManifestConfigField[];
};

type ManifestVolume = {
  name: string;
  mode: string;       // 'tier-bound' | 'flat'
  slot?: string;      // present when tier-bound
  bind: string;
  perInstance?: boolean;
  minSize?: string;
};

type ManifestConfigField = {
  key: string;
  type: string;       // 'string' in v1
  default?: string;
  description?: string;
  secret?: boolean;
};

type Tier = {
  name: string;
  state?: string;
};

type WizardStep = 'source' | 'preview' | 'tiers' | 'config' | 'confirm';

// stepOrder is the canonical list used by the indicator and by the
// next/back buttons. Adding a step is a single-list change.
const stepOrder: WizardStep[] = ['source', 'preview', 'tiers', 'config', 'confirm'];

function nextStep(s: WizardStep): WizardStep {
  const i = stepOrder.indexOf(s);
  return stepOrder[Math.min(i + 1, stepOrder.length - 1)];
}
function prevStep(s: WizardStep): WizardStep {
  const i = stepOrder.indexOf(s);
  return stepOrder[Math.max(i - 1, 0)];
}

export default function PluginInstall() {
  const { t } = useI18n();
  const navigate = useNavigate();

  const [step, setStep] = useState<WizardStep>('source');
  const [manifestText, setManifestText] = useState('');
  const [parsed, setParsed] = useState<ParsedManifest | null>(null);
  const [tiers, setTiers] = useState<Tier[]>([]);
  // Per-volume tier selection. Keyed by volume name.
  const [tierChoices, setTierChoices] = useState<Record<string, string>>({});
  // Per-config-key value. Keyed by env var name.
  const [configValues, setConfigValues] = useState<Record<string, string>>({});
  // Resolved profile preview from /api/plugin-profiles/preview at confirm.
  const [profilePreview, setProfilePreview] = useState<{ names?: string[] } | null>(null);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');

  // Pre-fetch tiers as soon as the wizard mounts so the picker step
  // is ready by the time the operator gets there. Cheap call; no
  // need to wait until step 3.
  useEffect(() => {
    api.getTiers().then((rows: any) => {
      const list: Tier[] = (Array.isArray(rows) ? rows : rows?.tiers ?? []).map((r: any) => ({
        name: r.name,
        state: r.state,
      }));
      setTiers(list);
    }).catch(() => { /* ignored — wizard renders without; user gets a placeholder */ });
  }, []);

  const tierBoundVolumes = useMemo(
    () => (parsed?.volumes ?? []).filter(v => v.mode === 'tier-bound'),
    [parsed],
  );

  // continueFrom does the per-step validation + side-effects then
  // advances. Splitting per-step logic out keeps the render small.
  function continueFrom(current: WizardStep) {
    setError('');
    if (current === 'source') {
      if (!manifestText.trim()) {
        setError(t('plugins.install.error.empty'));
        return;
      }
      setBusy(true);
      api.parsePlugin({ manifest: manifestText })
        .then(resp => {
          const m = resp.manifest as ParsedManifest;
          setParsed(m);
          // Pre-fill config defaults so the configure step starts
          // with a useful baseline.
          const defaults: Record<string, string> = {};
          for (const f of m.config ?? []) defaults[f.key] = f.default ?? '';
          setConfigValues(defaults);
          // Skip the tiers step if there's nothing to assign.
          setStep('preview');
        })
        .catch(e => setError(extractError(e, t('plugins.install.error.parse'))))
        .finally(() => setBusy(false));
      return;
    }
    if (current === 'preview') {
      // If no tier-bound volumes, skip directly to config.
      setStep(tierBoundVolumes.length === 0 ? 'config' : 'tiers');
      return;
    }
    if (current === 'tiers') {
      const missing = tierBoundVolumes.filter(v => !tierChoices[v.name]);
      if (missing.length > 0) {
        setError(t('plugins.install.error.tierMissing'));
        return;
      }
      setStep('config');
      return;
    }
    if (current === 'config') {
      // Pull the resolved-profile preview so the confirm screen can
      // show what default-limits/operator overrides did to the
      // requested profile list.
      setBusy(true);
      api.previewPluginProfile({ manifest: manifestText })
        .then(resp => setProfilePreview(resp))
        .catch(() => setProfilePreview(null))
        .finally(() => {
          setBusy(false);
          setStep('confirm');
        });
      return;
    }
  }

  function back() {
    setError('');
    // 'preview' → 'source' skips the in-between, etc. — just walk back.
    setStep(prevStep(step));
  }

  function install() {
    if (!parsed) return;
    setBusy(true);
    setError('');
    const tierAssignments = tierBoundVolumes.length > 0
      ? { perVolume: tierChoices }
      : undefined;
    api.installPlugin({ manifest: manifestText, tierAssignments })
      .then(() => {
        // Optional: write the operator's config overrides if any
        // differ from the manifest defaults. The install endpoint
        // already populated defaults; PUT only the changed keys.
        const overrides: Record<string, string> = {};
        for (const f of parsed.config ?? []) {
          const v = configValues[f.key] ?? '';
          if (v !== (f.default ?? '')) overrides[f.key] = v;
        }
        const after = Object.keys(overrides).length > 0
          ? api.updatePluginConfig(parsed.metadata.name, { ...configValues })
          : Promise.resolve();
        return after.then(() => {
          setSuccess(t('plugins.install.success', { name: parsed.metadata.name }));
          // Brief delay so the operator sees the success banner,
          // then bounce back to /plugins.
          setTimeout(() => navigate('/plugins'), 1200);
        });
      })
      .catch(e => setError(extractError(e, t('plugins.install.error.generic'))))
      .finally(() => setBusy(false));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('plugins.install.title')}</h1>
        <p className="subtitle">{t('plugins.install.subtitle')}</p>
      </div>

      <div className="wizard">
        <ol className="wizard-steps">
          {stepOrder.map(s => (
            <li
              key={s}
              className={
                s === step ? 'active' :
                stepOrder.indexOf(s) < stepOrder.indexOf(step) ? 'done' : ''
              }
            >
              {t(`plugins.install.step.${s}`)}
            </li>
          ))}
        </ol>

        {error && (
          <div className="wizard-error">
            <pre>{error}</pre>
          </div>
        )}
        {success && (
          <div className="wizard-success">{success}</div>
        )}

        <div className="wizard-step">
          {step === 'source' && (
            <SourceStep
              text={manifestText}
              onChange={setManifestText}
              busy={busy}
            />
          )}
          {step === 'preview' && parsed && <PreviewStep manifest={parsed} />}
          {step === 'tiers' && (
            <TierStep
              volumes={tierBoundVolumes}
              tiers={tiers}
              choices={tierChoices}
              onChange={(name, tier) => setTierChoices({ ...tierChoices, [name]: tier })}
            />
          )}
          {step === 'config' && parsed && (
            <ConfigStep
              fields={parsed.config ?? []}
              values={configValues}
              onChange={(key, val) => setConfigValues({ ...configValues, [key]: val })}
            />
          )}
          {step === 'confirm' && parsed && (
            <ConfirmStep
              manifest={parsed}
              tierChoices={tierChoices}
              configValues={configValues}
              profilePreview={profilePreview}
            />
          )}
        </div>

        <div className="wizard-actions">
          <button
            className="btn"
            onClick={() => navigate('/plugins')}
            disabled={busy}
          >
            {t('plugins.install.action.cancel')}
          </button>
          <div className="wizard-actions-right">
            {step !== 'source' && (
              <button className="btn" onClick={back} disabled={busy}>
                {t('plugins.install.action.back')}
              </button>
            )}
            {step !== 'confirm' ? (
              <button
                className="btn primary"
                onClick={() => continueFrom(step)}
                disabled={busy}
              >
                {busy ? <Spinner loading={true} /> : t('plugins.install.action.continue')}
              </button>
            ) : (
              <button
                className="btn primary"
                onClick={install}
                disabled={busy || !!success}
              >
                {busy
                  ? t('plugins.install.action.installing')
                  : t('plugins.install.action.install')}
              </button>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

function SourceStep({
  text, onChange, busy,
}: { text: string; onChange: (v: string) => void; busy: boolean }) {
  const { t } = useI18n();
  // 'catalog' is the default — point and click on a known plugin.
  // 'paste' is the fallback for manifests we don't ship a tile for
  // (private builds, sideloaded forks, in-development plugins).
  // Toggling to 'paste' preserves any text the operator typed; toggling
  // back to 'catalog' clears the textarea so a subsequent tile click
  // doesn't merge into stale paste content.
  const [mode, setMode] = useState<'catalog' | 'paste'>('catalog');
  const [selectedId, setSelectedId] = useState<string | null>(null);

  function chooseCatalogEntry(entry: CatalogEntry) {
    setSelectedId(entry.id);
    onChange(entry.manifestYaml);
  }

  return (
    <>
      <div className="wizard-source-modes">
        <button
          type="button"
          className={mode === 'catalog' ? 'tab active' : 'tab'}
          onClick={() => setMode('catalog')}
          disabled={busy}
        >
          {t('plugins.install.source.modeCatalog')}
        </button>
        <button
          type="button"
          className={mode === 'paste' ? 'tab active' : 'tab'}
          onClick={() => {
            setMode('paste');
            setSelectedId(null);
          }}
          disabled={busy}
        >
          {t('plugins.install.source.modePaste')}
        </button>
      </div>

      {mode === 'catalog' ? (
        <>
          <h2>{t('plugins.install.source.catalog.heading')}</h2>
          <p>{t('plugins.install.source.catalog.description')}</p>
          <ul className="wizard-plugin-catalog">
            {pluginCatalog.map(entry => (
              <li
                key={entry.id}
                className={
                  'wizard-plugin-card' +
                  (selectedId === entry.id ? ' selected' : '')
                }
              >
                <button
                  type="button"
                  className="wizard-plugin-card-button"
                  onClick={() => chooseCatalogEntry(entry)}
                  disabled={busy}
                >
                  <div className="wizard-plugin-card-header">
                    <span className="wizard-plugin-card-name">{entry.name}</span>
                    <span className="wizard-plugin-card-vendor">{entry.vendor}</span>
                  </div>
                  <p className="wizard-plugin-card-description">{entry.description}</p>
                  {entry.tags && entry.tags.length > 0 && (
                    <div className="wizard-plugin-card-tags">
                      {entry.tags.map(tag => (
                        <span key={tag} className="wizard-plugin-card-tag">{tag}</span>
                      ))}
                    </div>
                  )}
                </button>
              </li>
            ))}
          </ul>
          {selectedId && (
            <p className="wizard-plugin-catalog-hint">
              {t('plugins.install.source.catalog.selectedHint')}
            </p>
          )}
        </>
      ) : (
        <>
          <h2>{t('plugins.install.source.heading')}</h2>
          <p>{t('plugins.install.source.description')}</p>
          <textarea
            className="wizard-textarea"
            value={text}
            onChange={e => onChange(e.target.value)}
            placeholder={t('plugins.install.source.placeholder')}
            spellCheck={false}
            disabled={busy}
          />
        </>
      )}
    </>
  );
}

function PreviewStep({ manifest }: { manifest: ParsedManifest }) {
  const { t } = useI18n();
  const empty = t('plugins.install.preview.empty');
  return (
    <>
      <h2>{t('plugins.install.preview.heading')}</h2>
      <dl className="wizard-fields">
        <dt>{t('plugins.label.version')}</dt>
        <dd>{manifest.metadata.name} @ {manifest.metadata.version}</dd>
        {manifest.artifact.type === 'oci-image' && (
          <>
            <dt>{t('plugins.install.preview.image')}</dt>
            <dd className="mono">
              {manifest.artifact.image}
              {manifest.artifact.digest ? `@${manifest.artifact.digest}` : ''}
            </dd>
          </>
        )}
        {manifest.artifact.type === 'lxc-distro' && (
          <>
            <dt>{t('plugins.install.preview.distro')}</dt>
            <dd>
              {manifest.artifact.distro}/{manifest.artifact.release}
              {manifest.artifact.arch ? `/${manifest.artifact.arch}` : ''}
            </dd>
          </>
        )}
        {manifest.container.command && manifest.container.command.length > 0 && (
          <>
            <dt>{t('plugins.install.preview.command')}</dt>
            <dd className="mono">{manifest.container.command.join(' ')}</dd>
          </>
        )}
        <dt>{t('plugins.install.preview.instances')}</dt>
        <dd>{manifest.instances?.count ?? 1}</dd>
        <dt>{t('plugins.install.preview.profiles')}</dt>
        <dd>
          {(manifest.profiles ?? []).length > 0 ? (
            <span className="chip-row">
              {(manifest.profiles ?? []).map(p => <span key={p} className="chip">{p}</span>)}
            </span>
          ) : empty}
        </dd>
        <dt>{t('plugins.install.preview.volumes')}</dt>
        <dd>
          {(manifest.volumes ?? []).length > 0 ? (
            <ul style={{ margin: 0, padding: '0 0 0 16px' }}>
              {(manifest.volumes ?? []).map(v => (
                <li key={v.name}>
                  <span className="mono">{v.name}</span>
                  {' → '}
                  <span className="mono">{v.bind}</span>
                  {' ('}
                  {v.mode === 'tier-bound' ? `tier-bound, ${v.slot}` : 'flat'}
                  {v.perInstance ? ', perInstance' : ''}
                  {')'}
                </li>
              ))}
            </ul>
          ) : empty}
        </dd>
        <dt>{t('plugins.install.preview.ports')}</dt>
        <dd>
          {(manifest.ports ?? []).length > 0
            ? (manifest.ports ?? []).map(p => `${p.name} (${p.port}/${p.protocol})`).join(', ')
            : empty}
        </dd>
        <dt>{t('plugins.install.preview.config')}</dt>
        <dd>
          {(manifest.config ?? []).length > 0
            ? (manifest.config ?? []).map(c => c.key).join(', ')
            : empty}
        </dd>
      </dl>
    </>
  );
}

function TierStep({
  volumes, tiers, choices, onChange,
}: {
  volumes: ManifestVolume[];
  tiers: Tier[];
  choices: Record<string, string>;
  onChange: (volumeName: string, tier: string) => void;
}) {
  const { t } = useI18n();
  if (volumes.length === 0) {
    return (
      <>
        <h2>{t('plugins.install.tiers.heading')}</h2>
        <p>{t('plugins.install.tiers.noTierBound')}</p>
      </>
    );
  }
  return (
    <>
      <h2>{t('plugins.install.tiers.heading')}</h2>
      <p>{t('plugins.install.tiers.description')}</p>
      {volumes.map(v => (
        <div key={v.name} className="wizard-tier-row">
          <span className="volume-name">{v.name}</span>
          <span className="slot-name">{v.slot}</span>
          <select
            value={choices[v.name] ?? ''}
            onChange={e => onChange(v.name, e.target.value)}
          >
            <option value="">{t('plugins.install.tiers.placeholder')}</option>
            {tiers.map(tier => (
              <option key={tier.name} value={tier.name}>{tier.name}</option>
            ))}
          </select>
        </div>
      ))}
    </>
  );
}

function ConfigStep({
  fields, values, onChange,
}: {
  fields: ManifestConfigField[];
  values: Record<string, string>;
  onChange: (key: string, val: string) => void;
}) {
  const { t } = useI18n();
  if (fields.length === 0) {
    return (
      <>
        <h2>{t('plugins.install.config.heading')}</h2>
        <p>{t('plugins.install.config.noFields')}</p>
      </>
    );
  }
  return (
    <>
      <h2>{t('plugins.install.config.heading')}</h2>
      <p>{t('plugins.install.config.description')}</p>
      {fields.map(f => (
        <div key={f.key} className="wizard-config-row">
          <span className="config-key">{f.key}</span>
          <input
            className="config-input"
            type={f.secret ? 'password' : 'text'}
            value={values[f.key] ?? ''}
            onChange={e => onChange(f.key, e.target.value)}
            placeholder={f.default ?? ''}
          />
          {f.description && <span className="config-description">{f.description}</span>}
        </div>
      ))}
    </>
  );
}

function ConfirmStep({
  manifest, tierChoices, configValues, profilePreview,
}: {
  manifest: ParsedManifest;
  tierChoices: Record<string, string>;
  configValues: Record<string, string>;
  profilePreview: { names?: string[] } | null;
}) {
  const { t } = useI18n();
  const tierBound = (manifest.volumes ?? []).filter(v => v.mode === 'tier-bound');
  return (
    <>
      <h2>{t('plugins.install.confirm.heading')}</h2>
      <dl className="wizard-fields">
        <dt>{t('plugins.label.version')}</dt>
        <dd>{manifest.metadata.name} @ {manifest.metadata.version}</dd>

        {tierBound.length > 0 && (
          <>
            <dt>{t('plugins.install.confirm.tiersNote')}</dt>
            <dd>
              <ul style={{ margin: 0, padding: '0 0 0 16px' }}>
                {tierBound.map(v => (
                  <li key={v.name}>
                    <span className="mono">{v.name}</span>
                    {' → '}
                    <span className="mono">{tierChoices[v.name]}</span>
                  </li>
                ))}
              </ul>
            </dd>
          </>
        )}

        <dt>{t('plugins.install.confirm.profilesNote')}</dt>
        <dd>
          {profilePreview?.names && profilePreview.names.length > 0 ? (
            <span className="chip-row">
              {profilePreview.names.map(n => <span key={n} className="chip">{n}</span>)}
            </span>
          ) : (
            t('plugins.install.confirm.previewError')
          )}
        </dd>

        {Object.keys(configValues).length > 0 && (
          <>
            <dt>{t('plugins.install.preview.config')}</dt>
            <dd>
              <ul style={{ margin: 0, padding: '0 0 0 16px' }}>
                {Object.entries(configValues).map(([k, v]) => {
                  const field = (manifest.config ?? []).find(f => f.key === k);
                  const display = field?.secret ? '••••••' : (v || '(empty)');
                  return (
                    <li key={k}>
                      <span className="mono">{k}={display}</span>
                    </li>
                  );
                })}
              </ul>
            </dd>
          </>
        )}
      </dl>
    </>
  );
}
