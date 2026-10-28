import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

// Updates are plain apt: unattended-upgrades installs Debian security fixes and
// everything published to the Smooth* archive as soon as it appears. Some only
// take effect after a restart, which the operator starts from here.
interface RebootStatus {
  required: boolean;
  reasons: string[] | null;
}

export default function Updates() {
  const { t } = useI18n();
  const [status, setStatus] = useState<RebootStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [restarting, setRestarting] = useState(false);

  useEffect(() => {
    loadData();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function loadData() {
    setLoading(true);
    setError('');
    api.getRebootStatus()
      .then((s: RebootStatus) => setStatus(s))
      .catch(e => setError(extractError(e, t('updates.error.status'))))
      .finally(() => setLoading(false));
  }

  function restart() {
    if (!window.confirm(t('updates.confirm.restart'))) return;
    setError('');
    api.reboot()
      .then(() => setRestarting(true))
      .catch(e => setError(extractError(e, t('updates.error.restart'))));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('updates.title')}</h1>
        <p className="subtitle">{t('updates.subtitle')}</p>
        <button className="refresh-btn" onClick={loadData}>{t('common.refresh')}</button>
      </div>

      <div className="section">
        <p style={{ fontSize: 13, color: '#666', marginBottom: 12 }}>{t('updates.label.intro')}</p>
        {error && <div className="error-msg">{error}</div>}
        <Spinner loading={loading} />
        {restarting ? (
          <div className="success">{t('updates.label.restarting')}</div>
        ) : status?.required ? (
          <div style={{ background: '#fef3c7', borderRadius: 8, padding: 16, marginBottom: 12 }}>
            <p style={{ margin: '0 0 8px' }}><strong>{t('updates.label.restartNeeded')}</strong></p>
            <ul style={{ margin: 0 }}>
              {(status.reasons ?? []).map(r => <li key={r}>{r}</li>)}
            </ul>
          </div>
        ) : status && (
          <p style={{ color: '#16a34a', marginBottom: 12 }}>{t('updates.label.noRestart')}</p>
        )}
        <button className={`btn ${status?.required ? 'primary' : 'secondary'}`} onClick={restart} disabled={restarting}>
          {t('updates.button.restart')}
        </button>
      </div>
    </div>
  );
}
