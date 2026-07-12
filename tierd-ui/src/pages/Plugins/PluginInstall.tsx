import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import { pluginCatalogRepositories, type CatalogRepository } from '../../data/pluginCatalog';

// ParsedManifest matches the JSON shape POST /api/plugins/parse
// returns. Loose typing — the backend validates and the wizard
// only reads a small subset.
type ManifestService = {
  name: string;
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
  container?: { command?: string[]; restartPolicy?: string };
  volumes?: ManifestVolume[];
  ports?: { name: string; port: number; protocol: string; expose: boolean; hostExpose?: boolean }[];
  config?: ManifestConfigField[];
};

type ParsedManifest = {
  apiVersion: string;
  kind: string;
  metadata: { name: string; version: string; description?: string; vendor?: string; homepage?: string };
  services?: ManifestService[];
  // Legacy top-level fields kept optional for back-compat with any
  // hand-pasted old single-image manifests that bypass ParseManifest.
  artifact?: {
    type: string;
    image?: string;
    digest?: string;
    distro?: string;
    release?: string;
    arch?: string;
    packages?: string[];
    setup?: string[];
  };
  container?: { command?: string[]; restartPolicy?: string };
  instances?: { count?: number; configurable?: boolean };
  volumes?: ManifestVolume[];
  ports?: { name: string; port: number; protocol: string; expose: boolean; hostExpose?: boolean }[];
  ui?: { embed?: { path?: string; auth?: string } };
  profiles?: string[];
  config?: ManifestConfigField[];
};

function manifestServices(m: ParsedManifest | null | undefined): ManifestService[] {
  if (!m) return [];
  if (m.services && m.services.length > 0) return m.services;
  // Defensive fallback for a hand-pasted legacy single-service manifest.
  if (m.artifact) {
    return [{
      name: m.metadata?.name || 'plugin',
      artifact: m.artifact,
      container: m.container,
      volumes: m.volumes,
      ports: m.ports,
      config: m.config,
    }];
  }
  return [];
}

function allVolumes(m: ParsedManifest | null | undefined): ManifestVolume[] {
  return manifestServices(m).flatMap(s => s.volumes ?? []);
}

function allPorts(m: ParsedManifest | null | undefined): { name: string; port: number; protocol: string; expose: boolean; hostExpose?: boolean }[] {
  return manifestServices(m).flatMap(s => s.ports ?? []);
}

function allConfig(m: ParsedManifest | null | undefined): ManifestConfigField[] {
  return manifestServices(m).flatMap(s => s.config ?? []);
}

function allServiceImages(m: ParsedManifest | null | undefined): string {
  return manifestServices(m).map(s => s.artifact?.image ?? '').filter(Boolean).join(' ');
}

type ManifestVolume = {
  name: string;
  mode: string;       // 'tier-bound' | 'flat'
  bind: string;
  perInstance?: boolean;
  minSize?: string;
};

type ManifestConfigField = {
  key: string;
  type: string;
  label?: string;
  default?: string;
  description?: string;
  secret?: boolean;
  gpuVendor?: string;
  options?: { value: string; label?: string }[];
  min?: string;
  max?: string;
  step?: string;
  unit?: string;
};

type CatalogEntry = {
  id: string;
  name: string;
  vendor: string;
  description: string;
  homepage?: string;
  tags: string[];
  variants: CatalogManifestVariant[];
  // 'builtin' = served from the appliance's bundled snapshot (works offline);
  // 'github' = fetched live. Drives the "Bundled" card badge.
  source?: 'builtin' | 'github';
};

type CatalogManifestVariant = {
  id: string;
  label: string;
  accelerator: string;
  manifestYaml: string;
  manifest: ParsedManifest;
  assetName: string;
};

type CatalogAcceleratorChoice = {
  value: string;
  label: string;
  variant: CatalogManifestVariant;
  gpu?: GPUInfo;
};

type Tier = {
  name: string;
  state?: string;
};

type GPUInfo = {
  id: string;
  vendor: string;
  label: string;
  devicePath: string;
};

