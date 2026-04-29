import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function IscsiTargets() {
  const { t } = useI18n();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [targets, setTargets] = useState<any[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [targetAction, setTargetAction] = useState('');
  // Phase 7.6 — `mode` selects the backstore class. 'block' posts
  // block_device (legacy, paired with ZFS zvols / LVM LVs); 'file'
  // posts backing_file (LIO fileio against a regular file on a
  // mounted filesystem — and auto-pins PIN_LUN on smoothfs mounts
  // per Phase 6.5).
  const [newTarget, setNewTarget] = useState({
    mode: 'block' as 'block' | 'file',
    iqn: '',
    block_device: '',
    backing_file: '',
    chap_user: '',
    chap_pass: '',
  });

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { loadData(); }, []);

  function loadData() {
    setLoading(true);
    api.getIscsiTargets().then(t => { setTargets(t); setLoading(false); })
      .catch(e => { setError(extractError(e, t('iscsi.error.load'))); setLoading(false); });
    api.getProtocols().then(protocols => {
      const p = protocols.find((x: any) => x.name === 'iscsi');
      if (p) setEnabled(p.enabled);
    }).catch(() => {});
  }

  function toggleProtocol() {
    setToggling(true);
    api.toggleProtocol('iscsi', !enabled).then(() => {
      setEnabled(v => !v);
      setToggling(false);
    }).catch(e => { setError(extractError(e, t('iscsi.error.toggle'))); setToggling(false); });
  }

  function create() {
    // Strip the mode flag + the unused backing field before
    // posting; the REST handler rejects setting both block_device
    // and backing_file.
    const payload: any = {
      iqn: newTarget.iqn,
      chap_user: newTarget.chap_user,
      chap_pass: newTarget.chap_pass,
    };
    if (newTarget.mode === 'file') {
      payload.backing_file = newTarget.backing_file;
    } else {
      payload.block_device = newTarget.block_device;
    }
    api.createIscsiTarget(payload).then(() => {
      setShowCreate(false);
      setNewTarget({ mode: 'block', iqn: '', block_device: '', backing_file: '', chap_user: '', chap_pass: '' });
      loadData();
    }).catch(e => setError(extractError(e, t('iscsi.error.create'))));
  }

  function deleteTarget(iqn: string) {
    if (!confirm(t('iscsi.confirm.destroy'))) return;
    api.deleteIscsiTarget(iqn).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.destroy'))));
  }

  function quiesceTarget(iqn: string) {
    setTargetAction(iqn);
    api.quiesceIscsiTarget(iqn).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.quiesce'))))
      .finally(() => setTargetAction(''));
  }

  function resumeTarget(iqn: string) {
    setTargetAction(iqn);
    api.resumeIscsiTarget(iqn).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.resume'))))
      .finally(() => setTargetAction(''));
  }

  function planMove(target: any) {
    const destinationTier = prompt(t('iscsi.prompt.destinationTier'));
    if (!destinationTier?.trim()) return;
    setTargetAction(target.iqn);
    api.createIscsiMoveIntent(target.iqn, destinationTier.trim()).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.recordMove'))))
      .finally(() => setTargetAction(''));
  }

  function clearMoveIntent(iqn: string) {
    setTargetAction(iqn);
    api.clearIscsiMoveIntent(iqn).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.clearMove'))))
      .finally(() => setTargetAction(''));
  }

  function executeMoveIntent(iqn: string) {
    setTargetAction(iqn);
    api.executeIscsiMoveIntent(iqn).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.executeMove'))))
      .finally(() => setTargetAction(''));
  }

  function abortMoveIntent(iqn: string) {
    setTargetAction(iqn);
    api.abortIscsiMoveIntent(iqn).then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.abortMove'))))
      .finally(() => setTargetAction(''));
  }

  // Retry from the `failed` terminal state: abort to drop back to
  // planned, then immediately execute. If the abort step succeeds
  // but execute doesn't, the intent ends up in `planned` and the
  // operator can hit Execute Move directly without re-recording
  // the intent.
  function retryMoveIntent(iqn: string) {
    setTargetAction(iqn);
    api.abortIscsiMoveIntent(iqn)
      .then(() => api.executeIscsiMoveIntent(iqn))
      .then(loadData)
      .catch(e => setError(extractError(e, t('iscsi.error.retryMove'))))
      .finally(() => setTargetAction(''));
  }

  // Phase 8 active-LUN move state machine. `planned` and `completed`
  // are stable; `failed` needs operator action; the rest are
  // transient executor states. Returned as a tuple of (badge class,
  // human label) to keep the badge JSX trivial.
  function moveIntentBadgeClass(state: string): [string, string] {
    switch (state) {
      case 'planned':    return ['info',     t('iscsi.move.state.planned')];
      case 'executing':  return ['active',   t('iscsi.move.state.executing')];
      case 'unpinned':   return ['active',   t('iscsi.move.state.unpinned')];
      case 'moving':     return ['active',   t('iscsi.move.state.moving')];
      case 'cutover':    return ['active',   t('iscsi.move.state.cutover')];
      case 'repinning':  return ['active',   t('iscsi.move.state.repinning')];
      case 'completed':  return ['active',   t('iscsi.move.state.completed')];
      case 'failed':     return ['inactive', t('iscsi.move.state.failed')];
      default:           return ['neutral',  state || t('common.unknown')];
    }
  }

  // States where Abort drops the intent back to `planned`. Backend
  // accepts any in-flight state plus `failed`; we surface only the
  // in-flight cases through the Abort button and route `failed`
  // through the Retry button (one click, abort + execute).
  function moveIntentAbortable(state: string): boolean {
    return state === 'executing' || state === 'unpinned' ||
      state === 'moving' || state === 'cutover' ||
      state === 'repinning';
  }

  function lunPinBadge(target: any) {
    const status = target.lun_pin;
    if (!status) return <span className="badge neutral">{t('iscsi.lunPin.na')}</span>;
    if (status.state === 'pinned') return <span className="badge active">{t('iscsi.lunPin.pinned')}</span>;
    if (status.state === 'not_applicable') return <span className="badge neutral">{t('iscsi.lunPin.na')}</span>;
    const label = status.state === 'missing' ? t('iscsi.lunPin.missing')
      : status.state === 'not_regular' ? t('iscsi.lunPin.invalid')
      : status.state === 'unpinned' ? t('iscsi.lunPin.unpinned')
      : t('common.unknown');
    return <span className="badge inactive" title={status.reason || status.state}>{label}</span>;
  }

  function quiesceBadge(target: any) {
    if ((target.backing_type || 'block') !== 'file') return <span className="badge neutral">{t('iscsi.lunPin.na')}</span>;
    return target.quiesced
      ? <span className="badge active">{t('iscsi.quiesce.quiesced')}</span>
      : <span className="badge neutral">{t('iscsi.quiesce.online')}</span>;
  }

  function moveIntentBadge(target: any) {
    const intent = target.move_intent;
    if (!intent) return <span className="badge neutral">{t('iscsi.move.none')}</span>;
    const [badgeClass, label] = moveIntentBadgeClass(intent.state);
    const tooltipParts = [
      t('iscsi.move.tooltip.state', { state: intent.state }),
      t('iscsi.move.tooltip.destination', { destination: intent.destination_tier }),
    ];
    if (intent.reason) tooltipParts.push(t('iscsi.move.tooltip.reason', { reason: intent.reason }));
    if (intent.state_updated_at) tooltipParts.push(t('iscsi.move.tooltip.updated', { updated: intent.state_updated_at }));
    return (
      <span className={`badge ${badgeClass}`} title={tooltipParts.join('\n')}>
        {label}: {intent.destination_tier}
      </span>
    );
  }

  return (
    <div>
      {error && <div className="error-msg">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, alignItems: 'center' }}>
        <span>{t('iscsi.protocolLabel')} <span className={`badge ${enabled ? 'active' : 'inactive'}`}>{enabled ? t('iscsi.protocol.enabled') : t('iscsi.protocol.disabled')}</span></span>
        <button className="btn secondary" onClick={toggleProtocol} disabled={toggling}>
          {enabled ? t('iscsi.button.disable') : t('iscsi.button.enable')}
        </button>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => setShowCreate(true)} disabled={showCreate}>{t('iscsi.button.addTarget')}</button>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h3>{t('iscsi.create.title')}</h3>
          <div className="form-row" role="radiogroup" aria-label={t('iscsi.create.backstoreLabel')}>
            <label>
              <input type="radio" name="backstore" value="block"
                     checked={newTarget.mode === 'block'}
                     onChange={() => setNewTarget(p => ({ ...p, mode: 'block' }))} />
              {' '}{t('iscsi.create.modeBlock')}
            </label>
            <label>
              <input type="radio" name="backstore" value="file"
                     checked={newTarget.mode === 'file'}
                     onChange={() => setNewTarget(p => ({ ...p, mode: 'file' }))} />
              {' '}{t('iscsi.create.modeFile')}
            </label>
          </div>
          <div className="form-row">
            <label>{t('iscsi.field.iqn')} <input value={newTarget.iqn} onChange={e => setNewTarget(p => ({ ...p, iqn: e.target.value }))} placeholder="iqn.2024-01.com.example:storage" /></label>
            {newTarget.mode === 'block' ? (
              <label>{t('iscsi.field.blockDevice')} <input value={newTarget.block_device} onChange={e => setNewTarget(p => ({ ...p, block_device: e.target.value }))} placeholder="/dev/zvol/tank/vol0" /></label>
            ) : (
              <label>{t('iscsi.field.backingFile')} <input value={newTarget.backing_file} onChange={e => setNewTarget(p => ({ ...p, backing_file: e.target.value }))} placeholder="/mnt/smoothfs/pool0/lun0.img" /></label>
            )}
          </div>
          {newTarget.mode === 'file' && (
            <div className="form-hint" style={{ fontSize: 12, color: '#888', marginBottom: 8 }}>
              {t('iscsi.create.fileHint')}
            </div>
          )}
          <div className="form-row">
            <label>{t('iscsi.field.chapUser')} <input value={newTarget.chap_user} onChange={e => setNewTarget(p => ({ ...p, chap_user: e.target.value }))} /></label>
            <label>{t('iscsi.field.chapPass')} <input type="password" value={newTarget.chap_pass} onChange={e => setNewTarget(p => ({ ...p, chap_pass: e.target.value }))} /></label>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button className="btn secondary" onClick={() => setShowCreate(false)}>{t('common.cancel')}</button>
            <button className="btn primary" onClick={create}
                    disabled={
                      !newTarget.iqn.trim() ||
                      (newTarget.mode === 'block' ? !newTarget.block_device.trim() : !newTarget.backing_file.trim())
                    }>{t('arrays.button.create')}</button>
          </div>
        </div>
      )}

      <Spinner loading={loading} />
      {!loading && (
        targets.length === 0 ? (
          <div className="empty-state"><p>{t('iscsi.empty')}</p></div>
        ) : (
          <table className="data-table">
            <thead><tr><th>{t('iscsi.col.iqn')}</th><th>{t('iscsi.col.backing')}</th><th>{t('iscsi.col.path')}</th><th>{t('iscsi.col.lunPin')}</th><th>{t('iscsi.col.state')}</th><th>{t('iscsi.col.move')}</th><th>{t('arrays.col.actions')}</th></tr></thead>
            <tbody>
              {targets.map((row: any) => {
                // backing_type is new in Phase 7.5; rows persisted
                // before that migration carry no value (the deb
                // auto-fills 'block' at read time via DEFAULT, but
                // guard against any legacy payload).
                const backing = row.backing_type || 'block';
                return (
                  <tr key={row.iqn}>
                    <td><code>{row.iqn}</code></td>
                    <td>
                      <span className={`badge ${backing === 'file' ? 'info' : 'neutral'}`}>
                        {backing === 'file' ? t('iscsi.backing.file') : t('iscsi.backing.block')}
                      </span>
                    </td>
                    <td><code>{row.block_device}</code></td>
                    <td>{lunPinBadge(row)}</td>
                    <td>{quiesceBadge(row)}</td>
                    <td>{moveIntentBadge(row)}</td>
                    <td className="action-cell">
                      {backing === 'file' && (
                        <>
                          <button className="btn secondary" onClick={() => quiesceTarget(row.iqn)} disabled={targetAction === row.iqn || row.quiesced}>{t('iscsi.action.quiesce')}</button>
                          <button className="btn secondary" onClick={() => resumeTarget(row.iqn)} disabled={targetAction === row.iqn || !row.quiesced}>{t('iscsi.action.resume')}</button>
                          <button className="btn secondary" onClick={() => planMove(row)} disabled={targetAction === row.iqn || !row.quiesced || row.lun_pin?.state !== 'pinned' || !!row.move_intent}>{t('iscsi.action.planMove')}</button>
                          <button className="btn secondary" onClick={() => executeMoveIntent(row.iqn)} disabled={targetAction === row.iqn || !row.quiesced || row.lun_pin?.state !== 'pinned' || !row.move_intent || row.move_intent?.state !== 'planned'}>{t('iscsi.action.executeMove')}</button>
                          <button className="btn secondary" onClick={() => abortMoveIntent(row.iqn)} disabled={targetAction === row.iqn || !row.move_intent || !moveIntentAbortable(row.move_intent?.state)}>{t('iscsi.action.abortMove')}</button>
                          <button className="btn secondary" onClick={() => retryMoveIntent(row.iqn)} disabled={targetAction === row.iqn || !row.quiesced || row.lun_pin?.state !== 'pinned' || row.move_intent?.state !== 'failed'}>{t('iscsi.action.retryMove')}</button>
                          <button className="btn secondary" onClick={() => clearMoveIntent(row.iqn)} disabled={targetAction === row.iqn || !row.move_intent}>{t('iscsi.action.clearMove')}</button>
                        </>
                      )}
                      <button className="btn danger" onClick={() => deleteTarget(row.iqn)}>{t('arrays.action.destroy')}</button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )
      )}
    </div>
  );
}
