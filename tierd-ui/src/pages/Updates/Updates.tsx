import { useEffect, useRef, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

// Module-level cache persists across page navigations (component unmount/remount).
let cachedUpdateInfo: any = null;
let lastUpdateFetchMs = 0;
const UPDATE_TTL_MS = 60_000;

export default function Updates() {
  const { t } = useI18n();
  const [loadingChannel, setLoadingChannel] = useState(true);
  const [updateInfo, setUpdateInfo] = useState<any>(null);
  const [updateChannel, setUpdateChannel] = useState('stable');
  const [updateChecking, setUpdateChecking] = useState(true);
  const [updateApplying, setUpdateApplying] = useState(false);
  const [updateStage, setUpdateStage] = useState('');
  const [updateError, setUpdateError] = useState('');
  const [pkgApplying, setPkgApplying] = useState(false);
  const [pkgStage, setPkgStage] = useState('');
  const [pkgError, setPkgError] = useState('');
  const [success, setSuccess] = useState('');
  const progressTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const reconnectTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const pkgTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const sawProgressRef = useRef(false);

  useEffect(() => {
    loadData();
    return () => { clearIntervalSafe(progressTimerRef); clearIntervalSafe(reconnectTimerRef); clearIntervalSafe(pkgTimerRef); };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function clearIntervalSafe(ref: React.MutableRefObject<ReturnType<typeof setInterval> | null>) {
    if (ref.current) { clearInterval(ref.current); ref.current = null; }
  }

  function loadData() {
    setLoadingChannel(true);
    api.getUpdateChannel().then((c: any) => { setUpdateChannel(c.channel); setLoadingChannel(false); }).catch(() => setLoadingChannel(false));
    checkForUpdates();
    api.getDebianProgress().then((p: any) => {
      if (p.stage && p.stage !== 'idle' && p.stage !== 'complete' && p.stage !== 'failed') {
        setPkgApplying(true);
        setPkgStage(p.stage);
        pollPackageProgress();
      }
    }).catch(() => {});
  }

  function checkForUpdates(force = false) {
    const age = Date.now() - lastUpdateFetchMs;
    if (!force && cachedUpdateInfo && age < UPDATE_TTL_MS) {
      setUpdateInfo(cachedUpdateInfo);
      setUpdateChecking(false);
      return;
    }
    if (cachedUpdateInfo) setUpdateInfo(cachedUpdateInfo);
    setUpdateChecking(true);
    setUpdateError('');
    api.checkUpdate().then((info: any) => {
      if (!info) {
        setUpdateChecking(false);
        return;
      }
      if (cachedUpdateInfo) {
        if (!info.stable   && cachedUpdateInfo.stable)   info.stable   = cachedUpdateInfo.stable;
        if (!info.testing  && cachedUpdateInfo.testing)  info.testing  = cachedUpdateInfo.testing;
        if (!info.jbailes  && cachedUpdateInfo.jbailes)  info.jbailes  = cachedUpdateInfo.jbailes;
      }
      cachedUpdateInfo = info;
      lastUpdateFetchMs = Date.now();
      setUpdateInfo(info);
      setUpdateChecking(false);
    }).catch((e: any) => {
      if (!cachedUpdateInfo) setUpdateError(extractError(e, t('updates.error.check')));
      setUpdateChecking(false);
    });
  }

  function setChannel(channel: string) {
    api.setUpdateChannel(channel).then(() => {
      setUpdateChannel(channel);
      checkForUpdates(true);
    }).catch(e => setUpdateError(extractError(e, t('updates.error.setChannel'))));
  }

  function channelLabel(channel: string): string {
    if (channel === 'stable') return t('updates.channel.main');
    if (channel === 'testing') return t('updates.channel.testing');
    if (channel === 'jbailes') return t('updates.channel.jbailes');
    return channel;
  }

  function applyUpdate() {
    if (!updateInfo?.latest) return;
    if (!confirm(t('updates.confirm.apply', { version: updateInfo.latest.version }))) return;
    setUpdateApplying(true);
    setUpdateError('');
    setUpdateStage(t('updates.stage.starting'));
    sawProgressRef.current = false;
    api.applyUpdate().then(() => pollProgress())
      .catch(e => { setUpdateError(extractError(e, t('updates.error.startUpdate'))); setUpdateApplying(false); });
  }

  function uploadManualUpdate(event: React.ChangeEvent<HTMLInputElement>) {
    const files = Array.from(event.target.files || []);
    if (files.length !== 3) { setUpdateError(t('updates.validate.selectAll')); return; }
    const manifest = files.find(f => f.name === 'manifest.json');
    const binary = files.find(f => f.name === 'tierd');
    const ui = files.find(f => f.name === 'tierd-ui.tar.gz');
    if (!manifest || !binary || !ui) { setUpdateError(t('updates.validate.expectedFiles')); return; }
    if (!confirm(t('updates.confirm.manual'))) { event.target.value = ''; return; }
    const form = new FormData();
    form.append('manifest', manifest);
    form.append('tierd', binary);
    form.append('ui', ui);
    setUpdateApplying(true);
    setUpdateError('');
    setUpdateStage(t('updates.stage.uploading'));
    sawProgressRef.current = false;
    api.uploadUpdate(form).then(() => pollProgress())
      .catch(e => { setUpdateError(extractError(e, t('updates.error.upload'))); setUpdateApplying(false); event.target.value = ''; });
  }

  function pollProgress() {
    progressTimerRef.current = setInterval(() => {
      api.getUpdateProgress().then((p: any) => {
        setUpdateStage(p.stage);
        if (p.error) {
          setUpdateError(p.error);
          setUpdateApplying(false);
          clearIntervalSafe(progressTimerRef);
        } else if (p.stage === 'restarting') {
          clearIntervalSafe(progressTimerRef);
          setUpdateStage(t('updates.stage.restarting'));
          waitForReconnect();
        } else if (p.stage === 'idle') {
          if (sawProgressRef.current) {
            clearIntervalSafe(progressTimerRef);
            setUpdateStage(t('updates.stage.restarting'));
            waitForReconnect();
          } else {
            setUpdateError(t('updates.error.stoppedUnexpectedly'));
            setUpdateApplying(false);
            clearIntervalSafe(progressTimerRef);
          }
        } else {
          sawProgressRef.current = true;
        }
      }).catch(() => {
        clearIntervalSafe(progressTimerRef);
        setUpdateStage(t('updates.stage.reconnecting'));
        waitForReconnect();
      });
    }, 2000);
  }

  function waitForReconnect() {
    reconnectTimerRef.current = setInterval(() => {
      api.getHealth().then(() => {
        clearIntervalSafe(reconnectTimerRef);
        setUpdateApplying(false);
        setSuccess(t('updates.toast.complete'));
        setTimeout(() => window.location.reload(), 500);
      }).catch(() => {});
    }, 3000);
  }

  function applyPackageUpdates() {
    if (!confirm(t('updates.confirm.packages'))) return;
    setPkgApplying(true);
    setPkgError('');
    setPkgStage(t('updates.pkg.starting'));
    api.applyDebianPackages().then(() => pollPackageProgress())
      .catch(e => { setPkgError(extractError(e, t('updates.error.startPackages'))); setPkgApplying(false); });
  }

  function pollPackageProgress() {
    pkgTimerRef.current = setInterval(() => {
      api.getDebianProgress().then((p: any) => {
        setPkgStage(p.stage);
        if (p.error) {
          setPkgError(p.error);
          setPkgApplying(false);
          clearIntervalSafe(pkgTimerRef);
        } else if (p.stage === 'complete') {
          setPkgApplying(false);
          setSuccess(t('updates.toast.packagesComplete'));
          clearIntervalSafe(pkgTimerRef);
        } else if (p.stage === 'failed') {
          setPkgApplying(false);
          clearIntervalSafe(pkgTimerRef);
        }
      }).catch(e => { setPkgError(extractError(e, t('updates.error.getProgress'))); setPkgApplying(false); clearIntervalSafe(pkgTimerRef); });
    }, 2000);
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('updates.title')}</h1>
        <p className="subtitle">{t('updates.subtitle')}</p>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      {success && <div className="success">{success}</div>}

      <div className="section">
        <h2>{t('updates.section.smoothnas')}</h2>
        <Spinner loading={loadingChannel} />
        {!loadingChannel && (
          <>
            <div style={{ marginBottom: 16 }}>
              <div style={{ fontSize: 13, color: '#666', marginBottom: 8 }}>{t('updates.label.channel')}</div>
              <div style={{ display: 'flex', gap: 8 }}>
                {(['stable', 'testing', ...(updateChannel === 'jbailes' || !!updateInfo?.jbailes ? ['jbailes'] : [])] as string[]).map(channel => (
                  <button key={channel} className={`btn ${updateChannel === channel ? 'primary' : 'secondary'}`} onClick={() => setChannel(channel)}>
                    {channelLabel(channel)}
                  </button>
                ))}
              </div>
            </div>

            {updateError && <div className="error-msg">{updateError}</div>}
            {updateInfo && (() => {
              const chInfo = updateInfo[updateChannel as 'stable' | 'testing' | 'jbailes'];
              const responseMatchesChannel = updateInfo.channel === updateChannel;
              const available = responseMatchesChannel && updateInfo.available;
              return (
                <div style={{ background: '#f5f5f5', borderRadius: 8, padding: 16 }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 12, flexWrap: 'wrap', gap: 8 }}>
                    <span style={{ fontSize: 13, color: '#666' }}>
                      {t('updates.label.running')} <strong>v{updateInfo.current_version}</strong>
                    </span>
                    {chInfo && (
                      <span style={{ fontSize: 13, color: '#666' }}>
                        {t('updates.label.latest', { channel: channelLabel(updateChannel) })} <strong>v{chInfo.version}</strong>
                      </span>
                    )}
                    {updateChecking && <span style={{ fontSize: 12, color: '#aaa' }}>{t('updates.label.checking')}</span>}
                  </div>
                  {available ? (
                    <>
                      <p style={{ marginBottom: 8 }}>
                        {t('updates.label.availablePrefix')} v{updateInfo.current_version} → <strong>v{updateInfo.latest?.version}</strong>
                      </p>
                      {updateInfo.latest?.body && (
                        <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{updateInfo.latest.body}</p>
                      )}
                      <button className="btn primary" onClick={applyUpdate} disabled={updateApplying}>
                        {updateApplying ? updateStage || t('updates.button.updating') : t('updates.button.applyUpdate')}
                      </button>
                    </>
                  ) : responseMatchesChannel ? (
                    <p style={{ color: '#16a34a', margin: 0 }}>{t('updates.label.upToDate')}</p>
                  ) : (
                    <p style={{ fontSize: 13, color: '#aaa', margin: 0 }}>{t('updates.label.checkingChannel')}</p>
                  )}
                </div>
              );
            })()}
            {!updateInfo && updateChecking && <Spinner loading />}

            <div style={{ marginTop: 16 }}>
              <div style={{ fontSize: 13, color: '#666', marginBottom: 8 }}>{t('updates.label.manualUpdate')}</div>
              <p style={{ fontSize: 13, color: '#888', marginBottom: 8 }}>{t('updates.label.manualHint')}</p>
              <input type="file" multiple accept=".json,.gz,*" onChange={uploadManualUpdate} disabled={updateApplying} />
            </div>
          </>
        )}
      </div>

      <div className="section">
        <h2>{t('updates.section.packages')}</h2>
        <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{t('updates.label.pkgIntro')}</p>
        {pkgError && <div className="error-msg">{pkgError}</div>}
        <button className="btn primary" onClick={applyPackageUpdates} disabled={pkgApplying}>
          {pkgApplying ? pkgStage || t('updates.pkg.updating') : t('updates.button.updatePackages')}
        </button>
      </div>
    </div>
  );
}