type CatalogGPUSelection = {
  vendor: string;
  devicePath: string;
} | null;

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
  const [gpus, setGpus] = useState<GPUInfo[]>([]);
  const [catalogGPUSelection, setCatalogGPUSelection] = useState<CatalogGPUSelection>(null);
  // Per-volume tier selection. Keyed by volume name.
  const [tierChoices, setTierChoices] = useState<Record<string, string>>({});
  // Per-config-key value. Keyed by env var name.
  const [configValues, setConfigValues] = useState<Record<string, string>>({});
  // Resolved profile preview from /api/plugin-profiles/preview at confirm.
  const [profilePreview, setProfilePreview] = useState<{ names?: string[] } | null>(null);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

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
    api.getPluginGPUs().then((resp: any) => {
      setGpus(normalizeGPUs(resp?.gpus));
    }).catch(() => { /* ignored — GPU fields keep the automatic option */ });
  }, []);

  const tierBoundVolumes = useMemo(
    () => allVolumes(parsed).filter(v => v.mode === 'tier-bound'),
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
          for (const f of allConfig(m)) {
            defaults[f.key] = f.default ?? '';
            if (f.type === 'gpu' && catalogGPUSelection && gpuMatchesField(f, catalogGPUSelection.vendor)) {
              defaults[f.key] = catalogGPUSelection.devicePath;
            }
          }
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
    api.installPlugin({ manifest: manifestText, tierAssignments, config: { ...configValues } })
      .then(() => {
        // Install registers immediately and materialises in the background, so
        // leave the flow right away — the new plugin shows up in the list in its
        // pulling/creating state. No waiting on a multi-minute image pull.
        navigate('/plugins');
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

        <div className="wizard-step">
          {step === 'source' && (
            <SourceStep
              text={manifestText}
              onChange={setManifestText}
              gpus={gpus}
              onGPUSelection={setCatalogGPUSelection}
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
              gpus={gpus}
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
                disabled={busy}
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
  text, onChange, gpus, onGPUSelection, busy,
}: {
  text: string;
  onChange: (v: string) => void;
  gpus: GPUInfo[];
  onGPUSelection: (gpu: CatalogGPUSelection) => void;
  busy: boolean;
}) {
  const { t } = useI18n();
  // 'catalog' is the default — point and click on a known plugin.
  // 'paste' is the fallback for manifests we don't ship a tile for
  // (private builds, sideloaded forks, in-development plugins).
  // Toggling to 'paste' preserves any text the operator typed; toggling
  // back to 'catalog' clears the textarea so a subsequent tile click
  // doesn't merge into stale paste content.
  const [mode, setMode] = useState<'catalog' | 'paste'>('catalog');
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [selectedAccelerators, setSelectedAccelerators] = useState<Record<string, string>>({});
  const [catalogEntries, setCatalogEntries] = useState<CatalogEntry[]>([]);
  const [catalogLoading, setCatalogLoading] = useState(true);
  const [catalogError, setCatalogError] = useState('');

  useEffect(() => {
    let cancelled = false;
    setCatalogLoading(true);
    setCatalogError('');
    Promise.all(pluginCatalogRepositories.map(repo =>
      api.getPluginCatalogLatest(repo.repo)
        .then(release => ({ entries: buildCatalogEntries(repo, release), error: '' }))
        .catch(e => ({ entries: [] as CatalogEntry[], error: `${repo.repo}: ${extractError(e, 'Unable to load plugin catalog.')}` }))
    ))
      .then(results => {
        if (cancelled) return;
        const entries = results.flatMap(result => result.entries);
        setCatalogEntries(entries);
        setCatalogError(entries.length === 0 ? results.map(result => result.error).filter(Boolean).join('\n') : '');
      })
      .finally(() => {
        if (!cancelled) setCatalogLoading(false);
      });
    return () => { cancelled = true; };
  }, []);

  function chooseCatalogEntry(entry: CatalogEntry) {
    setSelectedId(entry.id);
    const choice = defaultAcceleratorChoice(entry, gpus);
    setSelectedAccelerators({ ...selectedAccelerators, [entry.id]: choice.value });
    applyCatalogChoice(choice);
  }

  function applyCatalogChoice(choice: CatalogAcceleratorChoice) {
    onChange(choice.variant.manifestYaml);
    onGPUSelection(choice.gpu ? { vendor: choice.gpu.vendor, devicePath: choice.gpu.devicePath } : null);
  }

  function changeAccelerator(entry: CatalogEntry, value: string) {
    const choices = acceleratorChoices(entry, gpus);
    const choice = choices.find(c => c.value === value) ?? defaultAcceleratorChoice(entry, gpus);
    setSelectedAccelerators({ ...selectedAccelerators, [entry.id]: choice.value });
    applyCatalogChoice(choice);
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
            onGPUSelection(null);
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
          {catalogLoading && <Spinner loading={true} />}
          {catalogError && <div className="wizard-error"><pre>{catalogError}</pre></div>}
          {!catalogLoading && catalogEntries.length > 0 && (
            <ul className="wizard-plugin-catalog">
              {catalogEntries.map(entry => (
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
                      {entry.source === 'builtin' && (
                        <span
                          className="wizard-plugin-card-badge"
                          title={t('plugins.install.source.catalog.bundledHint')}
                        >
                          {t('plugins.install.source.catalog.bundledBadge')}
                        </span>
                      )}
                      <span className="wizard-plugin-card-vendor">{entry.vendor}</span>
                    </div>
                    <p className="wizard-plugin-card-description">{entry.description}</p>
                    {entry.tags.length > 0 && (
                      <div className="wizard-plugin-card-tags">
                        {entry.tags.map(tag => (
                          <span key={tag} className="wizard-plugin-card-tag">{tag}</span>
                        ))}
                      </div>
                    )}
                  </button>
                  {selectedId === entry.id && acceleratorChoices(entry, gpus).length > 1 && (
                    <label className="wizard-plugin-card-accelerator">
                      <span>{t('plugins.install.source.catalog.accelerator')}</span>
                      <select
                        value={selectedAccelerators[entry.id] ?? defaultAcceleratorChoice(entry, gpus).value}
                        onChange={e => changeAccelerator(entry, e.target.value)}
                        disabled={busy}
                      >
                        {acceleratorChoices(entry, gpus).map(choice => (
                          <option key={choice.value} value={choice.value}>
                            {choice.label}
                          </option>
                        ))}
                      </select>
                    </label>
                  )}
                </li>
              ))}
            </ul>
          )}
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
            onChange={e => {
              onGPUSelection(null);
              onChange(e.target.value);
            }}
            placeholder={t('plugins.install.source.placeholder')}
            spellCheck={false}
            disabled={busy}
          />
        </>
      )}
    </>
  );
}

function buildCatalogEntries(repo: CatalogRepository, release: {
  repo: string;
  tagName: string;
  releaseUrl: string;
  source?: 'builtin' | 'github';
  manifests: Array<{ assetName: string; downloadUrl: string; manifestYaml: string; manifest: ParsedManifest }>;
}): CatalogEntry[] {
  const grouped = new Map<string, typeof release.manifests>();
  for (const item of release.manifests) {
    const key = item.manifest.metadata.name || item.assetName;
    grouped.set(key, [...(grouped.get(key) ?? []), item]);
  }

  return Array.from(grouped.entries()).map(([pluginName, items]) => {
    const variants = items.map(item => {
      const label = inferManifestVariant(item.assetName, item.manifest) || item.assetName;
      return {
        id: `${repo.id}:${item.assetName}`,
        label,
        accelerator: manifestAccelerator(item.assetName, item.manifest),
        manifestYaml: item.manifestYaml,
        manifest: item.manifest,
        assetName: item.assetName,
      };
    }).sort((a, b) => acceleratorRank(a.accelerator) - acceleratorRank(b.accelerator) || a.assetName.localeCompare(b.assetName));
    const primary = variants.find(v => v.accelerator !== 'cpu') ?? variants[0];
    const tags = new Set<string>();
    for (const variant of variants) {
      for (const tag of catalogTags(variant.manifest, variant.label, release.tagName)) {
        tags.add(tag);
      }
    }
    return {
      id: `${repo.id}:${pluginName}`,
      name: displayPluginName(pluginName),
      vendor: primary.manifest.metadata.vendor || ownerFromRepo(release.repo),
      description: firstSentence(primary.manifest.metadata.description) || primary.assetName,
      homepage: primary.manifest.metadata.homepage || `https://github.com/${release.repo}`,
      tags: Array.from(tags),
      variants,
      source: release.source,
    };
  });
}

function displayPluginName(name: string): string {
  switch (name) {
    case 'aimee-server':
      return 'aimee server';
    case 'aimee-kb':
      return 'aimee knowledge base';
    case 'aimee-combined':
      return 'aimee server + kb';
    case 'gh-runner':
      return 'GitHub Actions runner';
    case 'llama-cpp':
      return 'llama.cpp';
    case 'vllm':
      return 'vLLM';
    case 'wolf':
      return 'Wolf';
    default:
      return name
        .split('-')
        .filter(Boolean)
        .map(part => part.charAt(0).toUpperCase() + part.slice(1))
        .join(' ');
  }
}

function inferManifestVariant(assetName: string, manifest: ParsedManifest): string {
  const raw = `${assetName} ${allServiceImages(manifest)} ${(manifest.profiles ?? []).join(' ')}`.toLowerCase();
  if (raw.includes('cpu')) return 'CPU only';
  if (raw.includes('cuda')) return 'NVIDIA CUDA';
  if (raw.includes('rocm')) return 'AMD ROCm';
  if (raw.includes('vulkan')) return 'Vulkan';
  if (raw.includes('gpu-nvidia')) return 'NVIDIA';
  if (raw.includes('gpu-amd')) return 'AMD';
  if (raw.includes('gpu-intel')) return 'Intel';
  return '';
}

function manifestAccelerator(assetName: string, manifest: ParsedManifest): string {
  const raw = `${assetName} ${allServiceImages(manifest)} ${(manifest.profiles ?? []).join(' ')}`.toLowerCase();
  if (raw.includes('cpu')) return 'cpu';
  if (raw.includes('cuda') || raw.includes('gpu-nvidia')) return 'nvidia';
  if (raw.includes('gpu-intel')) return 'intel';
  if (raw.includes('rocm') || raw.includes('vulkan') || raw.includes('gpu-amd')) return 'amd';
  return 'unknown';
}

function acceleratorRank(accelerator: string): number {
  switch (accelerator) {
    case 'nvidia': return 10;
    case 'amd': return 20;
    case 'intel': return 30;
    case 'cpu': return 90;
    default: return 100;
  }
}

function acceleratorChoices(entry: CatalogEntry, gpus: GPUInfo[]): CatalogAcceleratorChoice[] {
  if (entry.variants.length === 1) {
    return [{
      value: entry.variants[0].id,
      label: entry.variants[0].label,
      variant: entry.variants[0],
    }];
  }

  const choices: CatalogAcceleratorChoice[] = [];
  const seen = new Set<string>();

  for (const gpu of gpus) {
    const variant = variantForGPU(entry, gpu);
    if (!variant) continue;
    const value = `gpu:${gpu.devicePath}`;
    if (seen.has(value)) continue;
    seen.add(value);
    choices.push({
      value,
      label: `${gpu.label} (${gpu.devicePath})`,
      variant,
      gpu,
    });
  }
  const cpu = entry.variants.find(v => v.accelerator === 'cpu');
  if (cpu) {
    choices.push({
      value: 'cpu',
      label: 'CPU only',
      variant: cpu,
    });
    seen.add('cpu');
  }

  if (choices.length === 0) {
    for (const variant of entry.variants) {
      choices.push({
        value: variant.id,
        label: variant.label,
        variant,
      });
    }
  }
  return choices;
}

function normalizeGPUs(rows: any): GPUInfo[] {
  return (Array.isArray(rows) ? rows : []).map((g: any) => ({
    id: g.id ?? g.devicePath,
    vendor: g.vendor ?? '',
    label: g.label ?? g.name ?? g.devicePath,
    devicePath: g.devicePath ?? '',
  })).filter((g: GPUInfo) => !!g.devicePath);
}

function defaultAcceleratorChoice(entry: CatalogEntry, gpus: GPUInfo[]): CatalogAcceleratorChoice {
  const choices = acceleratorChoices(entry, gpus);
  return choices.find(c => c.gpu) ?? choices.find(c => c.value === 'cpu') ?? choices[0];
}

function variantForGPU(entry: CatalogEntry, gpu: GPUInfo): CatalogManifestVariant | undefined {
  const vendor = gpu.vendor.toLowerCase();
  const exact = entry.variants.find(v => v.accelerator === vendor);
  if (exact) return exact;
  if (vendor === 'intel') {
    return entry.variants.find(v => v.accelerator === 'amd');
  }
  return undefined;
}

function catalogTags(manifest: ParsedManifest, variant: string, releaseTag: string): string[] {
  const tags = new Set<string>();
  if (releaseTag) tags.add(releaseTag);
  if (variant) tags.add(variant);
  if ((manifest.profiles ?? []).includes('runtime-control')) tags.add('runtime');
  if ((manifest.profiles ?? []).includes('wolf-runtime')) tags.add('runtime');
  if (allPorts(manifest).some(p => p.hostExpose)) tags.add('host ports');
  if (manifest.ui?.embed) tags.add('UI');
  if (manifest.instances?.configurable || (manifest.instances?.count ?? 1) > 1) tags.add('multi-instance');
  return Array.from(tags);
}

function firstSentence(text?: string): string {
  return (text ?? '').trim().split(/\n+/)[0]?.trim() ?? '';
}

function ownerFromRepo(repo: string): string {
  return repo.split('/')[0] || repo;
}

function PreviewStep({ manifest }: { manifest: ParsedManifest }) {
  const { t } = useI18n();
  const empty = t('plugins.install.preview.empty');
  const services = manifestServices(manifest);
  const showServiceHeading = services.length > 1;
  const allKeys = Array.from(new Set(allConfig(manifest).map(c => c.key)));
  return (
    <>
      <h2>{t('plugins.install.preview.heading')}</h2>
      <dl className="wizard-fields">
        <dt>{t('plugins.label.version')}</dt>
        <dd>{manifest.metadata.name} @ {manifest.metadata.version}</dd>
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
      </dl>

      {services.map(svc => (
        <div key={svc.name} className="wizard-preview-service">
          {showServiceHeading && (
            <h3 className="wizard-preview-service-heading">
              {t('plugins.install.preview.service', { name: svc.name })}
            </h3>
          )}
          <dl className="wizard-fields">
            {svc.artifact.type === 'oci-image' && (
              <>
                <dt>{t('plugins.install.preview.image')}</dt>
                <dd className="mono">
                  {svc.artifact.image}
                  {svc.artifact.digest ? `@${svc.artifact.digest}` : ''}
                </dd>
              </>
            )}
            {svc.artifact.type === 'lxc-distro' && (
              <>
                <dt>{t('plugins.install.preview.distro')}</dt>
                <dd>
                  {svc.artifact.distro}/{svc.artifact.release}
                  {svc.artifact.arch ? `/${svc.artifact.arch}` : ''}
                </dd>
              </>
            )}
            {svc.container?.command && svc.container.command.length > 0 && (
              <>
                <dt>{t('plugins.install.preview.command')}</dt>
                <dd className="mono">{svc.container.command.join(' ')}</dd>
              </>
            )}
            <dt>{t('plugins.install.preview.volumes')}</dt>
            <dd>
              {(svc.volumes ?? []).length > 0 ? (
                <ul style={{ margin: 0, padding: '0 0 0 16px' }}>
                  {(svc.volumes ?? []).map(v => (
                    <li key={`${svc.name}:${v.name}`}>
                      <span className="mono">{v.name}</span>
                      {' → '}
                      <span className="mono">{v.bind}</span>
                      {' ('}
                      {v.mode === 'tier-bound' ? 'tier-bound' : 'flat'}
                      {v.perInstance ? ', perInstance' : ''}
                      {')'}
                    </li>
                  ))}
                </ul>
              ) : empty}
            </dd>
            <dt>{t('plugins.install.preview.ports')}</dt>
            <dd>
              {(svc.ports ?? []).length > 0
                ? (svc.ports ?? []).map(p => `${p.name} (${p.port}/${p.protocol}${p.hostExpose ? ', host' : ''})`).join(', ')
                : empty}
            </dd>
          </dl>
        </div>
      ))}

      <dl className="wizard-fields">
        <dt>{t('plugins.install.preview.config')}</dt>
        <dd>{allKeys.length > 0 ? allKeys.join(', ') : empty}</dd>
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
  fields, values, gpus, onChange,
}: {
  fields: ManifestConfigField[];
  values: Record<string, string>;
  gpus: GPUInfo[];
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
          <span className="config-key">{f.label || f.key}</span>
          <ConfigInput field={f} value={values[f.key] ?? ''} gpus={gpus} onChange={v => onChange(f.key, v)} />
          {f.description && <span className="config-description">{f.description}</span>}
        </div>
      ))}
    </>
  );
}

