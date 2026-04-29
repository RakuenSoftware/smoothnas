import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

// Phase 7.8 — operator view + create-form for smoothfs pool
// lifecycle. The POST surface is Phase 7.7's SmoothfsHandler; this
// page just drives it. Pool deletion stops + removes the systemd
// mount unit via DELETE; Phase 2.5's auto-discovery handles the
// inverse (new pools appearing) the moment systemd brings the
// mount up.
export default function SmoothfsPools() {
  const { t } = useI18n();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [pools, setPools] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ name: '', tiersText: '', uuid: '' });
  // Phase 7.9 — movement-log + quiesce/reconcile surface.
  const [movementLog, setMovementLog] = useState<any[]>([]);
  const [logLoading, setLogLoading] = useState(false);
  const [busyPool, setBusyPool] = useState('');
  const [stagingByPool, setStagingByPool] = useState<Record<string, any>>({});

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { loadData(); loadMovementLog(); }, []);

  function loadData() {
    setLoading(true);
    api.getSmoothfsPools()
      .then(ps => {
        const rows = ps || [];
        setPools(rows);
        setLoading(false);
        loadWriteStaging(rows);
      })
      .catch(e => { setError(extractError(e, t('smoothfs.error.loadPools'))); setLoading(false); });
  }

  function loadWriteStaging(poolList = pools) {
    Promise.all(
      (poolList || []).map((p: any) =>
        api.getSmoothfsWriteStaging(p.name)
          .then((status: any) => ({ name: p.name, status }))
          .catch(() => ({ name: p.name, status: null }))
      )
    ).then(rows => {
      setStagingByPool(prev => {
        const next = { ...prev };
        for (const row of rows) {
          if (row.status) next[row.name] = row.status;
        }
        return next;
      });
    });
  }

  function loadMovementLog() {
    setLogLoading(true);
    api.getSmoothfsMovementLog(100, 0)
      .then(entries => { setMovementLog(entries || []); setLogLoading(false); })
      .catch(e => { setError(extractError(e, t('smoothfs.error.loadLog'))); setLogLoading(false); });
  }

  function quiesce(name: string) {
    setBusyPool(name);
    api.quiesceSmoothfsPool(name)
      .then(() => { setError(''); loadMovementLog(); })
      .catch(e => setError(extractError(e, t('smoothfs.error.quiesce'))))
      .finally(() => setBusyPool(''));
  }

  function reconcile(name: string) {
    const reason = prompt(t('smoothfs.prompt.reconcile', { name })) || '';
    setBusyPool(name);
    api.reconcileSmoothfsPool(name, reason)
      .then(() => { setError(''); loadMovementLog(); })
      .catch(e => setError(extractError(e, t('smoothfs.error.reconcile'))))
      .finally(() => setBusyPool(''));
  }

  function setWriteStaging(name: string, enabled: boolean) {
    setBusyPool(name);
    const current = stagingByPool[name];
    const fullPct = Number(current?.full_threshold_pct || 98);
    api.setSmoothfsWriteStaging(name, enabled, fullPct)
      .then((status: any) => {
        setStagingByPool(prev => ({ ...prev, [name]: status }));
        setError('');
      })
      .catch(e => setError(extractError(e, t('smoothfs.error.writeStaging'))))
      .finally(() => setBusyPool(''));
  }

  function refreshMetadataMask(name: string) {
    setBusyPool(name);
    api.refreshSmoothfsMetadataActiveMask(name)
      .then((status: any) => {
        setStagingByPool(prev => ({ ...prev, [name]: status }));
        setError('');
      })
      .catch(e => setError(extractError(e, t('smoothfs.error.metadataMask'))))
      .finally(() => setBusyPool(''));
  }

  function parseTiers(input: string): string[] {
    // Accept either newline-separated or colon-separated input —
    // colon matches the kernel's tiers= mount-option spelling so
    // experienced operators can paste it straight in; newline is
    // what the textarea produces by default.
    return input
      .split(/[\n:]/)
      .map(s => s.trim())
      .filter(s => s.length > 0);
  }

  function create() {
    const tiers = parseTiers(form.tiersText);
    if (!form.name.trim() || tiers.length === 0) {
      setError(t('smoothfs.validate.nameTiers'));
      return;
    }
    setCreating(true);
    const payload: { name: string; tiers: string[]; uuid?: string } = {
      name: form.name.trim(),
      tiers,
    };
    if (form.uuid.trim()) {
      payload.uuid = form.uuid.trim();
    }
    api.createSmoothfsPool(payload)
      .then(() => {
        setShowCreate(false);
        setForm({ name: '', tiersText: '', uuid: '' });
        setError('');
        loadData();
      })
      .catch(e => setError(extractError(e, t('smoothfs.error.createPool'))))
      .finally(() => setCreating(false));
  }

  function destroy(name: string) {
    if (!confirm(t('smoothfs.confirm.destroy', { name }))) return;
    api.deleteSmoothfsPool(name)
      .then(loadData)
      .catch(e => setError(extractError(e, t('smoothfs.error.destroyPool'))));
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <h2 style={{ margin: 0 }}>{t('smoothfs.section.pools')}</h2>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('smoothfs.button.createPool')}</button>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('smoothfs.create.title')}</h3>
          <div className="form-row">
            <label>{t('datasets.col.name')} <input value={form.name} onChange={e => setForm(p => ({ ...p, name: e.target.value }))} placeholder="tank" /></label>
            <label>{t('smoothfs.field.uuid')}
              <input value={form.uuid} onChange={e => setForm(p => ({ ...p, uuid: e.target.value }))}
                     placeholder={t('smoothfs.field.uuidPlaceholder')} />
            </label>
          </div>
          <div className="form-row">
            <label style={{ flex: 1 }}>
              {t('smoothfs.field.tierPaths')}
              <textarea rows={4} value={form.tiersText}
                        onChange={e => setForm(p => ({ ...p, tiersText: e.target.value }))}
                        placeholder={"/mnt/nvme-tier\n/mnt/sas-tier"}
                        style={{ fontFamily: 'monospace', width: '100%' }} />
            </label>
          </div>
          <div className="form-hint" style={{ fontSize: 12, color: '#888', marginBottom: 8 }}>
            {t('smoothfs.create.hint')}
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)} disabled={creating}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create}
                    disabled={creating || !form.name.trim() || parseTiers(form.tiersText).length === 0}>
              {creating ? t('tiers.button.creating') : t('arrays.button.create')}
            </button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        pools.length === 0 ? (
          <div className="empty-state"><p>{t('smoothfs.empty.pools')}</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>{t('datasets.col.name')}</th><th>{t('smoothfs.col.uuid')}</th><th>{t('smoothfs.col.tiers')}</th><th>{t('smoothfs.col.mountpoint')}</th><th>{t('smoothfs.col.writeStaging')}</th><th>{t('arrays.col.actions')}</th></tr></thead>
            <tbody>
              {pools.map((p: any) => {
                const staging = stagingByPool[p.name];
                return (
                  <tr key={p.uuid}>
                    <td><code>{p.name}</code></td>
                    <td><code style={{ fontSize: 11 }}>{p.uuid}</code></td>
                    <td>
                      {(p.tiers || []).map((t: string, i: number) => (
                        <div key={i}><code>{t}</code></div>
                      ))}
                    </td>
                    <td><code>{p.mountpoint}</code></td>
                    <td>
                      {staging && (
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 4, minWidth: 180 }}>
                          <span className={`state-badge ${staging.effective_enabled ? 'state-healthy' : staging.desired_enabled ? 'state-degraded' : ''}`}>
                            {staging.effective_enabled ? t('smoothfs.staging.active') : staging.desired_enabled ? t('smoothfs.staging.waitingForKernel') : t('smoothfs.staging.off')}
                          </span>
                          <span style={{ fontSize: 12, color: '#666' }}>
                            {t('smoothfs.staging.stagedBytes', { bytes: Number(staging.staged_bytes || 0).toLocaleString() })}
                          </span>
                          {staging.staged_rehome_bytes > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.rehomeBytes', { bytes: Number(staging.staged_rehome_bytes || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.range_staged_bytes > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.rangeBytes', { bytes: Number(staging.range_staged_bytes || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.range_staged_writes > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.rangeWrites', { count: Number(staging.range_staged_writes || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.range_staging_recovery_supported && (
                            <span style={{ fontSize: 12, color: '#3a7' }}>
                              {t('smoothfs.staging.rangeRecoverySupported')}
                            </span>
                          )}
                          {staging.range_staging_recovered_writes > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.recoveredRangeWrites', { count: Number(staging.range_staging_recovered_writes || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.range_staging_recovered_bytes > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.recoveredRangeBytes', { bytes: Number(staging.range_staging_recovered_bytes || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.range_staging_recovery_pending > 0 && (
                            <span style={{ fontSize: 12, color: '#a65f00' }}>
                              {t('smoothfs.staging.recoveryPending', { bytes: Number(staging.range_staging_recovery_pending || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.oldest_recovered_write_at && (
                            <span style={{ fontSize: 12, color: '#777' }}>
                              {t('smoothfs.staging.oldestRecovered', { when: staging.oldest_recovered_write_at })}
                            </span>
                          )}
                          {staging.last_recovery_at && (
                            <span style={{ fontSize: 12, color: '#777' }}>
                              {staging.last_recovery_reason
                                ? t('smoothfs.staging.lastRecoveryReason', { when: staging.last_recovery_at, reason: staging.last_recovery_reason })
                                : t('smoothfs.staging.lastRecovery', { when: staging.last_recovery_at })}
                            </span>
                          )}
                          {staging.staged_rehomes_total > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.rehomedFiles', { count: Number(staging.staged_rehomes_total || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.staged_rehomes_pending > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.pendingRehomes', { count: Number(staging.staged_rehomes_pending || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.write_staging_drain_pressure && (
                            <span style={{ fontSize: 12, color: '#a65f00' }}>
                              {t('smoothfs.staging.drainPressure')}
                            </span>
                          )}
                          {staging.write_staging_drainable_tier_mask > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.drainable', { mask: Number(staging.write_staging_drainable_tier_mask).toString(16) })}
                            </span>
                          )}
                          {staging.write_staging_drainable_rehomes > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.drainableRehomes', { count: Number(staging.write_staging_drainable_rehomes || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.recovered_range_tier_mask > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.recoveredRanges', { mask: Number(staging.recovered_range_tier_mask).toString(16) })}
                            </span>
                          )}
                          {staging.full_threshold_pct > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.ssdFullPct', { pct: staging.full_threshold_pct })}
                            </span>
                          )}
                          {staging.metadata_active_tier_mask > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.metadataMask', { mask: Number(staging.metadata_active_tier_mask).toString(16) })}
                            </span>
                          )}
                          {staging.write_staging_drain_active_tier_mask > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.drainMask', { mask: Number(staging.write_staging_drain_active_tier_mask).toString(16) })}
                            </span>
                          )}
                          {staging.recommended_metadata_active_tier_mask > 0 && staging.recommended_metadata_active_tier_mask !== staging.metadata_active_tier_mask && (
                            <span style={{ fontSize: 12, color: '#777' }}>
                              {t('smoothfs.staging.recommendedMask', { mask: Number(staging.recommended_metadata_active_tier_mask).toString(16) })}
                            </span>
                          )}
                          {staging.recommended_drain_active_tier_mask > 0 && staging.recommended_drain_active_tier_mask !== staging.write_staging_drain_active_tier_mask && (
                            <span style={{ fontSize: 12, color: '#777' }}>
                              {t('smoothfs.staging.recommendedDrainMask', { mask: Number(staging.recommended_drain_active_tier_mask).toString(16) })}
                            </span>
                          )}
                          {staging.metadata_tier_skips > 0 && (
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {t('smoothfs.staging.skippedProbes', { count: Number(staging.metadata_tier_skips || 0).toLocaleString() })}
                            </span>
                          )}
                          {staging.metadata_active_mask_reason && <span style={{ fontSize: 12, color: '#777' }}>{staging.metadata_active_mask_reason}</span>}
                          {staging.reason && <span style={{ fontSize: 12, color: '#777' }}>{staging.reason}</span>}
                        </div>
                      )}
                    </td>
                    <td className="action-cell">
                      {staging && (
                        <>
                          <button className="btn secondary" onClick={() => setWriteStaging(p.name, !staging.desired_enabled)}
                                  disabled={busyPool === p.name}>
                            {staging.desired_enabled ? t('smoothfs.button.disableStaging') : t('smoothfs.button.enableStaging')}
                          </button>
                          <button className="btn secondary" onClick={() => refreshMetadataMask(p.name)}
                                  disabled={busyPool === p.name || !staging.kernel_supported}>
                            {t('smoothfs.button.refreshMask')}
                          </button>
                          {' '}
                        </>
                      )}
                      <button className="btn secondary" onClick={() => quiesce(p.name)}
                              disabled={busyPool === p.name}>{t('iscsi.action.quiesce')}</button>
                      <button className="btn secondary" onClick={() => reconcile(p.name)}
                              disabled={busyPool === p.name}>{t('smoothfs.button.reconcile')}</button>
                      <button className="btn danger" onClick={() => destroy(p.name)}
                              disabled={busyPool === p.name}>{t('arrays.action.destroy')}</button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )
      )}

      <div style={{ marginTop: 32, display: 'flex', alignItems: 'center', gap: 8 }}>
        <h2 style={{ margin: 0 }}>{t('smoothfs.section.movementLog')}</h2>
        <div style={{ flex: 1 }} />
        <button className="refresh-btn" onClick={loadMovementLog} disabled={logLoading}>
          {logLoading ? t('common.loading') : t('common.refresh')}
        </button>
      </div>
      <p style={{ fontSize: 12, color: '#888' }}>
        {t('smoothfs.movementLog.hint')}
      </p>
      {movementLog.length === 0 ? (
        <div className="empty-state"><p>{t('smoothfs.empty.movementLog')}</p></div>
      ) : (
        <table className="data-table">
          <thead><tr>
            <th>{t('smoothfs.col.when')}</th><th>{t('smoothfs.col.seq')}</th><th>{t('smoothfs.col.object')}</th><th>{t('smoothfs.col.fromTo')}</th><th>{t('smoothfs.col.sourceDest')}</th>
          </tr></thead>
          <tbody>
            {movementLog.map((e: any) => (
              <tr key={e.id}>
                <td><code style={{ fontSize: 11 }}>{e.written_at}</code></td>
                <td>{e.transaction_seq}</td>
                <td><code style={{ fontSize: 11 }}>{String(e.object_id).slice(0, 16)}…</code></td>
                <td>
                  {e.from_state && <><code>{e.from_state}</code> → </>}
                  <code>{e.to_state}</code>
                </td>
                <td>
                  {e.source_tier && <code>{e.source_tier}</code>}
                  {e.source_tier && e.dest_tier && ' → '}
                  {e.dest_tier && <code>{e.dest_tier}</code>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
