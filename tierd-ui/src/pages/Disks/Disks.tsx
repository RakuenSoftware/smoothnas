import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { useToast } from '../../contexts/ToastContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import { pollJob } from '../../utils/poll';
import Spinner from '../../components/Spinner/Spinner';
import ConfirmDialog from '../../components/ConfirmDialog/ConfirmDialog';

export default function Disks() {
  const { t } = useI18n();
  const { disks, invalidate } = usePreload();
  const toast = useToast();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [wipingDisks, setWipingDisks] = useState(new Set<string>());
  const [powerByDisk, setPowerByDisk] = useState<Record<string, any>>({});
  const [timerInputs, setTimerInputs] = useState<Record<string, string>>({});
  const [powerBusy, setPowerBusy] = useState(new Set<string>());
  const [confirmVisible, setConfirmVisible] = useState(false);
  const [confirmTitle, setConfirmTitle] = useState('');
  const [confirmMessage, setConfirmMessage] = useState('');
  const confirmAction = useRef<(() => void) | null>(null);
  const stopPollRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    setLoading(false);
    return () => { stopPollRef.current?.(); };
  }, []);

  useEffect(() => {
    if (disks.length > 0 || !loading) setLoading(false);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [disks]);

  useEffect(() => {
    if (disks.length === 0) return;
    Promise.all(
      disks.map((disk: any) =>
        api.getDiskPower(disk.name)
          .then((status: any) => ({ name: disk.name, status }))
          .catch(() => ({ name: disk.name, status: null }))
      )
    ).then(rows => {
      const next: Record<string, any> = {};
      const timers: Record<string, string> = {};
      for (const row of rows) {
        if (row.status) {
          next[row.name] = row.status;
          timers[row.name] = row.status.timer_minutes ? String(row.status.timer_minutes) : '30';
        }
      }
      setPowerByDisk(next);
      setTimerInputs(prev => ({ ...timers, ...prev }));
    });
  }, [disks]);

  function refresh() {
    setLoading(true);
    invalidate('disks');
    setTimeout(() => setLoading(false), 500);
  }

  function identify(name: string) {
    api.identifyDisk(name).catch(e => setError(extractError(e, t('disks.error.identify'))));
  }

  function wipeDisk(disk: any) {
    setConfirmTitle(t('disks.wipe.title'));
    setConfirmMessage(t('disks.wipe.message', { path: disk.path }));
    confirmAction.current = () => {
      setConfirmVisible(false);
      setWipingDisks(prev => new Set(prev).add(disk.name));
      api.wipeDisk(disk.name).then((res: any) => {
        stopPollRef.current = pollJob(
          res.job_id,
          null,
          () => {
            setWipingDisks(prev => { const s = new Set(prev); s.delete(disk.name); return s; });
            toast.success(t('disks.toast.wiped'));
            invalidate('disks');
          },
          (err) => {
            setWipingDisks(prev => { const s = new Set(prev); s.delete(disk.name); return s; });
            toast.error(err);
          }
        );
      }).catch(e => {
        setWipingDisks(prev => { const s = new Set(prev); s.delete(disk.name); return s; });
        toast.error(extractError(e, t('disks.error.wipe')));
      });
    };
    setConfirmVisible(true);
  }

  function setPowerStatus(diskName: string, status: any) {
    setPowerByDisk(prev => ({ ...prev, [diskName]: status }));
    setTimerInputs(prev => ({ ...prev, [diskName]: status.timer_minutes ? String(status.timer_minutes) : prev[diskName] || '30' }));
  }

  function configureSpindown(disk: any, enabled: boolean) {
    const minutes = Number(timerInputs[disk.name] || '30');
    setPowerBusy(prev => new Set(prev).add(disk.name));
    api.setDiskSpindown(disk.name, enabled, minutes)
      .then((status: any) => {
        setPowerStatus(disk.name, status);
        toast.success(enabled ? t('disks.toast.spindownSet') : t('disks.toast.spindownDisabled'));
      })
      .catch(e => toast.error(extractError(e, t('disks.error.spindown'))))
      .finally(() => setPowerBusy(prev => { const s = new Set(prev); s.delete(disk.name); return s; }));
  }

  function standbyNow(disk: any) {
    setPowerBusy(prev => new Set(prev).add(disk.name));
    api.standbyDisk(disk.name)
      .then((status: any) => {
        setPowerStatus(disk.name, status);
        toast.success(t('disks.toast.standby'));
      })
      .catch(e => toast.error(extractError(e, t('disks.error.standby'))))
      .finally(() => setPowerBusy(prev => { const s = new Set(prev); s.delete(disk.name); return s; }));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('disks.title')}</h1>
        <p className="subtitle">{t('disks.subtitle')}</p>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      {error && <div className="error-msg">{error}</div>}
      <Spinner loading={loading} text={t('disks.loading')} />

      {!loading && (
        disks.length === 0 ? (
          <div className="empty-state">
            <div className="empty-icon">⊙</div>
            <p>{t('disks.empty')}</p>
          </div>
        ) : (
          <table className="data-table">
            <thead>
              <tr>
                <th>{t('disks.col.device')}</th>
                <th>{t('disks.col.model')}</th>
                <th>{t('disks.col.size')}</th>
                <th>{t('disks.col.type')}</th>
                <th>{t('disks.col.assignment')}</th>
                <th>{t('disks.col.power')}</th>
                <th>{t('disks.col.temp')}</th>
                <th>{t('disks.col.health')}</th>
                <th>{t('disks.col.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {disks.map((disk: any) => {
                const power = powerByDisk[disk.name];
                const busy = powerBusy.has(disk.name);
                const eligible = power?.eligible;
                return (
                  <tr key={disk.name}>
                    <td><code>{disk.path}</code></td>
                    <td>{disk.model || '—'}</td>
                    <td>{disk.size_human}</td>
                    <td><span className={`badge ${disk.type?.toLowerCase()}`}>{disk.type}</span></td>
                    <td><span className="badge assignment">{disk.assignment || t('disks.assignment.unassigned')}</span></td>
                    <td>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: 4, minWidth: 150 }}>
                        <span className={`badge ${power?.state === 'standby' ? 'healthy' : ''}`}>
                          {power?.state || t('common.unknown')}
                        </span>
                        {eligible ? (
                          <>
                            <span style={{ fontSize: 12, color: '#666' }}>
                              {power?.timer_minutes
                                ? t('disks.power.timerMinutes', { minutes: power.timer_minutes })
                                : t('disks.power.timerOff')}
                            </span>
                            {power?.observed_since && (
                              <span style={{ fontSize: 12, color: '#666' }}>
                                {t('disks.power.standbyPct', { pct: Math.round(power.time_in_standby_pct || 0) })}
                              </span>
                            )}
                            {power?.last_wake_at && (
                              <span style={{ fontSize: 12, color: '#777' }}>
                                {t('disks.power.lastWake', {
                                  reason: power.last_wake_reason || t('disks.power.wakeReasonObserved'),
                                  when: new Date(power.last_wake_at).toLocaleString(),
                                })}
                              </span>
                            )}
                          </>
                        ) : (
                          <span style={{ fontSize: 12, color: '#777' }}>
                            {power?.ineligible_reason || t('disks.power.notEligible')}
                          </span>
                        )}
                      </div>
                    </td>
                    <td>{disk.temp_c != null ? `${disk.temp_c}°C` : '—'}</td>
                    <td>
                      {disk.smart_status && (
                        <span className={`badge ${disk.smart_status === 'PASSED' ? 'healthy' : 'critical'}`}>
                          {disk.smart_status}
                        </span>
                      )}
                    </td>
                    <td className="action-cell">
                      <button className="btn secondary" onClick={() => identify(disk.name)} title={t('disks.actions.identify.title')}>
                        {t('disks.actions.identify')}
                      </button>
                      {' '}
                      <input
                        type="number"
                        min="1"
                        max="330"
                        value={timerInputs[disk.name] || '30'}
                        onChange={e => setTimerInputs(prev => ({ ...prev, [disk.name]: e.target.value }))}
                        disabled={!eligible || busy}
                        style={{ width: 64, padding: '4px 6px', marginRight: 4 }}
                        title={t('disks.actions.spindownInput.title')}
                      />
                      <button
                        className="btn secondary"
                        disabled={!eligible || busy}
                        onClick={() => configureSpindown(disk, true)}
                      >
                        {t('disks.actions.set')}
                      </button>
                      {' '}
                      <button
                        className="btn secondary"
                        disabled={!eligible || busy}
                        onClick={() => configureSpindown(disk, false)}
                      >
                        {t('disks.actions.off')}
                      </button>
                      {' '}
                      <button
                        className="btn secondary"
                        disabled={!eligible || busy}
                        onClick={() => standbyNow(disk)}
                      >
                        {t('disks.actions.standby')}
                      </button>
                      {' '}
                      <button
                        className="btn danger"
                        onClick={() => wipeDisk(disk)}
                        disabled={wipingDisks.has(disk.name) || disk.assignment === 'os'}
                        title={disk.assignment === 'os' ? t('disks.actions.wipe.osBlocked') : t('disks.actions.wipe.title')}
                      >
                        {wipingDisks.has(disk.name) ? t('disks.actions.wiping') : t('disks.actions.wipe')}
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )
      )}

      <ConfirmDialog
        visible={confirmVisible}
        title={confirmTitle}
        message={confirmMessage}
        confirmText={t('disks.actions.wipe')}
        confirmClass="btn danger"
        onConfirm={() => confirmAction.current?.()}
        onCancel={() => setConfirmVisible(false)}
      />
    </div>
  );
}