function ConfigInput({
  field,
  value,
  gpus,
  onChange,
}: {
  field: ManifestConfigField;
  value: string;
  gpus: GPUInfo[];
  onChange: (value: string) => void;
}) {
  const { t } = useI18n();
  if (field.type === 'select') {
    return (
      <select
        className="config-input"
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
        className="config-checkbox"
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
        className="config-input"
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
        className="config-input"
        type={field.secret ? 'password' : field.type === 'number' ? 'number' : 'text'}
        value={value}
        onChange={e => onChange(e.target.value)}
        placeholder={field.default ?? ''}
        min={field.min}
        max={field.max}
        step={field.step}
      />
      {field.unit && <span>{field.unit}</span>}
    </div>
  );
}

function gpuOptions(field: ManifestConfigField, value: string, gpus: GPUInfo[]): GPUInfo[] {
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

function gpuMatchesField(field: { gpuVendor?: string }, gpuVendor: string): boolean {
  const vendor = (field.gpuVendor ?? '').toLowerCase();
  const gpu = gpuVendor.toLowerCase();
  return !vendor || gpu === vendor || (vendor === 'amd' && gpu === 'intel');
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
  const tierBound = allVolumes(manifest).filter(v => v.mode === 'tier-bound');
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
                  const field = allConfig(manifest).find(f => f.key === k);
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
