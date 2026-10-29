import { useEffect, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useI18n } from '@rakuensoftware/smoothgui';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';

// ConfirmNetwork is where the browser lands after a network change moved the
// address it was using. Loading this page at the new address proves the new
// configuration works, so it confirms the pending change with the one-time
// token from the apply response. No session exists yet at the new address,
// which is why the token, not a login, authorises the confirmation.
export default function ConfirmNetwork() {
  const { t } = useI18n();
  const [params] = useSearchParams();
  const [state, setState] = useState<'confirming' | 'confirmed' | 'failed'>('confirming');
  const [error, setError] = useState('');

  useEffect(() => {
    api.confirmChangeWithToken(params.get('token') || '')
      .then(() => setState('confirmed'))
      .catch(e => { setError(extractError(e, t('network.error.confirm'))); setState('failed'); });
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div style={{ maxWidth: 520, margin: '15vh auto', padding: 24, textAlign: 'center' }}>
      <h1>{t('network.confirmPage.title')}</h1>
      {state === 'confirming' && <p>{t('network.confirmPage.confirming')}</p>}
      {state === 'confirmed' && <p>{t('network.confirmPage.confirmed', { host: location.host })}</p>}
      {state === 'failed' && <p className="error-msg">{error}</p>}
      <p><Link to="/login">{t('network.confirmPage.login')}</Link></p>
    </div>
  );
}
