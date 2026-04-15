import { useEffect, useState } from 'react';
import { usePreload } from '../../contexts/PreloadContext';
import { api } from '../../api/api';
import { extractError } from '../../utils/errors';
import Spinner from '../../components/Spinner/Spinner';

export default function Smart() {
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
    }).catch(e => setError(extractError(e, 'Failed to delete alarm rule')));
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>SMART</h1>
        <p className="subtitle">Drive health monitoring and alarm rules</p>
        <button className="refresh-btn" onClick={refresh}>Refresh</button>
      </div>

      {error && <div className="error-msg">{error}</div>}

      <div className="section">
        <h2>Alarm Rules</h2>
        <Spinner loading={loadingRules} />
        {!loadingRules && (
          alarmRules.length === 0 ? (
            <div className="empty-state"><p>No alarm rules configured.</p></div>
          ) : (
            <table className="data-table">
              <thead>
                <tr><th>Attribute</th><th>Operator</th><th>Threshold</th><th>Severity</th><th>Actions</th></tr>
              </thead>
              <tbody>
                {alarmRules.map((rule: any) => (
                  <tr key={rule.id}>
                    <td>{rule.attr_name}</td>
                    <td>{rule.operator}</td>
                    <td>{rule.threshold}</td>
                    <td><span className={`badge ${rule.severity}`}>{rule.severity}</span></td>
                    <td className="action-cell">
                      <button className="btn danger" onClick={() => deleteRule(rule.id)}>Delete</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )
        )}
      </div>

      <div className="section">
        <h2>Alarm History</h2>
        {alarmHistory.length === 0 ? (
          <div className="empty-state"><p>No alarm events recorded.</p></div>
        ) : (
          <table className="data-table">
            <thead>
              <tr><th>Time</th><th>Device</th><th>Attribute</th><th>Severity</th><th>Value</th></tr>
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
