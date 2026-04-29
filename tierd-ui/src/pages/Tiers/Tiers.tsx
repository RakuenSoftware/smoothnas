import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';
import NamespaceFiles from './NamespaceFiles';

function formatBytes(n: number): string {
  if (!n || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0; let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function Tiers() {
  const { t } = useI18n();
  const { invalidate: invalidatePreload } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [tiers, setTiers] = useState<any[]>([]);
  const [arrays, setArrays] = useState<any[]>([]);
  const [pools, setPools] = useState<any[]>([]);
  const [namespaces, setNamespaces] = useState<any[]>([]);
  const [spindownByPool, setSpindownByPool] = useState<Record<string, any>>({});
  const [spindownBusy, setSpindownBusy] = useState<Record<string, boolean>>({});
  const [expandedFiles, setExpandedFiles] = useState<Record<string, boolean>>({});
  const [createName, setCreateName] = useState('');
  const [creating, setCreating] = useState(false);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [metaOnFastest, setMetaOnFastest] = useState(false);
  const [customTiers, setCustomTiers] = useState<{ name: string; rank: number }[]>([
    { name: 'NVME', rank: 1 },
    { name: 'SSD', rank: 2 },
    { name: 'HDD', rank: 3 },
  ]);
  const [addSelections, setAddSelections] = useState<Record<string, string>>({});
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // Inline editing state for tier level fields
  const [editingLevel, setEditingLevel] = useState<Record<string, { targetFill: string; fullThreshold: string }>>({});
  const [savingLevel, setSavingLevel] = useState<Record<string, boolean>>({});

  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const [confirmClass, setConfirmClass] = useState('btn danger');
  const [confirmText, setConfirmText] = useState(t('arrays.confirm.confirm'));
  const confirmAction = useRef<(() => void) | null>(null);

  const tiersRef = useRef(tiers);
  tiersRef.current = tiers;

  useEffect(() => {
    load();
    // Steady-state refresh: keep usage numbers live while the page is open
    // (transient state polling, set up separately in startPolling, runs
    // faster and only while a tier is provisioning/destroying).
    const refresh = setInterval(() => { load(true); }, 3000);
    return () => {
      clearInterval(refresh);
      if (pollRef.current) clearInterval(pollRef.current);
    };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function startPolling() {
    if (pollRef.current) return; // already polling
    pollRef.current = setInterval(() => {
      load(true).then(() => {
        const busy = (tiersRef.current || []).some((t: any) => t.state === 'provisioning' || t.state === 'destroying');
        if (!busy && pollRef.current) {
          clearInterval(pollRef.current);
          pollRef.current = null;
        }
      });
    }, 1500);
  }

  function load(silent = false): Promise<void> {
    if (!silent) setLoading(true);
    setError('');
    const p = Promise.all([
      api.getTiers(),
      api.getArrays(),
      api.getPools().catch(() => []),
      api.getTieringNamespaces().catch(() => []),
    ])
      .then(([ts, arr, pls, nss]) => {
        setTiers(ts || []);
        setArrays(arr || []);
        setPools(pls || []);
        setNamespaces(nss || []);
        refreshSpindown(ts || []);
        setLoading(false);
        // Auto-poll if any tiers are in a transitional state
        if ((ts || []).some((t: any) => t.state === 'provisioning' || t.state === 'destroying')) {
          startPolling();
        }
      })
      .catch(e => {
        const msg = extractError(e, t('tiers.error.load'));
        setError(msg);
        toast.error(msg);
        setLoading(false);
      });
    return p;
  }

  function refreshSpindown(tierList = tiers) {
    Promise.all(
      (tierList || []).map((t: any) =>
        api.getTierSpindown(t.name)
          .then((policy: any) => ({ name: t.name, policy }))
          .catch(() => ({ name: t.name, policy: null }))
      )
    ).then(rows => {
      setSpindownByPool(prev => {
        const next = { ...prev };
        for (const row of rows) {
          if (row.policy) next[row.name] = row.policy;
        }
        return next;
      });
    });
  }

  function setPoolSpindown(poolName: string, enabled: boolean) {
    setSpindownBusy(prev => ({ ...prev, [poolName]: true }));
    api.setTierSpindown(poolName, enabled)
      .then((policy: any) => {
        setSpindownByPool(prev => ({ ...prev, [poolName]: policy }));
        toast.success(enabled ? t('tiers.toast.spindownEnabled') : t('tiers.toast.spindownDisabled'));
      })
      .catch(e => toast.error(extractError(e, t('tiers.error.spindown'))))
      .finally(() => setSpindownBusy(prev => ({ ...prev, [poolName]: false })));
  }

  function setPoolActiveWindows(poolName: string, activeWindows: any[]) {
    const current = spindownByPool[poolName];
    setSpindownBusy(prev => ({ ...prev, [poolName]: true }));
    api.setTierSpindown(poolName, !!current?.enabled, activeWindows)
      .then((policy: any) => {
        setSpindownByPool(prev => ({ ...prev, [poolName]: policy }));
        toast.success(activeWindows.length ? t('tiers.toast.activeWindowSaved') : t('tiers.toast.activeWindowsCleared'));
      })
      .catch(e => toast.error(extractError(e, t('tiers.error.activeWindows'))))
      .finally(() => setSpindownBusy(prev => ({ ...prev, [poolName]: false })));
  }

  function arrayById(id: number) {
    return arrays.find((a: any) => a.id === id);
  }

  function assignedArrayIds(): Set<number> {
    const out = new Set<number>();
    for (const t of tiers) {
      for (const slot of t.tiers || []) {
        if (slot.array_id != null) out.add(slot.array_id);
      }
    }
    return out;
  }

  // Backings used by any tier slot, keyed by "kind:ref" (e.g. "zfs:tank").
  // mdadm uses array_id for uniqueness and that set is returned separately
  // by assignedArrayIds; this one covers everything else.
  function assignedBackingKeys(): Set<string> {
    const out = new Set<string>();
    for (const t of tiers) {
      for (const slot of t.tiers || []) {
        if (slot.backing_kind && slot.backing_kind !== 'mdadm' && slot.backing_ref) {
          out.add(`${slot.backing_kind}:${slot.backing_ref}`);
        }
      }
    }
    return out;
  }

  // Candidate is a generic, displayable item that can be assigned to a
  // tier slot. kind="mdadm" items carry an array_id; kind="zfs" (and
  // future btrfs/bcachefs) carry a backing_ref.
  type Candidate = {
    key: string;      // unique across kinds: "mdadm:md0" or "zfs:tank"
    label: string;    // what the dropdown shows
    kind: string;     // "mdadm" | "zfs" | ...
    arrayId?: number; // set for mdadm
    ref?: string;     // set for non-mdadm (and also carried by mdadm as device path)
    arrayPath?: string; // set for mdadm — used for optimistic UI updates
  };

  // Flat list of every backing that isn't already assigned to a slot.
  // Mixes mdadm arrays and ZFS pools today; btrfs/bcachefs slot in
  // here too once they're creatable from the Arrays page.
  function unassignedBackings(): Candidate[] {
    const arrIDs = assignedArrayIds();
    const otherKeys = assignedBackingKeys();
    const out: Candidate[] = [];
    for (const a of arrays || []) {
      if (arrIDs.has(a.id)) continue;
      out.push({
        key: `mdadm:${a.name}`,
        label: `${a.name} (${a.raid_level || 'mdadm'}, ${a.lv_size || a.size_human || '?'})`,
        kind: 'mdadm',
        arrayId: a.id,
        arrayPath: a.path,
      });
    }
    for (const p of pools || []) {
      const key = `zfs:${p.name}`;
      if (otherKeys.has(key)) continue;
      out.push({
        key,
        label: `${p.name} (zfs ${p.vdev_type || ''}, ${p.size_human || '?'})`,
        kind: 'zfs',
        ref: p.name,
      });
    }
    return out;
  }

  function candidateByKey(key: string): Candidate | undefined {
    return unassignedBackings().find(c => c.key === key);
  }

  function updateCustomTier(i: number, field: 'name' | 'rank', value: string) {
    setCustomTiers(prev => prev.map((t, idx) => idx === i ? { ...t, [field]: field === 'rank' ? Number(value) : value } : t));
  }

  function addCustomTier() {
    const maxRank = customTiers.reduce((m, t) => Math.max(m, t.rank), 0);
    setCustomTiers(prev => [...prev, { name: '', rank: maxRank + 1 }]);
  }

  function removeCustomTier(i: number) {
    setCustomTiers(prev => prev.filter((_, idx) => idx !== i));
  }

  async function createTier() {
    const name = createName.trim();
    if (!name) return;
    setError('');
    setCreating(true);
    try {
      const tiers = showAdvanced ? customTiers : undefined;
      await api.createTier(name, tiers, metaOnFastest);
      setCreateName('');
      setShowAdvanced(false);
      setCustomTiers([{ name: 'NVME', rank: 1 }, { name: 'SSD', rank: 2 }, { name: 'HDD', rank: 3 }]);
      setMetaOnFastest(false);
      toast.success(t('tiers.toast.created', { name }));
      load(true);
    } catch (e) {
      const msg = extractError(e, t('tiers.error.create'));
      setError(msg);
      toast.error(msg);
    } finally {
      setCreating(false);
    }
  }

  function deleteTier(name: string) {
    setConfirmTitle(t('tiers.confirm.deleteTitle'));
    setConfirmMessage(t('tiers.confirm.deleteMessage', { name }));
    setConfirmClass('btn danger');
    setConfirmText(t('common.delete'));
    confirmAction.current = () => {
      setConfirmVisible(false);
      // Optimistically mark the tier destroying so the dim/spinner
      // appears immediately, even if the API call is slow (e.g. while
      // tierd is tearing down a busy smoothfs mount). Polling reconciles
      // the state once the backend finishes.
      setTiers(prev => prev.map((t: any) =>
        t.name === name ? { ...t, state: 'destroying' } : t));
      startPolling();
      api.deleteTier(name).then(() => {
        load(true);
        invalidatePreload('arrays');
      }).catch(e => {
        // Roll back the optimistic state so the user can retry.
        load(true);
        const msg = extractError(e, t('tiers.error.delete'));
        setError(msg);
        toast.error(msg);
      });
    };
    setConfirmVisible(true);
  }

  function addArrayToTier(tierName: string, slotName: string) {
    const key = `${tierName}:${slotName}`;
    const selectedKey = (addSelections[key] || '').trim();
    if (!selectedKey) return;
    const cand = candidateByKey(selectedKey);
    if (!cand) {
      toast.error(t('tiers.error.backingNotFound', { key: selectedKey }));
      return;
    }
    setConfirmTitle(t('tiers.confirm.assignTitle'));
    setConfirmMessage(t('tiers.confirm.assignMessage', { kind: cand.kind, label: cand.label, slot: slotName, tier: tierName }));
    setConfirmClass('btn primary');
    setConfirmText(t('tiers.button.assign'));
    confirmAction.current = () => {
      setConfirmVisible(false);
      const req = cand.kind === 'mdadm'
        ? api.assignTierArray(tierName, slotName, cand.arrayId!)
        : api.assignTierBacking(tierName, slotName, cand.kind, cand.ref!);
      req.then(() => {
        setAddSelections(p => ({ ...p, [key]: '' }));
        // Optimistically show the slot as assigned and the tier as healthy
        // before the background poll catches up.
        setTiers(prev => prev.map((t: any) => {
          if (t.name !== tierName) return t;
          return {
            ...t,
            state: 'healthy',
            tiers: (t.tiers || []).map((s: any) =>
              s.name === slotName
                ? {
                    ...s,
                    state: 'assigned',
                    backing_kind: cand.kind,
                    backing_ref: cand.ref || cand.arrayPath,
                    pv_device: cand.arrayPath || null,
                    array_id: cand.arrayId || null,
                  }
                : s
            ),
          };
        }));
        load(true);
        invalidatePreload('arrays');
      }).catch(e => {
        const msg = extractError(e, t('tiers.error.assign'));
        setError(msg);
        toast.error(msg);
      });
    };
    setConfirmVisible(true);
  }

  function startEditLevel(tierName: string, level: any) {
    const key = `${tierName}:${level.name}`;
    setEditingLevel(p => ({
      ...p,
      [key]: {
        targetFill: String(level.target_fill_pct ?? ''),
        fullThreshold: String(level.full_threshold_pct ?? ''),
      },
    }));
  }

  function cancelEditLevel(tierName: string, levelName: string) {
    const key = `${tierName}:${levelName}`;
    setEditingLevel(p => { const n = { ...p }; delete n[key]; return n; });
  }

  async function saveLevel(tierName: string, levelName: string) {
    const key = `${tierName}:${levelName}`;
    const edits = editingLevel[key];
    if (!edits) return;
    setSavingLevel(p => ({ ...p, [key]: true }));
    try {
      await api.updateTierLevel(tierName, levelName, {
        target_fill_pct: Number(edits.targetFill),
        full_threshold_pct: Number(edits.fullThreshold),
      });
      toast.success(t('tiers.toast.levelUpdated', { name: levelName }));
      cancelEditLevel(tierName, levelName);
      load(true);
    } catch (e) {
      toast.error(extractError(e, t('tiers.error.updateLevel')));
    } finally {
      setSavingLevel(p => ({ ...p, [key]: false }));
    }
  }

  return (
    <div className="page">
      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText={confirmText}
        confirmClass={confirmClass}
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />

      <div className="page-header">
        <h1>{t('tiers.title')}</h1>
        <p className="subtitle">{t('tiers.subtitle')}</p>
        <button className="refresh-btn" onClick={() => load()} disabled={creating}>{t('common.refresh')}</button>
      </div>

      {error && <div className="error-msg">{error}</div>}

      <div className="create-card">
        <h2>{t('tiers.section.create')}</h2>
        <div className="create-grid">
          <label>
            {t('tiers.field.tierName')}
            <input value={createName} onChange={e => setCreateName(e.target.value)} placeholder="media"
              disabled={creating} />
          </label>
        </div>
        <p className="hint">{t('tiers.create.mountHintPrefix')}<code>/mnt/{createName || '{tiername}'}</code>.</p>
        <div style={{ marginTop: 8, marginBottom: 8 }}>
          <button className="btn" style={{ fontSize: 12 }} onClick={() => setShowAdvanced(v => !v)} disabled={creating}>
            {showAdvanced ? t('tiers.button.advancedExpanded') : t('tiers.button.advancedCollapsed')}
          </button>
        </div>
        {showAdvanced && (
          <div style={{ marginBottom: 12, padding: '10px 12px', background: 'var(--bg-alt, #f5f5f5)', borderRadius: 6 }}>
            <div style={{ fontSize: 12, color: '#666', marginBottom: 8 }}>
              {t('tiers.advanced.description')}
            </div>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 80px 32px', gap: '4px 8px', alignItems: 'center', marginBottom: 6 }}>
              <span style={{ fontSize: 11, color: '#999' }}>{t('datasets.col.name')}</span>
              <span style={{ fontSize: 11, color: '#999' }}>{t('tiers.col.rank')}</span>
              <span />
              {customTiers.map((ct, i) => (
                <>
                  <input key={`n${i}`} value={ct.name} placeholder={t('tiers.field.levelNamePlaceholder')}
                    onChange={e => updateCustomTier(i, 'name', e.target.value)}
                    disabled={creating} style={{ fontSize: 13 }} />
                  <input key={`r${i}`} type="number" min={1} value={ct.rank}
                    onChange={e => updateCustomTier(i, 'rank', e.target.value)}
                    disabled={creating} style={{ fontSize: 13 }} />
                  <button key={`x${i}`} style={{ fontSize: 13, padding: '2px 6px' }}
                    onClick={() => removeCustomTier(i)} disabled={creating || customTiers.length <= 1}>✕</button>
                </>
              ))}
            </div>
            <button style={{ fontSize: 12 }} onClick={addCustomTier} disabled={creating}>{t('tiers.button.addLevel')}</button>

            <div style={{ marginTop: 14, paddingTop: 10, borderTop: '1px solid #ddd' }}>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
                <input
                  type="checkbox"
                  checked={metaOnFastest}
                  onChange={e => setMetaOnFastest(e.target.checked)}
                  disabled={creating}
                />
                {t('tiers.field.metaOnFastest')}
              </label>
              {metaOnFastest && (
                <div style={{ marginTop: 8, padding: '8px 10px', background: '#fff4e5', border: '1px solid #f5c08a', borderRadius: 4, fontSize: 12, color: '#8a4a00' }}>
                  <strong>{t('tiers.warning.label')}</strong> {t('tiers.warning.metaOnFastest')}
                </div>
              )}
            </div>
          </div>
        )}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button className="btn primary" onClick={createTier}
            disabled={creating || !createName.trim()}>
            {creating ? t('tiers.button.creating') : t('tiers.button.createTier')}
          </button>
        </div>
      </div>

      <Spinner loading={loading} />

      {!loading && (
        tiers.length === 0 ? (
          <div className="empty-state">{t('tiers.empty')}</div>
        ) : (
          <div className="tier-grid">
            {tiers.map((tier: any) => {
              const isAsyncProvisioning = tier.state === 'provisioning' && (tier.tiers || []).some((s: any) => s.state !== 'empty');
              return (
                <div key={tier.name} className={`tier-card${tier.state === 'destroying' ? ' tier-deleting' : ''}${isAsyncProvisioning ? ' tier-provisioning' : ''}`}>
                  <div className="tier-head">
                    <h2>{tier.name}</h2>
                    <span className={`state-badge state-${tier.state}`}>{tier.state}</span>
                    {tier.state === 'destroying' ? (
                      <span className="slot-assigning">{t('tiers.status.deleting')}</span>
                    ) : isAsyncProvisioning ? (
                      <span className="slot-assigning">{t('tiers.status.provisioning')}</span>
                    ) : (
                      <button className="btn danger" onClick={() => deleteTier(tier.name)}>{t('common.delete')}</button>
                    )}
                  </div>

                  <div className="row">
                    <span className="label">{t('tiers.row.mountPoint')}</span>
                    <code>{tier.mount_point}</code>
                  </div>
                  <div className="row">
                    <span className="label">{t('arrays.detail.filesystem')}</span>
                    <span>{tier.filesystem}</span>
                  </div>
                  {(() => {
                    const policy = spindownByPool[tier.name];
                    if (!policy) return null;
                    const busy = !!spindownBusy[tier.name];
                    const ssdBalanceMovement = policy.ssd_target_fill?.movement;
                    const ssdBalanceBusy = !!ssdBalanceMovement?.active || (ssdBalanceMovement?.pending_moves ?? 0) > 0;
                    const ssdBalanceExhausted = !!ssdBalanceMovement?.candidate_exhausted;
                    const ssdBalanceReady = (!!policy.ssd_target_fill?.satisfied || ssdBalanceExhausted) && !ssdBalanceBusy;
                    return (
                      <div className="row">
                        <span className="label">{t('tiers.row.spindown')}</span>
                        <span style={{ flex: 1, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', justifyContent: 'flex-end' }}>
                          <span className={`state-badge ${policy.enabled ? 'state-healthy' : policy.eligible ? '' : 'state-degraded'}`}>
                            {policy.enabled ? t('tiers.spindown.enabled') : policy.eligible ? t('tiers.spindown.eligible') : t('tiers.spindown.blocked')}
                          </span>
                          {policy.enabled && policy.active_windows?.length > 0 && (
                            <span className={`state-badge ${policy.active_now ? 'state-healthy' : 'state-degraded'}`}>
                              {policy.active_now ? t('tiers.spindown.windowOpen') : t('tiers.spindown.deferred')}
                            </span>
                          )}
                          {policy.next_active_at && (
                            <span style={{ fontSize: 12, color: '#777' }}>
                              {t('tiers.spindown.next', { when: new Date(policy.next_active_at).toLocaleString() })}
                            </span>
                          )}
                          {policy.ssd_target_fill?.required && (
                            <span style={{ fontSize: 12, color: ssdBalanceReady ? '#666' : '#a65f00', maxWidth: 240, textAlign: 'right' }}>
                              {t('tiers.spindown.ssdBalance', { state: ssdBalanceExhausted && !policy.ssd_target_fill.satisfied ? t('tiers.spindown.bestEffort') : ssdBalanceReady ? t('tiers.spindown.ready') : ssdBalanceBusy ? t('tiers.spindown.moving') : t('tiers.spindown.notAtTarget') })}
                            </span>
                          )}
                          {!policy.eligible && policy.reasons?.length > 0 && (
                            <span style={{ fontSize: 12, color: '#777', maxWidth: 240, textAlign: 'right' }}>
                              {policy.reasons.join('; ')}
                            </span>
                          )}
                          <button
                            className="btn secondary"
                            style={{ fontSize: 11, padding: '2px 8px' }}
                            disabled={busy || (!policy.enabled && !policy.eligible)}
                            onClick={() => setPoolSpindown(tier.name, !policy.enabled)}
                            title={policy.enabled ? t('tiers.spindown.disableTooltip') : t('tiers.spindown.enableTooltip')}
                          >
                            {busy ? t('tiers.spindown.working') : policy.enabled ? t('iscsi.button.disable') : t('iscsi.button.enable')}
                          </button>
                          <button
                            className="btn secondary"
                            style={{ fontSize: 11, padding: '2px 8px' }}
                            disabled={busy}
                            onClick={() => setPoolActiveWindows(tier.name, [{ days: ['daily'], start: '01:00', end: '06:00' }])}
                            title={t('tiers.spindown.nightlyTooltip')}
                          >
                            {t('tiers.spindown.nightly')}
                          </button>
                          <button
                            className="btn secondary"
                            style={{ fontSize: 11, padding: '2px 8px' }}
                            disabled={busy}
                            onClick={() => setPoolActiveWindows(tier.name, [])}
                            title={t('tiers.spindown.anytimeTooltip')}
                          >
                            {t('tiers.spindown.anytime')}
                          </button>
                        </span>
                      </div>
                    );
                  })()}

                  {(() => {
                    const totals = (tier.tiers || []).reduce(
                      (acc: { cap: number; used: number; free: number }, l: any) => ({
                        cap: acc.cap + (l.capacity_bytes || 0),
                        used: acc.used + (l.used_bytes || 0),
                        free: acc.free + (l.free_bytes || 0),
                      }),
                      { cap: 0, used: 0, free: 0 },
                    );
                    if (totals.cap === 0) return null;
                    const pct = (totals.used / totals.cap) * 100;
                    return (
                      <div className="row">
                        <span className="label">{t('tiers.row.storage')}</span>
                        <span style={{ flex: 1 }}>
                          <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12 }}>
                            <span>{t('tiers.storage.usedFree', { used: formatBytes(totals.used), free: formatBytes(totals.free) })}</span>
                            <span style={{ color: '#888' }}>{t('tiers.storage.capacity', { cap: formatBytes(totals.cap), pct: pct.toFixed(1) })}</span>
                          </div>
                          <div style={{ marginTop: 3, height: 6, background: '#e5e5e5', borderRadius: 3, overflow: 'hidden' }}>
                            <div style={{
                              width: `${Math.min(100, pct)}%`,
                              height: '100%',
                              background: pct >= 90 ? '#d9534f' : pct >= 70 ? '#f0ad4e' : '#5cb85c',
                            }} />
                          </div>
                        </span>
                      </div>
                    );
                  })()}

                  {(tier.tiers || []).length > 0 && (
                    <div style={{ marginTop: 12 }}>
                      <div style={{ fontSize: 12, color: '#888', marginBottom: 6, textTransform: 'uppercase', letterSpacing: '0.5px' }}>{t('tiers.section.levels')}</div>
                      <div style={{ fontSize: 12, display: 'grid', gridTemplateColumns: '80px 1fr 1fr 56px 56px 60px', gap: 4, color: '#999', marginBottom: 4 }}>
                        <span>{t('tiers.col.level')}</span><span>{t('tiers.col.capacity')}</span><span>{t('tiers.col.usedFree')}</span><span>{t('tiers.col.fillPct')}</span><span>{t('tiers.col.fullPct')}</span><span></span>
                      </div>
                      {(tier.tiers || []).map((level: any) => {
                        const lkey = `${tier.name}:${level.name}`;
                        const editing = editingLevel[lkey];
                        const saving = savingLevel[lkey];
                        const arr = level.array_id != null ? arrayById(level.array_id) : null;
                        // Display precedence: legacy pv_device (mdadm), then mdadm array path/name,
                        // then the generic backing_ref (zfs/btrfs/bcachefs).
                        const display = level.pv_device || arr?.path || arr?.name
                          || (level.backing_ref ? `${level.backing_kind}:${level.backing_ref}` : null);
                        const addKey = lkey;
                        const opts = unassignedBackings();
                        return (
                          <div key={level.name} className="tier-level-row">
                            <span className="level-name">
                              {level.name}
                              {level.rank != null && (
                                <span style={{ marginLeft: 4, fontSize: 10, background: '#e0e0e0', borderRadius: 8, padding: '1px 5px', color: '#555' }}>#{level.rank}</span>
                              )}
                            </span>
                            <span>{level.capacity_bytes != null ? formatBytes(level.capacity_bytes) : '—'}</span>
                            <span>
                              {level.used_bytes != null ? formatBytes(level.used_bytes) : '—'}
                              {' / '}
                              {level.free_bytes != null ? formatBytes(level.free_bytes) : '—'}
                              {level.capacity_bytes && level.used_bytes != null && level.capacity_bytes > 0 && (
                                <div style={{ marginTop: 2, height: 4, background: '#e5e5e5', borderRadius: 2, overflow: 'hidden' }}
                                     title={t('tiers.storage.usedPctTooltip', { pct: Math.round((level.used_bytes / level.capacity_bytes) * 100) })}>
                                  <div style={{
                                    width: `${Math.min(100, (level.used_bytes / level.capacity_bytes) * 100)}%`,
                                    height: '100%',
                                    background: (level.used_bytes / level.capacity_bytes) >= (level.full_threshold_pct ?? 95) / 100
                                      ? '#d9534f'
                                      : (level.used_bytes / level.capacity_bytes) >= (level.target_fill_pct ?? 50) / 100
                                        ? '#f0ad4e'
                                        : '#5cb85c',
                                  }} />
                                </div>
                              )}
                            </span>
                            {editing ? (
                              <>
                                <input className="level-fill-input" type="number" min={0} max={100}
                                  value={editing.targetFill}
                                  onChange={e => setEditingLevel(p => ({ ...p, [lkey]: { ...p[lkey], targetFill: e.target.value } }))} />
                                <input className="level-fill-input" type="number" min={0} max={100}
                                  value={editing.fullThreshold}
                                  onChange={e => setEditingLevel(p => ({ ...p, [lkey]: { ...p[lkey], fullThreshold: e.target.value } }))} />
                                <div style={{ display: 'flex', gap: 4 }}>
                                  <button style={{ fontSize: 11, padding: '2px 6px' }} onClick={() => saveLevel(tier.name, level.name)} disabled={saving}>{t('common.save')}</button>
                                  <button style={{ fontSize: 11, padding: '2px 6px' }} onClick={() => cancelEditLevel(tier.name, level.name)}>✕</button>
                                </div>
                              </>
                            ) : (
                              <>
                                <span>{level.target_fill_pct ?? '—'}%</span>
                                <span>{level.full_threshold_pct ?? '—'}%</span>
                                <button style={{ fontSize: 11, padding: '2px 6px' }} onClick={() => startEditLevel(tier.name, level)}>{t('common.edit')}</button>
                              </>
                            )}
                            <div style={{ gridColumn: '1 / -1', paddingLeft: 4, paddingBottom: 4 }}>
                              {display ? (
                                <code style={{ fontSize: 11 }}>{display}</code>
                              ) : (
                                <div className="slot-add">
                                  {isAsyncProvisioning ? (
                                    <span className="slot-assigning">{t('tiers.status.assigning')}</span>
                                  ) : (
                                    <>
                                      <select value={addSelections[addKey] || ''}
                                        onChange={e => setAddSelections(p => ({ ...p, [addKey]: e.target.value }))}>
                                        <option value="">{t('tiers.field.unassignedOption')}</option>
                                        {opts.map((c) => (
                                          <option key={c.key} value={c.key}>{c.label}</option>
                                        ))}
                                      </select>
                                      {addSelections[addKey] && (
                                        <button onClick={() => addArrayToTier(tier.name, level.name)}>{t('common.apply')}</button>
                                      )}
                                    </>
                                  )}
                                </div>
                              )}
                            </div>
                          </div>
                        );
                      })}
                    </div>
                  )}

                  {(() => {
                    const ns = namespaces.find((n: any) => n.placement_domain === tier.name && n.backend_kind === 'mdadm');
                    if (!ns) return null;
                    const expanded = expandedFiles[tier.name];
                    return (
                      <div style={{ marginTop: 12, borderTop: '1px solid #e5e5e5', paddingTop: 8 }}>
                        <button
                          className="btn"
                          style={{ fontSize: 12 }}
                          onClick={() => setExpandedFiles(p => ({ ...p, [tier.name]: !p[tier.name] }))}
                        >
                          {expanded ? t('tiers.button.managePinsExpanded') : t('tiers.button.managePinsCollapsed')}
                        </button>
                        {expanded && <NamespaceFiles nsID={ns.id} />}
                      </div>
                    );
                  })()}
                </div>
              );
            })}
          </div>
        )
      )}

    </div>
  );
}
