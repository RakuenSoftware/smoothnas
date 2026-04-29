import { useEffect, useState } from 'react';
import { useI18n } from '@rakuensoftware/smoothgui';
import { usePreload } from '../../contexts/PreloadContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function Smart() {
  const { t } = useI18n();
  const { alarmHistory, invalidate } = usePreload();
  const [loadingRules, setLoadingRules] = useState(true);
  const [error, setError] = useState('');
  const [alarmRules, setAlarmRules] = useState<any[]>([]);

  useEffect(() => { loadRules(); }, []);

  function loadRules() {
    setLoadingRules(true);
    api.getAlarmRules().then(r => { setAlarmRules(r); setLoadingRules(false); })
      .catch(() => setLoadingRules(false));
  }

  function refresh() {
    invalidate('alarmHistory');
    loadRules();
  }

  function deleteRule(id: number) {
    api.deleteAlarmRule(id).then(() => {
      invalidate('alarmHistory');
      loadRules();
    }).catch(e => setError(extractError(e, t('smart.error.deleteRule'))));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>{t('smart.title')}</h1>
        <p className="subtitle">{t('smart.subtitle')}</p>
        <button className="refresh-btn" onClick={refresh}>{t('common.refresh')}</button>
      </div>

      {error && <div className="error-msg">{error}</div>}

      <div className="section">
        <h2>{t('smart.section.alarmRules')}</h2>
        <Spinner loading={loadingRules} />
        {!loadingRules && (
          alarmRules.length === 0 ? (
            <div className="empty-state"><p>{t('smart.empty.rules')}</p></div>
          ) : (
            <table className="data-table">
              <thead>
                <tr><th>{t('dashboard.alerts.attribute')}</th><th>{t('smart.col.operator')}</th><th>{t('smart.col.threshold')}</th><th>{t('dashboard.alerts.severity')}</th><th>{t('arrays.col.actions')}</th></tr>
              </thead>
              <tbody>
                {alarmRules.map((rule: any) => (
                  <tr key={rule.id}>
                    <td>{rule.attr_name}</td>
                    <td>{rule.operator}</td>
                    <td>{rule.threshold}</td>
                    <td><span className={`badge ${rule.severity}`}>{rule.severity}</span></td>
                    <td className="action-cell">
                      <button className="btn danger" onClick={() => deleteRule(rule.id)}>{t('common.delete')}</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )
        )}
      </div>

      <div className="section">
        <h2>{t('smart.section.alarmHistory')}</h2>
        {alarmHistory.length === 0 ? (
          <div className="empty-state"><p>{t('smart.empty.history')}</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>{t('dashboard.alerts.time')}</th><th>{t('dashboard.alerts.device')}</th><th>{t('dashboard.alerts.attribute')}</th><th>{t('dashboard.alerts.severity')}</th><th>{t('dashboard.alerts.value')}</th></tr>
            </thead>
            <tbody>
              {alarmHistory.map((event: any, i: number) => (
                <tr key={i} className={event.severity}>
                  <td>{event.timestamp}</td>
                  <td>{event.device_path}</td>
                  <td>{event.attr_name}</td>
                  <td>{event.severity}</td>
                  <td>{event.value}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
