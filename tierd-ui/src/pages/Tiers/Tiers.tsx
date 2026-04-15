import { useEffect, useRef, useState } from 'react';
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
  const { invalidate: invalidatePreload } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [tiers, setTiers] = useState<any[]>([]);
  const [arrays, setArrays] = useState<any[]>([]);
  const [namespaces, setNamespaces] = useState<any[]>([]);
  const [expandedFiles, setExpandedFiles] = useState<Record<string, boolean>>({});
  const [createName, setCreateName] = useState('');
  const [creating, setCreating] = useState(false);
  const [showAdvanced, setShowAdvanced] = useState(false);
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
  const [confirmText, setConfirmText] = useState('Confirm');
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
    const p = Promise.all([api.getTiers(), api.getArrays(), api.getTieringNamespaces().catch(() => [])])
      .then(([ts, arr, nss]) => {
        setTiers(ts || []);
        setArrays(arr || []);
        setNamespaces(nss || []);
        setLoading(false);
        // Auto-poll if any tiers are in a transitional state
        if ((ts || []).some((t: any) => t.state === 'provisioning' || t.state === 'destroying')) {
          startPolling();
        }
      })
      .catch(e => {
        const msg = extractError(e, 'Failed to load');
        setError(msg);
        toast.error(msg);
        setLoading(false);
      });
    return p;
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

  function unassignedArrays(): any[] {
    const assigned = assignedArrayIds();
    return (arrays || []).filter((a: any) => !assigned.has(a.id));
  }

  function arrayByName(name: string) {
    return arrays.find((a: any) => a.name === name || a.path === `/dev/${name}` || a.path === name);
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
      await api.createTier(name, tiers);
      setCreateName('');
      setShowAdvanced(false);
      setCustomTiers([{ name: 'NVME', rank: 1 }, { name: 'SSD', rank: 2 }, { name: 'HDD', rank: 3 }]);
      toast.success(`Tier "${name}" created`);
      load(true);
    } catch (e) {
      const msg = extractError(e, 'Failed to create tier');
      setError(msg);
      toast.error(msg);
    } finally {
      setCreating(false);
    }
  }

  function deleteTier(name: string) {
    setConfirmTitle('Delete Tier');
    setConfirmMessage(`Delete tier "${name}"? This will unmount and destroy the tier storage. This cannot be undone.`);
    setConfirmClass('btn danger');
    setConfirmText('Delete');
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.deleteTier(name).then(() => {
        load(true);
        startPolling();
        invalidatePreload('arrays');
      }).catch(e => {
        const msg = extractError(e, 'Failed to delete tier');
        setError(msg);
        toast.error(msg);
      });
    };
    setConfirmVisible(true);
  }

  function addArrayToTier(tierName: string, slotName: string) {
    const key = `${tierName}:${slotName}`;
    const selected = (addSelections[key] || '').trim();
    if (!selected) return;
    const arr = arrayByName(selected);
    if (!arr) {
      toast.error(`Array "${selected}" not found — try refreshing`);
      return;
    }
    setConfirmTitle('Assign Array');
    setConfirmMessage(`Assign "${selected}" to the ${slotName} slot of tier "${tierName}"?\n\nArray assignments are permanent — this cannot be undone without deleting the tier.`);
    setConfirmClass('btn primary');
    setConfirmText('Assign');
    confirmAction.current = () => {
      setConfirmVisible(false);
      api.assignTierArray(tierName, slotName, arr.id).then(() => {
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
                ? { ...s, state: 'assigned', pv_device: arr.path, array_id: arr.id }
                : s
            ),
          };
        }));
        load(true);
        invalidatePreload('arrays');
      }).catch(e => {
        const msg = extractError(e, 'Failed to assign array to tier');
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
      toast.success(`Level "${levelName}" updated`);
      cancelEditLevel(tierName, levelName);
      load(true);
    } catch (e) {
      toast.error(extractError(e, 'Failed to update level'));
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
        <h1>Tiers</h1>
        <p className="subtitle">Create named storage tiers for the heat engine. Each tier is provisioned at its own mount point automatically.</p>
        <button className="refresh-btn" onClick={() => load()} disabled={creating}>Refresh</button>
      </div>

      {error && <div className="error-msg">{error}</div>}

      <div className="create-card">
        <h2>Create Tier</h2>
        <div className="create-grid">
          <label>
            Tier Name
            <input value={createName} onChange={e => setCreateName(e.target.value)} placeholder="media"
              disabled={creating} />
          </label>
        </div>
        <p className="hint">Tier mount point will be <code>/mnt/{createName || '{tiername}'}</code>.</p>
        <div style={{ marginTop: 8, marginBottom: 8 }}>
          <button className="btn" style={{ fontSize: 12 }} onClick={() => setShowAdvanced(v => !v)} disabled={creating}>
            {showAdvanced ? '▾ Advanced' : '▸ Advanced'}
          </button>
        </div>
        {showAdvanced && (
          <div style={{ marginBottom: 12, padding: '10px 12px', background: 'var(--bg-alt, #f5f5f5)', borderRadius: 6 }}>
            <div style={{ fontSize: 12, color: '#666', marginBottom: 8 }}>
              Define the storage levels for this tier (fastest first). Names must be unique; ranks control hot-to-cold ordering.
            </div>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 80px 32px', gap: '4px 8px', alignItems: 'center', marginBottom: 6 }}>
              <span style={{ fontSize: 11, color: '#999' }}>Name</span>
              <span style={{ fontSize: 11, color: '#999' }}>Rank</span>
              <span />
              {customTiers.map((ct, i) => (
                <>
                  <input key={`n${i}`} value={ct.name} placeholder="e.g. NVME"
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
            <button style={{ fontSize: 12 }} onClick={addCustomTier} disabled={creating}>+ Add Level</button>
          </div>
        )}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button className="btn primary" onClick={createTier}
            disabled={creating || !createName.trim()}>
            {creating ? 'Creating…' : 'Create Tier'}
          </button>
        </div>
      </div>

      <Spinner loading={loading} />

      {!loading && (
        tiers.length === 0 ? (
          <div className="empty-state">No tiers created yet.</div>
        ) : (
          <div className="tier-grid">
            {tiers.map((t: any) => {
              const isAsyncProvisioning = t.state === 'provisioning' && (t.tiers || []).some((s: any) => s.state !== 'empty');
              return (
                <div key={t.name} className={`tier-card${t.state === 'destroying' ? ' tier-deleting' : ''}${isAsyncProvisioning ? ' tier-provisioning' : ''}`}>
                  <div className="tier-head">
                    <h2>{t.name}</h2>
                    <span className={`state-badge state-${t.state}`}>{t.state}</span>
                    {t.state === 'destroying' ? (
                      <span className="slot-assigning">Deleting…</span>
                    ) : isAsyncProvisioning ? (
                      <span className="slot-assigning">Provisioning…</span>
                    ) : (
                      <button className="btn danger" onClick={() => deleteTier(t.name)}>Delete</button>
                    )}
                  </div>

                  <div className="row">
                    <span className="label">Mount Point</span>
                    <code>{t.mount_point}</code>
                  </div>
                  <div className="row">
                    <span className="label">Filesystem</span>
                    <span>{t.filesystem}</span>
                  </div>

                  {(() => {
                    const totals = (t.tiers || []).reduce(
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
                        <span className="label">Storage</span>
                        <span style={{ flex: 1 }}>
                          <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12 }}>
                            <span>{formatBytes(totals.used)} used / {formatBytes(totals.free)} free</span>
                            <span style={{ color: '#888' }}>{formatBytes(totals.cap)} ({pct.toFixed(1)}%)</span>
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

                  {(t.tiers || []).length > 0 && (
                    <div style={{ marginTop: 12 }}>
                      <div style={{ fontSize: 12, color: '#888', marginBottom: 6, textTransform: 'uppercase', letterSpacing: '0.5px' }}>Tier Levels</div>
                      <div style={{ fontSize: 12, display: 'grid', gridTemplateColumns: '80px 1fr 1fr 56px 56px 60px', gap: 4, color: '#999', marginBottom: 4 }}>
                        <span>Level</span><span>Capacity</span><span>Used / Free</span><span>Fill%</span><span>Full%</span><span></span>
                      </div>
                      {(t.tiers || []).map((level: any) => {
                        const lkey = `${t.name}:${level.name}`;
                        const editing = editingLevel[lkey];
                        const saving = savingLevel[lkey];
                        const arr = level.array_id != null ? arrayById(level.array_id) : null;
                        const display = level.pv_device || arr?.path || arr?.name || null;
                        const addKey = lkey;
                        const opts = unassignedArrays();
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
                                     title={`${Math.round((level.used_bytes / level.capacity_bytes) * 100)}% used`}>
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
                                  <button style={{ fontSize: 11, padding: '2px 6px' }} onClick={() => saveLevel(t.name, level.name)} disabled={saving}>Save</button>
                                  <button style={{ fontSize: 11, padding: '2px 6px' }} onClick={() => cancelEditLevel(t.name, level.name)}>✕</button>
                                </div>
                              </>
                            ) : (
                              <>
                                <span>{level.target_fill_pct ?? '—'}%</span>
                                <span>{level.full_threshold_pct ?? '—'}%</span>
                                <button style={{ fontSize: 11, padding: '2px 6px' }} onClick={() => startEditLevel(t.name, level)}>Edit</button>
                              </>
                            )}
                            <div style={{ gridColumn: '1 / -1', paddingLeft: 4, paddingBottom: 4 }}>
                              {display ? (
                                <code style={{ fontSize: 11 }}>{display}</code>
                              ) : (
                                <div className="slot-add">
                                  {isAsyncProvisioning ? (
                                    <span className="slot-assigning">Assigning…</span>
                                  ) : (
                                    <>
                                      <select value={addSelections[addKey] || ''}
                                        onChange={e => setAddSelections(p => ({ ...p, [addKey]: e.target.value }))}>
                                        <option value="">— unassigned —</option>
                                        {opts.map((a: any) => (
                                          <option key={a.id} value={a.name || a.path}>{a.name || a.path}</option>
                                        ))}
                                      </select>
                                      {addSelections[addKey] && (
                                        <button onClick={() => addArrayToTier(t.name, level.name)}>Apply</button>
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
                    const ns = namespaces.find((n: any) => n.placement_domain === t.name && n.backend_kind === 'mdadm');
                    if (!ns) return null;
                    const expanded = expandedFiles[t.name];
                    return (
                      <div style={{ marginTop: 12, borderTop: '1px solid #e5e5e5', paddingTop: 8 }}>
                        <button
                          className="btn"
                          style={{ fontSize: 12 }}
                          onClick={() => setExpandedFiles(p => ({ ...p, [t.name]: !p[t.name] }))}
                        >
                          {expanded ? '▾ Manage pins' : '▸ Manage pins'}
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
