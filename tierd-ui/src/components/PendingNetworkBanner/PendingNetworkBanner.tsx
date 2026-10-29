import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import './PendingNetworkBanner.css';

// PendingNetworkBanner shows an unconfirmed network change on every page,
// with its countdown and Confirm/Revert, so it cannot be missed by someone
// who is not on the Network page when the change is applied.
export default function PendingNetworkBanner() {
  const { t } = useI18n();
  const [pending, setPending] = useState<any>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    let alive = true;
    const poll = () => api.getPendingChange()
      .then(p => { if (alive) setPending(p || null); })
      .catch(() => {});
    poll();
    const id = setInterval(poll, 2000);
    return () => { alive = false; clearInterval(id); };
  }, []);

  if (!pending) return null;

  const done = () => { setPending(null); setError(''); window.dispatchEvent(new Event('network-change-settled')); };
  return (
    <div className="pending-network-banner" role="alert">
      <span>
        {t('network.pending.message', { seconds: pending.remaining_seconds ?? '?' })}
        {pending.description ? ` (${pending.description})` : ''}
      </span>
      {error && <span className="pending-network-banner-error">{error}</span>}
      <button className="btn primary" onClick={() => api.confirmChange().then(done)
        .catch(e => setError(extractError(e, t('network.error.confirm'))))}>
        {t('network.pending.confirm')}
      </button>
      <button className="btn danger" onClick={() => api.revertChange().then(done)
        .catch(e => setError(extractError(e, t('network.error.revert'))))}>
        {t('network.pending.revert')}
      </button>
    </div>
  );
}
